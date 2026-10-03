package llm_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// A real client timeout, as the standard library builds it, is what a
// provider's transport error is made of, so the table uses one rather than a
// guess at its shape.
func TestHTTPClientTimeout_IsTheShapeThatFooledTheContextCheck(t *testing.T) {
	err := httpClientTimeout(t)

	var netErr net.Error
	require.ErrorAs(t, err, &netErr)
	assert.True(t, netErr.Timeout(), "a timeout")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "which reads as the caller's deadline to a check of the chain")
	assert.NotErrorIs(t, err, context.Canceled)
	var provider *llm.Error
	assert.NotErrorAs(t, err, &provider)
}

func TestRetryable(t *testing.T) {
	clientTimeout := httpClientTimeout(t)
	retryable := &llm.Error{Provider: "anthropic", Status: 529, Type: "overloaded_error", Retryable: true}
	permanent := &llm.Error{Provider: "anthropic", Status: 400, Type: "invalid_request_error"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "a retryable provider error", err: retryable, want: true},
		{name: "a provider error that is not retryable", err: permanent, want: false},
		{name: "a retryable provider error wrapped in fmt.Errorf", err: fmt.Errorf("generate: %w", retryable), want: true},
		{name: "a wrapped provider error that is not retryable", err: fmt.Errorf("generate: %w", permanent), want: false},
		{name: "context.Canceled", err: context.Canceled, want: false},
		{name: "context.DeadlineExceeded", err: context.DeadlineExceeded, want: false},
		{
			// The provider's own flag decides, whatever its transport error
			// wraps. A client's own timeout wraps the deadline error while the
			// caller's context is live, and is worth another try. A caller who
			// gave up is told by the provider with the context's own error and
			// no *Error, and that is not retryable (the rows above).
			name: "a retryable provider error whose transport error wraps a deadline",
			err:  &llm.Error{Provider: "openai", Retryable: true, Err: context.DeadlineExceeded},
			want: true,
		},
		{
			name: "a retryable provider error whose transport error wraps a cancel",
			err:  &llm.Error{Provider: "openai", Retryable: true, Err: context.Canceled},
			want: true,
		},
		{
			name: "a provider error that is not retryable, whose transport error wraps a deadline",
			err:  &llm.Error{Provider: "openai", Retryable: false, Err: context.DeadlineExceeded},
			want: false,
		},
		{
			name: "the timeout an http.Client raises, marked retryable by its provider",
			err:  &llm.Error{Provider: "openai", Retryable: true, Err: clientTimeout},
			want: true,
		},
		{
			name: "the same timeout wrapped again on the way up",
			err:  fmt.Errorf("generate: %w", &llm.Error{Provider: "openai", Retryable: true, Err: clientTimeout}),
			want: true,
		},
		{
			name: "the same timeout marked not retryable",
			err:  &llm.Error{Provider: "openai", Retryable: false, Err: clientTimeout},
			want: false,
		},
		{name: "the timeout an http.Client raises, with no provider error around it", err: clientTimeout, want: false},
		{name: "a deadline error wrapped", err: fmt.Errorf("slow: %w", context.DeadlineExceeded), want: false},
		{name: "a cancel wrapped", err: fmt.Errorf("stopped: %w", context.Canceled), want: false},
		{
			// An *Error in the chain decides even beside a context error.
			name: "a retryable provider error joined with a context error",
			err:  fmt.Errorf("%w; last error: %w", context.Canceled, retryable),
			want: true,
		},
		{name: "ErrBudgetExceeded", err: llm.ErrBudgetExceeded, want: false},
		{name: "ErrBudgetExceeded wrapped", err: fmt.Errorf("%w: 12 of 10 tokens", llm.ErrBudgetExceeded), want: false},
		{name: "ErrNoPrice", err: llm.ErrNoPrice, want: false},
		{name: "a plain error", err: errors.New("boom"), want: false},
		{name: "nil", err: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, llm.Retryable(tt.err))
		})
	}
}

func TestError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *llm.Error
		want string
	}{
		{
			name: "a response from the provider",
			err: &llm.Error{
				Provider: "anthropic", Status: 429, Type: "rate_limit_error",
				Message: "slow down", RequestID: "req_1", RetryAfter: 2 * time.Second, Retryable: true,
			},
			want: "llm: anthropic: status 429 rate_limit_error: slow down",
		},
		{
			name: "a type that is the status again is not written twice",
			err:  &llm.Error{Provider: "openai", Status: 403, Type: "403", Message: "forbidden"},
			want: "llm: openai: status 403: forbidden",
		},
		{
			name: "no type",
			err:  &llm.Error{Provider: "anthropic", Status: 502, Message: "Bad Gateway"},
			want: "llm: anthropic: status 502: Bad Gateway",
		},
		{
			name: "a transport error, with no response",
			err:  &llm.Error{Provider: "openai", Retryable: true, Err: errors.New("dial tcp: connection refused")},
			want: "llm: openai: dial tcp: connection refused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.err.Error())
		})
	}
}

func TestError_Unwrap(t *testing.T) {
	transport := errors.New("connection reset")
	err := fmt.Errorf("generate: %w", &llm.Error{Provider: "openai", Err: transport})

	assert.ErrorIs(t, err, transport)

	var provider *llm.Error
	assert.ErrorAs(t, err, &provider)
	assert.NoError(t, (&llm.Error{Provider: "openai", Status: 500}).Unwrap(), "a response has no transport error to unwrap")
}
