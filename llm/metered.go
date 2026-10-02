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
	// CostMicros is the call's cost, and Priced whether the table had a
	// price for every model that worked on it.
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
// across its Attempts. Record runs after the call, with no lock held, so calls
// that finish together may be recorded in either order.
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
	resp, err := m.model.Generate(ctx, req)
	m.account(ctx, req, start, resp, err)
	return resp, err
}

// Stream implements Model. The call is recorded when the stream ends, and
// its Duration covers all of it.
func (m *Metered) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error) {
	start := m.now()
	resp, err := m.model.Stream(ctx, req, fn)
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
		c.Usage = billed(resp)
		c.Stop = resp.Stop
		if cost, perr := m.prices.CostOf(resp); perr == nil {
			c.CostMicros, c.Priced = cost, true
		}
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
