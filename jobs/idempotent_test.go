package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdempotent_RunsOnceForANewKey(t *testing.T) {
	g := NewMemoryGuard()
	calls := 0

	err := Idempotent(context.Background(), g, "job-a", "order-1", func(ctx context.Context) error {
		calls++
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestIdempotent_SkipsAnAlreadyDoneKey(t *testing.T) {
	g := NewMemoryGuard()
	calls := 0
	fn := func(ctx context.Context) error {
		calls++
		return nil
	}

	require.NoError(t, Idempotent(context.Background(), g, "job-a", "order-1", fn))
	require.NoError(t, Idempotent(context.Background(), g, "job-a", "order-1", fn))

	assert.Equal(t, 1, calls, "fn must not run a second time for a key already marked done")
}

func TestIdempotent_DifferentKeysRunIndependently(t *testing.T) {
	g := NewMemoryGuard()
	calls := 0
	fn := func(ctx context.Context) error {
		calls++
		return nil
	}

	require.NoError(t, Idempotent(context.Background(), g, "job-a", "order-1", fn))
	require.NoError(t, Idempotent(context.Background(), g, "job-a", "order-2", fn))

	assert.Equal(t, 2, calls)
}

func TestIdempotent_FnFailureDoesNotMarkDone(t *testing.T) {
	g := NewMemoryGuard()
	calls := 0

	err := Idempotent(context.Background(), g, "job-a", "order-1", func(ctx context.Context) error {
		calls++
		return errors.New("downstream refused")
	})
	require.Error(t, err)

	// A failed attempt must be retried, not silently accepted as done.
	err = Idempotent(context.Background(), g, "job-a", "order-1", func(ctx context.Context) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "a failed fn must not mark the key done, so the next attempt actually runs")
}

// failingGuard's Done always errors, to exercise the "unknown, not not-done"
// path: a guard the caller cannot even query must never be read as licence
// to run fn again.
type failingGuard struct{}

func (failingGuard) Done(context.Context, string) (bool, error) {
	return false, errors.New("store unreachable")
}
func (failingGuard) MarkDone(context.Context, string) error { return nil }

func TestIdempotent_UnreadableGuardRefusesToRun(t *testing.T) {
	calls := 0
	err := Idempotent(context.Background(), failingGuard{}, "job-a", "order-1", func(ctx context.Context) error {
		calls++
		return nil
	})
	require.Error(t, err)
	assert.Zero(t, calls, "fn must not run when the guard cannot say whether the key is already done")
}

// markFailsGuard succeeds fn but always fails MarkDone, to check the error
// this produces names the right side as already having happened.
type markFailsGuard struct{ done map[string]bool }

func (g *markFailsGuard) Done(_ context.Context, key string) (bool, error) {
	if g.done == nil {
		return false, nil
	}
	return g.done[key], nil
}
func (g *markFailsGuard) MarkDone(context.Context, string) error {
	return errors.New("ledger write failed")
}

func TestIdempotent_MarkDoneFailureIsReportedNotSwallowed(t *testing.T) {
	g := &markFailsGuard{}
	ranSideEffect := false

	err := Idempotent(context.Background(), g, "job-a", "order-1", func(ctx context.Context) error {
		ranSideEffect = true
		return nil
	})

	require.Error(t, err)
	assert.True(t, ranSideEffect, "the side effect ran; a bookkeeping failure must not be confused with fn failing")
	assert.Contains(t, err.Error(), "already ran")
}

// TestIdempotent_SameKeyDifferentJobsDoNotCollide is the regression test
// for a flat, unnamespaced key: two unrelated jobs that both reach for the
// same natural key (a calendar date, a batch id) must not have the second
// one's Done check silently see the first one's completion.
func TestIdempotent_SameKeyDifferentJobsDoNotCollide(t *testing.T) {
	g := NewMemoryGuard()
	var jobACalls, jobBCalls int

	require.NoError(t, Idempotent(context.Background(), g, "job-a", "2026-09-17", func(ctx context.Context) error {
		jobACalls++
		return nil
	}))
	require.NoError(t, Idempotent(context.Background(), g, "job-b", "2026-09-17", func(ctx context.Context) error {
		jobBCalls++
		return nil
	}))

	assert.Equal(t, 1, jobACalls)
	assert.Equal(t, 1, jobBCalls, "job-b must run even though job-a already completed the same natural key")
}

// TestIdempotent_KeysWithSeparatorBytesDoNotCollide is the regression test
// for the join scheme itself: job="a\x00b", key="c" and job="a",
// key="b\x00c" both produced "a\x00b\x00c" under a plain separator join,
// so the second pair's Done check saw the first pair's completion despite
// naming a different job entirely.
func TestIdempotent_KeysWithSeparatorBytesDoNotCollide(t *testing.T) {
	g := NewMemoryGuard()
	var calls int
	fn := func(ctx context.Context) error {
		calls++
		return nil
	}

	require.NoError(t, Idempotent(context.Background(), g, "a\x00b", "c", fn))
	require.NoError(t, Idempotent(context.Background(), g, "a", "b\x00c", fn))

	assert.Equal(t, 2, calls,
		"two different (job, key) pairs must not collide even when a value contains the byte a naive separator join would have used")
}

func TestIdempotent_EmptyJobNameIsRefused(t *testing.T) {
	g := NewMemoryGuard()
	calls := 0
	err := Idempotent(context.Background(), g, "", "key", func(ctx context.Context) error {
		calls++
		return nil
	})
	require.Error(t, err)
	assert.Zero(t, calls)
}

func TestNamespacedKey_NoCollisionsAcrossVariedInputs(t *testing.T) {
	tests := []struct{ job1, key1, job2, key2 string }{
		{"a\x00b", "c", "a", "b\x00c"},
		{"1", "23", "12", "3"},
		{"", "ab", "a", "b"},
		{"job", "key", "job2", "key"},
	}
	for _, tt := range tests {
		k1 := namespacedKey(tt.job1, tt.key1)
		k2 := namespacedKey(tt.job2, tt.key2)
		assert.NotEqual(t, k1, k2, "namespacedKey(%q,%q) collided with namespacedKey(%q,%q)", tt.job1, tt.key1, tt.job2, tt.key2)
	}
}

func TestMemoryGuard_DoneDefaultsFalse(t *testing.T) {
	g := NewMemoryGuard()
	done, err := g.Done(context.Background(), "never-seen")
	require.NoError(t, err)
	assert.False(t, done)
}

// TestIdempotent_TwoSeparateGuardsBothRunSameKey pins the single-process
// scope of Guard: two guards that share no store — two replicas — each find
// the same key not done and each run fn. The slow fn keeps the two runs
// overlapped the way two replicas on one schedule would be. Until a shared
// store exists this duplication is the behavior, not a test race.
func TestIdempotent_TwoSeparateGuardsBothRunSameKey(t *testing.T) {
	g1 := NewMemoryGuard()
	g2 := NewMemoryGuard()
	var calls atomic.Int32
	//nolint:unparam // both runs must succeed for the calls==2 pin; a failing fn tests another path.
	fn := func(context.Context) error {
		time.Sleep(50 * time.Millisecond)
		calls.Add(1)
		return nil
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, g := range []*MemoryGuard{g1, g2} {
		wg.Add(1)
		go func(i int, g *MemoryGuard) {
			defer wg.Done()
			<-start
			errs[i] = Idempotent(context.Background(), g, "job-a", "order-1", fn)
		}(i, g)
	}
	close(start)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, int32(2), calls.Load(),
		"two guards over separate stores each run fn for the same key; there is no shared lock")
}
