package llm

import (
	"errors"
	"fmt"
	"time"
)

// ErrBudgetExceeded is returned by Budgeted for a call it refused.
var ErrBudgetExceeded = errors.New("llm: budget exceeded")

// ErrNoPrice is returned where a cost is needed for a model the price table
// does not list.
var ErrNoPrice = errors.New("llm: no price for model")

// ErrScriptExhausted is returned by Scripted when asked for a turn its script
// does not have.
var ErrScriptExhausted = errors.New("llm: script has no reply for this turn")

// Error is a failed call to a provider.
type Error struct {
	// Provider is the provider's name, "anthropic" or "openai".
	Provider string
	// Status is the HTTP status, or zero when no response arrived.
	Status int
	// Type is the provider's own error type, such as "rate_limit_error".
	Type      string
	Message   string
	RequestID string
	// RetryAfter is the wait the provider asked for, or zero.
	RetryAfter time.Duration
	// Retryable reports whether the same request may succeed later.
	Retryable bool
	// Err is the transport error when no response arrived.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("llm: %s: %v", e.Provider, e.Err)
	}
	return fmt.Sprintf("llm: %s: status %d %s: %s", e.Provider, e.Status, e.Type, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether err is a provider failure worth trying again.
//
// If err holds an *Error, its Retryable field decides, whatever the error it
// wraps says. A client's own timeout wraps context.DeadlineExceeded while the
// caller's context is still live, and is worth another try; a provider whose
// caller's context is done returns that context's error and no *Error. Any
// other error is not retryable: a bare context cancellation or deadline,
// ErrBudgetExceeded, and anything that is not from a provider.
func Retryable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Retryable
}
