package jobs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScheduler_Register_RejectsNonPositiveInterval(t *testing.T) {
	s := NewScheduler(SchedulerOptions{})
	err := s.Register(Entry{Name: "a", Interval: 0, Func: noop})
	require.Error(t, err)
}

func TestScheduler_Register_RejectsDuplicateName(t *testing.T) {
	s := NewScheduler(SchedulerOptions{})
	require.NoError(t, s.Register(Entry{Name: "a", Interval: time.Second, Func: noop}))
	err := s.Register(Entry{Name: "a", Interval: time.Second, Func: noop})
	require.Error(t, err, "two entries silently sharing a name would make their log lines indistinguishable")
}

func TestScheduler_Run_RunsEachEntryImmediatelyAndOnTick(t *testing.T) {
	var calls atomic.Int32
	s := NewScheduler(SchedulerOptions{})
	require.NoError(t, s.Register(Entry{
		Name:     "ticker",
		Interval: 15 * time.Millisecond,
		Func: func(ctx context.Context) (Outcome, error) {
			calls.Add(1)
			return Outcome{}, nil
		},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	// At least the immediate run plus one tick; loose bound to avoid a
	// flaky timing-exact assertion.
	assert.GreaterOrEqual(t, calls.Load(), int32(2))
}

func TestScheduler_Run_EntriesAreIndependent(t *testing.T) {
	var fast, slow atomic.Int32
	s := NewScheduler(SchedulerOptions{})
	require.NoError(t, s.Register(Entry{
		Name:     "fast",
		Interval: 10 * time.Millisecond,
		Func: func(ctx context.Context) (Outcome, error) {
			fast.Add(1)
			return Outcome{}, nil
		},
	}))
	require.NoError(t, s.Register(Entry{
		Name:     "slow",
		Interval: time.Hour,
		Func: func(ctx context.Context) (Outcome, error) {
			slow.Add(1)
			return Outcome{}, nil
		},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	assert.GreaterOrEqual(t, fast.Load(), int32(2), "the fast entry must tick multiple times")
	assert.Equal(t, int32(1), slow.Load(), "the slow entry must run its immediate tick and nothing more in this window")
}

func noop(ctx context.Context) (Outcome, error) { return Outcome{}, nil }
