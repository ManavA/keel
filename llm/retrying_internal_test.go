package llm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/retry"
)

// The design states the defaults of a zero RetryOptions, and a test that waits
// for them would take seconds, so they are read off the options instead.
func TestRetryOptions_withDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   RetryOptions
		want RetryOptions
	}{
		{
			name: "the zero value: three attempts, half a second to thirty, a minute of Retry-After",
			want: RetryOptions{
				Retry:         retry.Options{MaxAttempts: 3, BaseDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second},
				MaxRetryAfter: time.Minute,
			},
		},
		{
			name: "what the caller sets is kept",
			in: RetryOptions{
				Retry:         retry.Options{MaxAttempts: 7, BaseDelay: time.Second, MaxDelay: 10 * time.Second},
				MaxRetryAfter: 5 * time.Second,
			},
			want: RetryOptions{
				Retry:         retry.Options{MaxAttempts: 7, BaseDelay: time.Second, MaxDelay: 10 * time.Second},
				MaxRetryAfter: 5 * time.Second,
			},
		},
		{
			name: "a negative value is read as unset",
			in:   RetryOptions{Retry: retry.Options{MaxAttempts: -1, BaseDelay: -1, MaxDelay: -1}, MaxRetryAfter: -1},
			want: RetryOptions{
				Retry:         retry.Options{MaxAttempts: 3, BaseDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second},
				MaxRetryAfter: time.Minute,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.withDefaults()

			assert.NotNil(t, got.Retry.Retryable, "Retryable is always set")
			got.Retry.Retryable = nil
			assert.Equal(t, tt.want, got)
		})
	}
}
