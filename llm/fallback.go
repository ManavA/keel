package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
)

// FallbackOptions configures a Fallback. The zero value moves on after any
// error but a cancelled context or a refused budget.
type FallbackOptions struct {
	// ShouldFallback decides whether err moves on to the next model. Nil
	// uses the default above.
	ShouldFallback func(err error) bool
	// OnRefusal also moves on when a model answers with StopRefusal.
	OnRefusal bool
	Logger    *slog.Logger
}

// Fallback tries each Model in order until one answers.
type Fallback struct {
	models    []Model
	should    func(err error) bool
	onRefusal bool
	logger    *slog.Logger
}

var _ Model = (*Fallback)(nil)

// NewFallback builds a chain over models, which must not be empty.
func NewFallback(opts FallbackOptions, models ...Model) (*Fallback, error) {
	if len(models) == 0 {
		return nil, errors.New("llm: fallback needs at least one model")
	}
	for i, m := range models {
		if m == nil {
			return nil, fmt.Errorf("llm: fallback model %d is nil", i+1)
		}
	}
	f := &Fallback{
		models:    append([]Model(nil), models...),
		should:    opts.ShouldFallback,
		onRefusal: opts.OnRefusal,
		logger:    opts.Logger,
	}
	if f.should == nil {
		f.should = moveOnByDefault
	}
	if f.logger == nil {
		f.logger = slog.Default()
	}
	return f, nil
}

// moveOnByDefault is every error but a context that ended and a refused
// budget: those would meet the next model unchanged.
func moveOnByDefault(err error) bool {
	return !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, ErrBudgetExceeded)
}

// Generate implements Model. Request.Model goes to the first model only: a
// model name means something to one provider, so each later model uses its
// own configured default. The error of a failed chain wraps every model's.
func (f *Fallback) Generate(ctx context.Context, req Request) (*Response, error) {
	var never atomic.Bool
	return f.run(ctx, req, &never, func(m Model, req Request) (*Response, error) {
		return m.Generate(ctx, req)
	})
}

// Stream implements Model. It moves on only before the first delta: once the
// caller's callback has been called, the failure or the refusal is returned,
// since no other model can take the stream over without repeating itself.
func (f *Fallback) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error) {
	var delivered atomic.Bool
	return f.run(ctx, req, &delivered, func(m Model, req Request) (*Response, error) {
		return m.Stream(ctx, req, func(d Delta) error {
			delivered.Store(true)
			return fn(d)
		})
	})
}

// run offers req to each model until one answers. delivered reports whether
// the caller has already been handed part of a reply, which ends the chain.
//
// A refusal that is moved past was billed, so its usage is kept and listed in
// the Attempts of the reply that follows. When the last model refuses too, its
// refusal is the reply: it is a model's answer, and the caller reads its Stop.
func (f *Fallback) run(ctx context.Context, req Request, delivered *atomic.Bool, call func(Model, Request) (*Response, error)) (*Response, error) {
	var (
		failures []error
		refused  []Attempt
	)
	for i, m := range f.models {
		if i > 0 {
			req.Model = ""
		}
		resp, err := call(m, req)
		last := i == len(f.models)-1
		stop := last || delivered.Load() || ctx.Err() != nil
		refusal := err == nil && f.onRefusal && resp.Stop == StopRefusal

		if err == nil && (!refusal || stop) {
			return withAttempts(resp, refused), nil
		}
		if refusal {
			refused = append(refused, attemptsOf(resp)...)
			failures = append(failures, fmt.Errorf("model %d of %d: refused", i+1, len(f.models)))
			f.logger.Warn("llm: fallback: model refused, trying the next", "model", i+1, "of", len(f.models))
			continue
		}

		failures = append(failures, fmt.Errorf("model %d of %d: %w", i+1, len(f.models), err))
		if stop || !f.should(err) {
			return nil, fmt.Errorf("llm: fallback: %w", errors.Join(failures...))
		}
		f.logger.Warn("llm: fallback: model failed, trying the next", "model", i+1, "of", len(f.models), "error", err)
	}
	// Unreachable: the last model's outcome always returns above.
	return nil, fmt.Errorf("llm: fallback: %w", errors.Join(failures...))
}

// attemptsOf is every billed attempt behind resp: its Attempts, or its one
// Model and Usage.
func attemptsOf(resp *Response) []Attempt {
	if len(resp.Attempts) > 0 {
		return resp.Attempts
	}
	return []Attempt{{Model: resp.Model, Usage: resp.Usage}}
}

// withAttempts returns resp with the attempts of earlier models listed ahead
// of its own, so a price table sees every billed attempt. It leaves resp as
// the model returned it.
func withAttempts(resp *Response, earlier []Attempt) *Response {
	if len(earlier) == 0 {
		return resp
	}
	out := *resp
	out.Attempts = append(append([]Attempt(nil), earlier...), attemptsOf(resp)...)
	return &out
}
