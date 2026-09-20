package retry_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/retry"
)

func TestBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	b := retry.NewBreaker(retry.BreakerOptions{
		FailureThreshold: 3,
		ResetTimeout:     time.Minute,
	})

	for i := 0; i < 3; i++ {
		err := b.Do(context.Background(), func() error {
			return errFail
		}, retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond})
		require.Error(t, err)
		require.NotErrorIs(t, err, retry.ErrOpen, "failing calls before the threshold must run fn, not short-circuit")
	}

	called := false
	err := b.Do(context.Background(), func() error {
		called = true
		return nil
	}, retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond})

	require.ErrorIs(t, err, retry.ErrOpen)
	assert.False(t, called, "an open breaker must not invoke fn")
}

func TestBreaker_SuccessResetsConsecutiveCount(t *testing.T) {
	b := retry.NewBreaker(retry.BreakerOptions{
		FailureThreshold: 3,
		ResetTimeout:     time.Minute,
	})
	fail := func() error { return errFail }
	ok := func() error { return nil }
	opts := retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond}

	require.Error(t, b.Do(context.Background(), fail, opts))
	require.Error(t, b.Do(context.Background(), fail, opts))
	require.NoError(t, b.Do(context.Background(), ok, opts))

	// Two more failures must not trip the breaker: the success reset the count.
	require.Error(t, b.Do(context.Background(), fail, opts))
	require.Error(t, b.Do(context.Background(), fail, opts))

	called := false
	require.NoError(t, b.Do(context.Background(), func() error {
		called = true
		return nil
	}, opts))
	assert.True(t, called, "two failures after a success must still run fn")
}

func TestBreaker_HalfOpenProbeSuccessCloses(t *testing.T) {
	b := retry.NewBreaker(retry.BreakerOptions{
		FailureThreshold: 2,
		ResetTimeout:     20 * time.Millisecond,
	})
	opts := retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond}

	require.Error(t, b.Do(context.Background(), func() error { return errFail }, opts))
	require.Error(t, b.Do(context.Background(), func() error { return errFail }, opts))

	time.Sleep(50 * time.Millisecond)

	require.NoError(t, b.Do(context.Background(), func() error { return nil }, opts))

	called := false
	require.NoError(t, b.Do(context.Background(), func() error {
		called = true
		return nil
	}, opts))
	assert.True(t, called, "a successful probe must close the breaker")
}

func TestBreaker_HalfOpenProbeFailureReopens(t *testing.T) {
	b := retry.NewBreaker(retry.BreakerOptions{
		FailureThreshold: 2,
		ResetTimeout:     20 * time.Millisecond,
	})
	opts := retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond}

	require.Error(t, b.Do(context.Background(), func() error { return errFail }, opts))
	require.Error(t, b.Do(context.Background(), func() error { return errFail }, opts))

	time.Sleep(50 * time.Millisecond)

	probeErr := b.Do(context.Background(), func() error { return errFail }, opts)
	require.Error(t, probeErr)
	require.NotErrorIs(t, probeErr, retry.ErrOpen, "the probe itself must run fn")

	called := false
	err := b.Do(context.Background(), func() error {
		called = true
		return nil
	}, opts)
	require.ErrorIs(t, err, retry.ErrOpen)
	assert.False(t, called, "a failed probe must leave the breaker open")
}

func TestBreaker_CountsOneCallNotOneAttempt(t *testing.T) {
	b := retry.NewBreaker(retry.BreakerOptions{
		FailureThreshold: 2,
		ResetTimeout:     time.Minute,
	})

	calls := 0
	err := b.Do(context.Background(), func() error {
		calls++
		return errFail
	}, retry.Options{MaxAttempts: 5, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond})
	require.Error(t, err)
	assert.Equal(t, 5, calls, "the inner backoff still runs its own attempts")

	// One failed call is one breaker failure: the next call must still run.
	called := false
	require.Error(t, b.Do(context.Background(), func() error {
		called = true
		return errFail
	}, retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond}))
	assert.True(t, called)
}
