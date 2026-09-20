package retry

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Defaults for a zero-value [BreakerOptions].
const (
	DefaultBreakerFailureThreshold = 5
	DefaultBreakerResetTimeout     = 30 * time.Second
)

// ErrOpen is returned without invoking fn while the breaker is open.
var ErrOpen = errors.New("retry: circuit open")

// BreakerOptions configures [Breaker]. The zero value trips after
// [DefaultBreakerFailureThreshold] consecutive failures and allows a
// half-open probe after [DefaultBreakerResetTimeout].
type BreakerOptions struct {
	// FailureThreshold is how many consecutive [Breaker.Do] failures trip
	// the breaker open. Zero uses the default.
	FailureThreshold int
	// ResetTimeout is how long an open breaker waits before letting one
	// probe call through. Zero uses the default.
	ResetTimeout time.Duration
}

func (o BreakerOptions) withDefaults() BreakerOptions {
	if o.FailureThreshold <= 0 {
		o.FailureThreshold = DefaultBreakerFailureThreshold
	}
	if o.ResetTimeout <= 0 {
		o.ResetTimeout = DefaultBreakerResetTimeout
	}
	return o
}

const (
	breakerClosed = iota
	breakerOpen
	breakerHalfOpen
)

// Breaker counts consecutive [Breaker.Do] failures and short-circuits
// callers while the downstream is out. A Do failure is one failed call,
// not one failed attempt: the inner backoff in [Do] still runs to its own
// limit before the breaker counts it. A success resets the count and
// closes the breaker.
type Breaker struct {
	mu           sync.Mutex
	threshold    int
	resetTimeout time.Duration
	failures     int
	state        int
	openedAt     time.Time
}

// NewBreaker returns a closed breaker.
func NewBreaker(opts BreakerOptions) *Breaker {
	opts = opts.withDefaults()
	return &Breaker{
		threshold:    opts.FailureThreshold,
		resetTimeout: opts.ResetTimeout,
	}
}

// Do runs fn through [Do] unless the breaker is open, in which case it
// returns [ErrOpen] without invoking fn. After [BreakerOptions.ResetTimeout]
// an open breaker lets one probe call through: a success closes it and a
// failure reopens it, while other callers keep getting [ErrOpen] until the
// probe finishes.
func (b *Breaker) Do(ctx context.Context, fn func() error, opts Options) error {
	probe := false
	b.mu.Lock()
	if b.threshold <= 0 {
		b.threshold = DefaultBreakerFailureThreshold
	}
	if b.resetTimeout <= 0 {
		b.resetTimeout = DefaultBreakerResetTimeout
	}
	switch b.state {
	case breakerOpen:
		if time.Since(b.openedAt) < b.resetTimeout {
			b.mu.Unlock()
			return ErrOpen
		}
		b.state = breakerHalfOpen
		probe = true
	case breakerHalfOpen:
		b.mu.Unlock()
		return ErrOpen
	}
	b.mu.Unlock()

	err := Do(ctx, fn, opts)

	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.failures = 0
		b.state = breakerClosed
		return nil
	}
	if probe {
		b.state = breakerOpen
		b.openedAt = time.Now()
		return err
	}
	b.failures++
	if b.failures >= b.threshold {
		b.state = breakerOpen
		b.openedAt = time.Now()
	}
	return err
}
