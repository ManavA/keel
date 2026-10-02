package llm_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/llm"
)

func TestRetryable(t *testing.T) {
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
			// A transport failure is marked retryable by its provider, but
			// not when the transport failed because the caller gave up.
			name: "a retryable provider error whose transport error is a cancelled context",
			err:  &llm.Error{Provider: "openai", Retryable: true, Err: context.Canceled},
			want: false,
		},
		{
			name: "a retryable provider error whose transport error is an expired deadline",
			err:  &llm.Error{Provider: "openai", Retryable: true, Err: context.DeadlineExceeded},
			want: false,
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
