package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Defaults used by a zero-value [Options].
const (
	DefaultMaxAttempts = 5
	DefaultBaseDelay   = 100 * time.Millisecond
	DefaultMaxDelay    = 30 * time.Second
)

// Options configures [Do]. The zero value retries up to [DefaultMaxAttempts]
// times with [DefaultBaseDelay] and [DefaultMaxDelay], retrying every error.
type Options struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration

	// Retryable decides whether an error should be retried. Nil retries
	// everything.
	Retryable func(error) bool
}

// Error is what [Do] returns once it stops retrying: the error from the
// last attempt, and how many attempts were made.
type Error struct {
	Attempts int
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("retry: gave up after %d attempt(s): %v", e.Attempts, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Do calls fn until it succeeds, opts.Retryable rejects its error, ctx is
// cancelled, or opts.MaxAttempts is reached, sleeping with full jitter
// between attempts. See the package doc for the algorithm and the
// cancellation contract.
func Do(ctx context.Context, fn func() error, opts Options) error {
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	base := opts.BaseDelay
	if base <= 0 {
		base = DefaultBaseDelay
	}
	maxDelay := opts.MaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultMaxDelay
	}
	retryable := opts.Retryable
	if retryable == nil {
		retryable = func(error) bool { return true }
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return &Error{Attempts: attempt - 1, Err: err}
		}

		err := fn()
		if err == nil {
			return nil
		}
		if attempt == maxAttempts || !retryable(err) {
			return &Error{Attempts: attempt, Err: err}
		}

		delay := FullJitter(attempt, base, maxDelay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return &Error{Attempts: attempt, Err: ctx.Err()}
		case <-timer.C:
		}
	}

	// Unreachable: the loop above always returns by the time attempt
	// reaches maxAttempts.
	return &Error{Attempts: maxAttempts, Err: errors.New("retry: no attempt ran")}
}

// FullJitter returns a random duration in [0, cap), where cap is
// base*2^(attempt-1) clamped to max. attempt is 1-indexed: the delay before
// the retry following the first failed attempt uses attempt=1, so its cap
// starts at base rather than base*2.
func FullJitter(attempt int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = DefaultBaseDelay
	}
	if max <= 0 {
		max = DefaultMaxDelay
	}
	if attempt < 1 {
		attempt = 1
	}

	window := base
	for range attempt - 1 {
		if window >= max {
			window = max
			break
		}
		next := window * 2
		if next <= window { // overflowed time.Duration's int64
			window = max
			break
		}
		window = next
	}
	if window > max {
		window = max
	}
	if window <= 0 {
		return 0
	}

	return rand.N(window) //nolint:gosec // G404: backoff jitter has no security purpose
}
