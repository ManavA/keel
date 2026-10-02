package llm

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Price is what one model costs, in millionths of a US dollar per million
// tokens: $4 per million tokens is 4_000_000.
type Price struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Prices is a price table by model name.
type Prices map[string]Price

// Cost is what u costs on model, in millionths of a US dollar, rounded up.
// It returns ErrNoPrice for a model the table does not list.
func (p Prices) Cost(model string, u Usage) (int64, error) {
	price, ok := p[model]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrNoPrice, model)
	}
	total := u.InputTokens*price.Input + u.OutputTokens*price.Output +
		u.CacheReadTokens*price.CacheRead + u.CacheWriteTokens*price.CacheWrite
	return (total + 999_999) / 1_000_000, nil
}

// CostOf is the cost of a whole Response: each of its Attempts, or its one
// Model and Usage when it has none.
func (p Prices) CostOf(resp *Response) (int64, error) {
	if len(resp.Attempts) == 0 {
		return p.Cost(resp.Model, resp.Usage)
	}
	var total int64
	for _, a := range resp.Attempts {
		c, err := p.Cost(a.Model, a.Usage)
		if err != nil {
			return 0, err
		}
		total += c
	}
	return total, nil
}

// CostFor is the cost of a whole Response as Budgeted and Metered price it:
// each attempt at the model it names or, when the table does not list that
// one, at asked, the model the request asked for. A provider commonly answers
// an alias with a dated id the table does not carry, which CostOf would refuse.
//
// It returns an error wrapping ErrNoPrice when some attempt can be priced at
// neither name, and never the sum of the attempts that could be.
func (p Prices) CostFor(resp *Response, asked string) (int64, error) {
	if resp == nil {
		return 0, errors.New("llm: no reply to price")
	}
	if cost, ok := priceReply(p, resp, asked); ok {
		return cost, nil
	}
	var named []string
	for _, a := range attemptsOf(resp) {
		if q := fmt.Sprintf("%q", a.Model); !slices.Contains(named, q) {
			named = append(named, q)
		}
	}
	return 0, fmt.Errorf("%w: the reply is from %s and the request asked for %q",
		ErrNoPrice, strings.Join(named, ", "), asked)
}
