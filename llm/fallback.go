package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// moveOnByDefault is every error but a refused budget, which would meet the
// next model unchanged. A caller whose context has ended is not read from the
// error: the chain looks at the context itself, since an error that wraps a
// deadline, such as an HTTP client's own timeout, is a failed model like any
// other.
func moveOnByDefault(err error) bool {
	return !errors.Is(err, ErrBudgetExceeded)
}

// Generate implements Model. Request.Model goes to the first model only: a
// model name means something to one provider, so each later model uses its
// own configured default. The error of a failed chain wraps every model's.
func (f *Fallback) Generate(ctx context.Context, req Request) (*Response, error) {
	return f.run(ctx, req, &streamGuard{}, func(m Model, req Request, _ *streamGuard) (*Response, error) {
		return m.Generate(ctx, req)
	})
}

// Stream implements Model. It moves on only before the first delta: once the
// caller's callback has been called, the failure or the refusal is returned,
// since no other model can take the stream over without repeating itself. An
// error returned by fn itself is the caller's own signal to stop, and comes
// back as fn returned it.
func (f *Fallback) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error) {
	return f.run(ctx, req, &streamGuard{fn: fn}, func(m Model, req Request, g *streamGuard) (*Response, error) {
		return m.Stream(ctx, req, g.deliver)
	})
}

// run offers req to each model until one answers. g reports whether the
// caller has already been handed part of a reply, which ends the chain, and
// what the caller's callback returned.
//
// The caller's context ends the chain, and so does an error the options'
// ShouldFallback refuses to move past.
//
// A refusal that is moved past was billed, so its usage is kept and listed in
// the Attempts of the reply that follows. If no later model answers, the last
// refusal is the reply, with the earlier ones in its Attempts: it is a model's
// answer, the caller reads its Stop, and returning an error instead would
// leave what was paid for out of every account. The failures that followed it
// are logged.
func (f *Fallback) run(ctx context.Context, req Request, g *streamGuard, call func(Model, Request, *streamGuard) (*Response, error)) (*Response, error) {
	var (
		failures []error
		refused  refusals
	)
	for i, m := range f.models {
		if i > 0 {
			req.Model = ""
		}
		resp, err := requireReply(call(m, req, g))
		if err != nil {
			if cbErr := g.callbackErr(); cbErr != nil {
				return nil, cbErr
			}
		}
		last := i == len(f.models)-1
		stop := last || g.delivered.Load() || ctx.Err() != nil

		if err == nil {
			if !f.onRefusal || resp.Stop != StopRefusal {
				return withAttempts(resp, refused.attempts()), nil
			}
			refused.add(resp)
			if stop {
				return refused.reply(), nil
			}
			f.logger.Warn("llm: fallback: model refused, trying the next", "model", i+1, "of", len(f.models))
			continue
		}

		failures = append(failures, fmt.Errorf("model %d of %d: %w", i+1, len(f.models), err))
		if stop || !f.should(err) {
			if refused.last != nil {
				f.logger.Warn("llm: fallback: model failed, returning the earlier refusal", "model", i+1, "of", len(f.models), "error", err)
				return refused.reply(), nil
			}
			return nil, fmt.Errorf("llm: fallback: %w", errors.Join(failures...))
		}
		f.logger.Warn("llm: fallback: model failed, trying the next", "model", i+1, "of", len(f.models), "error", err)
	}
	// Unreachable: the last model's outcome always returns above.
	return nil, fmt.Errorf("llm: fallback: %w", errors.Join(failures...))
}

// refusals are the replies of models that declined and were moved past.
type refusals struct {
	// earlier holds the attempts of every refusal before the last.
	earlier []Attempt
	last    *Response
}

func (r *refusals) add(resp *Response) {
	if r.last != nil {
		r.earlier = append(r.earlier, attemptsOf(r.last)...)
	}
	r.last = resp
}

// attempts lists every billed attempt of every refusal, in order, or none.
func (r *refusals) attempts() []Attempt {
	if r.last == nil {
		return nil
	}
	return append(append([]Attempt(nil), r.earlier...), attemptsOf(r.last)...)
}

// reply is the last refusal, with the earlier ones listed in its Attempts.
func (r *refusals) reply() *Response { return withAttempts(r.last, r.earlier) }

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
