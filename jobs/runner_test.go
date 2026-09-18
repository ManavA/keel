package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunner_Run_ReturnsOutcomeExitCode(t *testing.T) {
	r := NewRunner(RunnerOptions{})
	exit := r.Run(context.Background(), "test-job", func(ctx context.Context) (Outcome, error) {
		return Outcome{Attempted: 3, Succeeded: 3}, nil
	})
	assert.Equal(t, 0, exit)
}

func TestRunner_Run_ErrorIsTreatedAsFatal(t *testing.T) {
	r := NewRunner(RunnerOptions{})
	exit := r.Run(context.Background(), "test-job", func(ctx context.Context) (Outcome, error) {
		return Outcome{Attempted: 3, Succeeded: 3}, errors.New("boom")
	})
	// Even though the counts alone would say "success", an error return
	// must win: the job could not even measure its own work.
	assert.Equal(t, 1, exit, "an error return must force a non-zero exit regardless of the counts")
}

func TestRunner_Run_TimeoutCancelsContext(t *testing.T) {
	r := NewRunner(RunnerOptions{Timeout: 20 * time.Millisecond})
	started := make(chan struct{})
	var sawDone bool

	r.Run(context.Background(), "slow-job", func(ctx context.Context) (Outcome, error) {
		close(started)
		select {
		case <-ctx.Done():
			sawDone = true
		case <-time.After(2 * time.Second):
		}
		return Outcome{Attempted: 1, Succeeded: 1}, nil
	})

	<-started
	require.True(t, sawDone, "Runner.Run must cancel the context passed to fn once Timeout elapses")
}

func TestRunner_Run_ZeroTimeoutAddsNoDeadline(t *testing.T) {
	r := NewRunner(RunnerOptions{})
	exit := r.Run(context.Background(), "job", func(ctx context.Context) (Outcome, error) {
		_, hasDeadline := ctx.Deadline()
		assert.False(t, hasDeadline, "Timeout zero must not add a deadline to the caller's context")
		return Outcome{}, nil
	})
	assert.Equal(t, 0, exit)
}
