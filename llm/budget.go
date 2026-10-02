package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// defaultMaxReplyTokens is the reply bound a request that sets none is
// assumed to have.
const defaultMaxReplyTokens = 16000

// BudgetOptions configures a Budgeted. A limit of zero is no limit.
type BudgetOptions struct {
	// MaxTokens bounds Usage.Total summed over every call.
	MaxTokens int64
	// MaxCostMicros bounds the cost summed over every call, in millionths of
	// a US dollar. It needs Prices.
	MaxCostMicros int64
	Prices        Prices
	// Model is the model a request that names none is priced as.
	Model string
	// DefaultMaxTokens is the reply bound assumed for a request that sets
	// none. Default 16000.
	DefaultMaxTokens int
}

// Spend is what a Budgeted has used.
type Spend struct {
	Calls      int64
	Tokens     int64
	CostMicros int64
}

// hold is what a call in flight has set aside: its worst case, tokens and
// cost, and the model that was priced for it.
type hold struct {
	tokens, cost int64
	model        string
}

// Budgeted refuses a call that could take a Model past a budget.
//
// Spend and the holds of calls in flight sit under one mutex, and a call is
// admitted or refused in a single step with its hold taken, so calls that
// start together cannot all be admitted against the same room. A negative
// limit is below any spend and refuses every call.
type Budgeted struct {
	model Model
	opts  BudgetOptions

	mu    sync.Mutex
	spent Spend
	held  hold
}

var _ Model = (*Budgeted)(nil)

// NewBudgeted wraps m. It returns an error when MaxCostMicros is set
// without Prices.
func NewBudgeted(m Model, opts BudgetOptions) (*Budgeted, error) {
	if opts.MaxCostMicros != 0 && len(opts.Prices) == 0 {
		return nil, errors.New("llm: a cost budget needs Prices")
	}
	if opts.DefaultMaxTokens <= 0 {
		opts.DefaultMaxTokens = defaultMaxReplyTokens
	}
	return &Budgeted{model: m, opts: opts}, nil
}

// Generate implements Model. A call it refuses returns an error wrapping
// ErrBudgetExceeded, or ErrNoPrice for a model a cost limit cannot price, and
// never reaches the inner model.
func (b *Budgeted) Generate(ctx context.Context, req Request) (*Response, error) {
	return b.call(req, func() (*Response, error) { return b.model.Generate(ctx, req) })
}

// Stream implements Model. The worst case is held for the whole stream, and a
// stream that fails, even after deltas, adds nothing.
func (b *Budgeted) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error) {
	g := &streamGuard{fn: fn}
	resp, err := b.call(req, func() (*Response, error) { return b.model.Stream(ctx, req, g.deliver) })
	if err != nil {
		if cbErr := g.callbackErr(); cbErr != nil {
			return nil, cbErr
		}
	}
	return resp, err
}

// Spent reports what has been used so far. Calls counts the calls that
// answered: a call that failed adds nothing, itself included, which is not
// what Metered.Totals counts.
func (b *Budgeted) Spent() Spend {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// call admits req, runs it and settles. The settle is deferred so that a model
// that panics gives its hold back.
func (b *Budgeted) call(req Request, run func() (*Response, error)) (*Response, error) {
	h, err := b.admit(req)
	if err != nil {
		return nil, err
	}
	var answered *Response
	defer func() { b.settle(h, answered) }()
	resp, err := requireReply(run())
	if err == nil {
		answered = resp
	}
	return resp, err
}

// admit prices req's worst case and, if it fits beside what is spent and what
// calls in flight hold, holds it.
func (b *Budgeted) admit(req Request) (hold, error) {
	model := req.Model
	if model == "" {
		model = b.opts.Model
	}
	maxOut := req.MaxTokens
	if maxOut <= 0 {
		maxOut = b.opts.DefaultMaxTokens
	}
	worst := Usage{InputTokens: EstimateInputTokens(req), OutputTokens: int64(maxOut)}
	h := hold{tokens: worst.Total(), model: model}
	if b.opts.MaxCostMicros != 0 {
		cost, err := b.opts.Prices.Cost(model, worst)
		if err != nil {
			return hold{}, fmt.Errorf("pricing the worst case: %w", err)
		}
		h.cost = cost
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if limit := b.opts.MaxTokens; limit != 0 && b.spent.Tokens+b.held.tokens+h.tokens > limit {
		return hold{}, fmt.Errorf("%w: a call of up to %d tokens would take the %d spent and %d in flight past the limit of %d",
			ErrBudgetExceeded, h.tokens, b.spent.Tokens, b.held.tokens, limit)
	}
	if limit := b.opts.MaxCostMicros; limit != 0 && b.spent.CostMicros+b.held.cost+h.cost > limit {
		return hold{}, fmt.Errorf("%w: a call costing up to %d would take the %d spent and %d in flight past the limit of %d (millionths of a dollar)",
			ErrBudgetExceeded, h.cost, b.spent.CostMicros, b.held.cost, limit)
	}
	b.held.tokens += h.tokens
	b.held.cost += h.cost
	return h, nil
}

// settle gives h back and, for a call that answered, adds what it used. A
// call that did not answer adds nothing: whether the provider billed it
// cannot be known.
//
// The real usage is priced by priceReply. Only a reply it cannot price is
// charged the cost that was held for it, which is the most the call was
// admitted at, so the budget errs toward refusing the next call rather than
// toward forgetting this one.
func (b *Budgeted) settle(h hold, resp *Response) {
	var tokens, cost int64
	if resp != nil {
		tokens = billed(resp).Total()
		cost = h.cost
		if c, ok := priceReply(b.opts.Prices, resp, h.model); ok {
			cost = c
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held.tokens -= h.tokens
	b.held.cost -= h.cost
	if resp != nil {
		b.spent.Calls++
		b.spent.Tokens += tokens
		b.spent.CostMicros += cost
	}
}

// priceReply is what resp cost. Each attempt behind it is priced at the model
// it names, or, when the table does not list that one, at asked, the model
// the request asked for: a provider commonly answers an alias with a dated id
// the table does not carry. ok is false when some attempt can be priced at
// neither, and with no table.
func priceReply(prices Prices, resp *Response, asked string) (cost int64, ok bool) {
	for _, a := range attemptsOf(resp) {
		c, err := prices.Cost(a.Model, a.Usage)
		if err != nil {
			c, err = prices.Cost(asked, a.Usage)
		}
		if err != nil {
			return 0, false
		}
		cost += c
	}
	return cost, true
}

// billed is the usage of every attempt behind resp.
func billed(resp *Response) Usage {
	var u Usage
	for _, a := range attemptsOf(resp) {
		u = u.Add(a.Usage)
	}
	return u
}
