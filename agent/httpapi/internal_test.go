package httpapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRuns is a Runs that is never called.
type stubRuns struct{ Runs }

func TestNew_Defaults(t *testing.T) {
	tests := []struct {
		name       string
		opts       Options
		poll, beat time.Duration
	}{
		{"the zero value", Options{}, 500 * time.Millisecond, 15 * time.Second},
		{"set", Options{PollInterval: time.Second, Heartbeat: 2 * time.Second}, time.Second, 2 * time.Second},
		// A negative interval would make a timer that is always ready, and
		// the stream would read the store in a loop.
		{"negative", Options{PollInterval: -1, Heartbeat: -1}, 500 * time.Millisecond, 15 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Runs = stubRuns{}
			api, err := New(tt.opts)
			require.NoError(t, err)
			assert.Equal(t, tt.poll, api.poll)
			assert.Equal(t, tt.beat, api.heartbeat)
		})
	}
}
