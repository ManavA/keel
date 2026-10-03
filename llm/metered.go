package llm

import (
	"cmp"
	"context"
	"sync"
	"time"
)

// CallRecord is the account of one call through a Metered.
type CallRecord struct {
	At       time.Time
	Duration time.Duration
	// Model is the model that answered, or the one asked for when the call
	// failed.
	Model string
	Usage Usage
	// CostMicros is the call's cost, and Priced whether every attempt was
	// priced: by its own model's name in the table or, failing that, at the
	// model the request asked for. A model that a fallback moves to should be
	// listed in the table, since otherwise it is priced at the first model's
	// rate.
	CostMicros int64
	Priced     bool
	Stop       StopReason
	Err        error
}

// MeterOptions configures a Metered. The zero value counts tokens and
// prices nothing.
type MeterOptions struct {
	Prices Prices
	// Record receives each CallRecord. Nil keeps only the totals.
	Record func(ctx context.Context, c CallRecord)
	// Now defaults to time.Now.
	Now func() time.Time
}

// Metered accounts for every call through a Model.
//
// A failed call has no usage and is not priced: its record carries the error,
// and its cost is zero. A reply that more than one model worked on is counted
// across its Attempts. A reply from a model the table does not list is priced
// at the model the request asked for, and Priced is false only when neither is
// listed. Record runs after the call, with no lock held, so calls that finish
// together may be recorded in either order.
type Metered struct {
	model  Model
	prices Prices
	record func(ctx context.Context, c CallRecord)
	now    func() time.Time

	mu     sync.Mutex
	totals Spend
}

var _ Model = (*Metered)(nil)

// NewMetered wraps m.
func NewMetered(m Model, opts MeterOptions) *Metered {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Metered{model: m, prices: opts.Prices, record: opts.Record, now: now}
}

// Generate implements Model.
func (m *Metered) Generate(ctx context.Context, req Request) (*Response, error) {
	start := m.now()
	resp, err := requireReply(m.model.Generate(ctx, req))
	m.account(ctx, req, start, resp, err)
	return resp, err
}

// Stream implements Model. The call is recorded when the stream ends, and
// its Duration covers all of it.
func (m *Metered) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error) {
	start := m.now()
	g := &streamGuard{fn: fn}
	resp, err := requireReply(m.model.Stream(ctx, req, g.deliver))
	if cbErr := g.callbackErr(); err != nil && cbErr != nil {
		err = cbErr
	}
	m.account(ctx, req, start, resp, err)
	return resp, err
}

// Totals reports what every call so far used. Calls counts failed calls too.
func (m *Metered) Totals() Spend {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totals
}

// account adds one finished call to the totals and hands its record on.
func (m *Metered) account(ctx context.Context, req Request, start time.Time, resp *Response, err error) {
	c := CallRecord{At: start, Duration: m.now().Sub(start), Model: req.Model, Err: err}
	if err == nil {
		c.Model = cmp.Or(resp.Model, req.Model)
		c.Usage = resp.BilledUsage()
		c.Stop = resp.Stop
		c.CostMicros, c.Priced = priceReply(m.prices, resp, req.Model)
	}

	m.mu.Lock()
	m.totals.Calls++
	m.totals.Tokens += c.Usage.Total()
	m.totals.CostMicros += c.CostMicros
	m.mu.Unlock()

	if m.record != nil {
		m.record(ctx, c)
	}
}
