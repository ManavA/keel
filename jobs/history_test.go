package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRunRecord_DerivesStatusAndExitCode(t *testing.T) {
	started := time.Now()
	finished := started.Add(time.Second)

	rec := NewRunRecord("job", started, finished, Outcome{Attempted: 2, Succeeded: 0, Failed: 2})
	assert.Equal(t, "job", rec.Name)
	assert.Equal(t, 2, rec.Attempted)
	assert.Equal(t, 2, rec.Failed)
	assert.Equal(t, StatusDidNothing, rec.Status)
	assert.Equal(t, 1, rec.ExitCode)
	assert.Equal(t, started, rec.StartedAt)
	assert.Equal(t, finished, rec.FinishedAt)
}

func TestMemoryHistoryStore_ListIsNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryHistoryStore()

	for _, o := range []Outcome{{Attempted: 1, Succeeded: 1}, {}} {
		require.NoError(t, s.Record(ctx, NewRunRecord("job", time.Now(), time.Now(), o)))
	}

	runs, err := s.List(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, StatusIdle, runs[0].Status, "the newest run must come first")
	assert.Equal(t, StatusSuccess, runs[1].Status)

	limited, err := s.List(ctx, "", 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)

	other, err := s.List(ctx, "no-such-entry", 10)
	require.NoError(t, err)
	assert.Empty(t, other)
}

func TestScheduler_Run_RecordsHistoryForEntriesWithAStore(t *testing.T) {
	ctx := context.Background()
	recorded := NewMemoryHistoryStore()
	plain := NewMemoryHistoryStore()

	s := NewScheduler(SchedulerOptions{})
	require.NoError(t, s.Register(Entry{
		Name:     "recorded",
		Interval: time.Hour,
		History:  recorded,
		Func: func(context.Context) (Outcome, error) {
			return Outcome{Attempted: 3, Succeeded: 3}, nil
		},
	}))
	require.NoError(t, s.Register(Entry{
		Name:     "unrecorded",
		Interval: time.Hour,
		Func: func(context.Context) (Outcome, error) {
			return Outcome{Attempted: 3, Succeeded: 3}, nil
		},
	}))

	runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	s.Run(runCtx)

	runs, err := recorded.List(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1, "the entry with a store must record its immediate run")
	assert.Equal(t, "recorded", runs[0].Name)
	assert.Equal(t, 3, runs[0].Attempted)
	assert.Equal(t, 3, runs[0].Succeeded)
	assert.Equal(t, StatusSuccess, runs[0].Status)
	assert.Equal(t, 0, runs[0].ExitCode)
	assert.False(t, runs[0].StartedAt.IsZero())
	assert.False(t, runs[0].FinishedAt.IsZero())

	empty, err := plain.List(ctx, "", 10)
	require.NoError(t, err)
	assert.Empty(t, empty, "a store no entry references must stay empty")

	unrecorded, err := recorded.List(ctx, "unrecorded", 10)
	require.NoError(t, err)
	assert.Empty(t, unrecorded, "the entry without a store must record nothing")
}

func TestScheduler_Run_RecordsFailedRuns(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryHistoryStore()

	s := NewScheduler(SchedulerOptions{})
	require.NoError(t, s.Register(Entry{
		Name:     "exploding",
		Interval: time.Hour,
		History:  store,
		Func: func(context.Context) (Outcome, error) {
			panic("entry exploded")
		},
	}))

	runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	s.Run(runCtx)

	runs, err := store.List(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1, "even a panicking run must leave a history row")
	assert.Equal(t, StatusFailed, runs[0].Status)
	assert.Equal(t, 1, runs[0].ExitCode)
}

func TestRunner_RunOutcome_ReturnsOutcomeAndExitCode(t *testing.T) {
	r := NewRunner(RunnerOptions{})
	outcome, exit := r.RunOutcome(context.Background(), "job", func(context.Context) (Outcome, error) {
		return Outcome{Attempted: 2, Succeeded: 1, Failed: 1}, nil
	})
	assert.Equal(t, 0, exit, "a partial run is OK, so it exits zero")
	assert.Equal(t, StatusPartial, outcome.Status())
}
