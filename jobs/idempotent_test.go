package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdempotent_RunsOnceForANewKey(t *testing.T) {
	g := NewMemoryGuard()
	calls := 0

	err := Idempotent(context.Background(), g, "order-1", func(ctx context.Context) error {
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

	require.NoError(t, Idempotent(context.Background(), g, "order-1", fn))
	require.NoError(t, Idempotent(context.Background(), g, "order-1", fn))

	assert.Equal(t, 1, calls, "fn must not run a second time for a key already marked done")
}

func TestIdempotent_DifferentKeysRunIndependently(t *testing.T) {
	g := NewMemoryGuard()
	calls := 0
	fn := func(ctx context.Context) error {
		calls++
		return nil
	}

	require.NoError(t, Idempotent(context.Background(), g, "order-1", fn))
	require.NoError(t, Idempotent(context.Background(), g, "order-2", fn))

	assert.Equal(t, 2, calls)
}

func TestIdempotent_FnFailureDoesNotMarkDone(t *testing.T) {
	g := NewMemoryGuard()
	calls := 0

	err := Idempotent(context.Background(), g, "order-1", func(ctx context.Context) error {
		calls++
		return errors.New("downstream refused")
	})
	require.Error(t, err)

	// A failed attempt must be retried, not silently accepted as done.
	err = Idempotent(context.Background(), g, "order-1", func(ctx context.Context) error {
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
	err := Idempotent(context.Background(), failingGuard{}, "order-1", func(ctx context.Context) error {
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

	err := Idempotent(context.Background(), g, "order-1", func(ctx context.Context) error {
		ranSideEffect = true
		return nil
	})

	require.Error(t, err)
	assert.True(t, ranSideEffect, "the side effect ran; a bookkeeping failure must not be confused with fn failing")
	assert.Contains(t, err.Error(), "already ran")
}

func TestMemoryGuard_DoneDefaultsFalse(t *testing.T) {
	g := NewMemoryGuard()
	done, err := g.Done(context.Background(), "never-seen")
	require.NoError(t, err)
	assert.False(t, done)
}
