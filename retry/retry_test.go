package retry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/retry"
)

var errFail = errors.New("boom")

func TestDo_SucceedsWithoutRetry(t *testing.T) {
	calls := 0
	err := retry.Do(context.Background(), func() error {
		calls++
		return nil
	}, retry.Options{BaseDelay: time.Microsecond})

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestDo_RetriesUntilSuccess(t *testing.T) {
	calls := 0
	err := retry.Do(context.Background(), func() error {
		calls++
		if calls < 3 {
			return errFail
		}
		return nil
	}, retry.Options{MaxAttempts: 5, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond})

	require.NoError(t, err)
	assert.Equal(t, 3, calls)
}

func TestDo_ExhaustsMaxAttempts(t *testing.T) {
	calls := 0
	err := retry.Do(context.Background(), func() error {
		calls++
		return errFail
	}, retry.Options{MaxAttempts: 4, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond})

	require.Error(t, err)
	assert.Equal(t, 4, calls)

	var retryErr *retry.Error
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, 4, retryErr.Attempts)
	assert.ErrorIs(t, err, errFail)
}

func TestDo_NonRetryableErrorStopsImmediately(t *testing.T) {
	calls := 0
	err := retry.Do(context.Background(), func() error {
		calls++
		return errFail
	}, retry.Options{
		MaxAttempts: 10,
		BaseDelay:   time.Microsecond,
		MaxDelay:    time.Millisecond,
		Retryable:   func(error) bool { return false },
	})

	require.Error(t, err)
	assert.Equal(t, 1, calls, "a non-retryable error must not be retried")

	var retryErr *retry.Error
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, 1, retryErr.Attempts)
}

func TestDo_RetryableSeesEachError(t *testing.T) {
	errA := errors.New("a")
	errB := errors.New("b")
	seen := []error{}
	calls := 0

	err := retry.Do(context.Background(), func() error {
		calls++
		if calls == 1 {
			return errA
		}
		return errB
	}, retry.Options{
		MaxAttempts: 2,
		BaseDelay:   time.Microsecond,
		MaxDelay:    time.Millisecond,
		Retryable: func(err error) bool {
			seen = append(seen, err)
			return true
		},
	})

	require.Error(t, err)
	assert.Equal(t, []error{errA}, seen, "Retryable is only consulted before a retry, not on the final attempt")
	assert.ErrorIs(t, err, errB)
}

func TestDo_ZeroOptionsAreUsable(t *testing.T) {
	calls := 0
	err := retry.Do(context.Background(), func() error {
		calls++
		return nil
	}, retry.Options{})

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestDo_AlreadyCancelledContextRunsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	err := retry.Do(ctx, func() error {
		calls++
		return nil
	}, retry.Options{})

	require.Error(t, err)
	assert.Equal(t, 0, calls)

	var retryErr *retry.Error
	require.ErrorAs(t, err, &retryErr)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestDo_CancellationStopsBeforeNextAttempt asserts that cancelling ctx
// during the backoff sleep returns Do well before the sleep would have
// elapsed on its own, rather than only before the attempt after that.
func TestDo_CancellationStopsBeforeNextAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const longDelay = 5 * time.Second
	calls := 0
	start := time.Now()

	done := make(chan error, 1)
	go func() {
		done <- retry.Do(ctx, func() error {
			calls++
			return errFail
		}, retry.Options{
			MaxAttempts: 10,
			BaseDelay:   longDelay,
			MaxDelay:    longDelay,
		})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		// Tight bound: the sleep this cancels is longDelay (5s) and cancel
		// fires ~50ms in, so a cancellable timer returns within a few tens
		// of milliseconds of that — 250ms leaves generous scheduling slack.
		// A mutant that swaps the cancellable timer for a plain
		// time.Sleep(delay) draws delay uniformly from [0, 5s) and only
		// slips under this bound on a small fraction of runs, instead of
		// the roughly 2-in-5 chance the previous 2s bound left it.
		assert.Less(t, elapsed, 250*time.Millisecond)
		assert.Equal(t, 1, calls, "only the attempt already in flight should have run")
	case <-time.After(time.Second):
		t.Fatal("Do did not return within 1s of the context being cancelled")
	}
}

func TestFullJitter_NeverExceedsCap(t *testing.T) {
	const base = 10 * time.Millisecond
	const max = 200 * time.Millisecond

	for attempt := 1; attempt <= 20; attempt++ {
		for i := 0; i < 200; i++ {
			d := retry.FullJitter(attempt, base, max)
			require.GreaterOrEqual(t, d, time.Duration(0))
			require.Less(t, d, max+1)
		}
	}
}

func TestFullJitter_CapsAtMaxDelay(t *testing.T) {
	const base = time.Second
	const max = 3 * time.Second

	// attempt=10 would want base*2^9 without a cap; the cap must win.
	for i := 0; i < 500; i++ {
		d := retry.FullJitter(10, base, max)
		require.Less(t, d, max+1)
	}
}

// TestFullJitter_Distribution samples many draws at a fixed attempt and
// checks the shape a uniform [0, cap) distribution must have: the low and
// high ends of the range both get hit, the mean sits near cap/2, and no
// tenth of the range is starved. A generous tolerance keeps this from being
// flaky while still failing if the implementation stops being uniform (for
// example, if it returned a fixed offset instead of jitter, or only ever
// jittered near cap).
func TestFullJitter_Distribution(t *testing.T) {
	const base = 100 * time.Millisecond
	const max = time.Hour // effectively uncapped at this attempt
	const attempt = 5     // cap = base * 2^4 = 1.6s
	const n = 20000
	capWindow := base * 16

	var sum time.Duration
	minSeen, maxSeen := time.Duration(1<<62), time.Duration(0)
	buckets := make([]int, 10)

	for i := 0; i < n; i++ {
		d := retry.FullJitter(attempt, base, max)
		require.GreaterOrEqual(t, d, time.Duration(0))
		require.Less(t, d, capWindow)

		sum += d
		if d < minSeen {
			minSeen = d
		}
		if d > maxSeen {
			maxSeen = d
		}
		bucket := int(float64(d) / float64(capWindow) * 10)
		if bucket > 9 {
			bucket = 9
		}
		buckets[bucket]++
	}

	mean := sum / n
	assert.InDelta(t, float64(capWindow/2), float64(mean), float64(capWindow)*0.1,
		"mean of a uniform [0, cap) distribution should sit near cap/2")
	assert.Less(t, minSeen, capWindow/10, "20000 draws should reach near the bottom of the range")
	assert.Greater(t, maxSeen, capWindow*85/100, "20000 draws should reach near the top of the range")

	for i, count := range buckets {
		assert.Greater(t, count, n/10/2,
			"bucket %d of 10 is far under its expected share; distribution is not roughly uniform", i)
	}
}
