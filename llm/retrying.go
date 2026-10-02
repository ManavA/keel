package llm

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ManavA/keel/retry"
)

// What a zero RetryOptions retries with. The delay between attempts is
// retry.Do's full jitter: a random wait below a ceiling that doubles from
// the base delay to retry.DefaultMaxDelay.
const (
	defaultRetryAttempts  = 3
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultMaxRetryAfter  = time.Minute
)

// RetryOptions configures a Retrying. The zero value retries a retryable
// failure up to three attempts.
type RetryOptions struct {
	// Retry is passed to retry.Do. Its Retryable is replaced by Retryable
	// from this package, and MaxAttempts defaults to 3.
	Retry retry.Options
	// MaxRetryAfter caps how long a provider's Retry-After is honoured.
	// Default 60 seconds.
	MaxRetryAfter time.Duration
}

func (o RetryOptions) withDefaults() RetryOptions {
	if o.Retry.MaxAttempts <= 0 {
		o.Retry.MaxAttempts = defaultRetryAttempts
	}
	if o.Retry.BaseDelay <= 0 {
		o.Retry.BaseDelay = defaultRetryBaseDelay
	}
	if o.Retry.MaxDelay <= 0 {
		o.Retry.MaxDelay = retry.DefaultMaxDelay
	}
	if o.MaxRetryAfter <= 0 {
		o.MaxRetryAfter = defaultMaxRetryAfter
	}
	o.Retry.Retryable = Retryable
	return o
}

// Retrying retries a Model's retryable failures with backoff.
type Retrying struct {
	model Model
	opts  RetryOptions
}

var _ Model = (*Retrying)(nil)

// NewRetrying wraps m.
func NewRetrying(m Model, opts RetryOptions) *Retrying {
	return &Retrying{model: m, opts: opts.withDefaults()}
}

// Generate implements Model. A failure is returned as retry.Do's
// *retry.Error, through which errors.As still finds an *Error.
func (r *Retrying) Generate(ctx context.Context, req Request) (*Response, error) {
	var resp *Response
	err := r.do(ctx, Retryable, func() (err error) {
		resp, err = r.model.Generate(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Stream implements Model. It retries only while fn has not been called: a
// stream that has delivered a delta is never replayed into the same callback,
// so its failure is returned as it is.
func (r *Retrying) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error) {
	var delivered atomic.Bool
	retryable := func(err error) bool { return !delivered.Load() && Retryable(err) }
	var resp *Response
	err := r.do(ctx, retryable, func() (err error) {
		resp, err = r.model.Stream(ctx, req, func(d Delta) error {
			delivered.Store(true)
			return fn(d)
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// do runs call under retry.Do. A failed attempt that retryable accepts and
// that carries a RetryAfter is held for it, capped, before the error goes
// back to retry.Do, which then adds its own jitter. The last attempt is not
// held, since nothing follows it.
func (r *Retrying) do(ctx context.Context, retryable func(error) bool, call func() error) error {
	opts := r.opts.Retry
	opts.Retryable = retryable
	attempt := 0
	return retry.Do(ctx, func() error {
		attempt++
		err := call()
		if err == nil || attempt >= opts.MaxAttempts || !retryable(err) {
			return err
		}
		var le *Error
		if !errors.As(err, &le) || le.RetryAfter <= 0 {
			return err
		}
		if werr := sleep(ctx, min(le.RetryAfter, r.opts.MaxRetryAfter)); werr != nil {
			return fmt.Errorf("llm: waiting out retry-after: %w (after %w)", werr, err)
		}
		return err
	}, opts)
}

// sleep waits for d, or returns the context's error if that ends first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
