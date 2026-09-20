package pg_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/jobs"
	jobspg "github.com/ManavA/keel/jobs/pg"
	"github.com/ManavA/keel/log"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// openStore gives a test its own Store over the package's shared database,
// applying the migration first. The migration is idempotent (CREATE TABLE IF
// NOT EXISTS), so running it once per test is cheap and safe against rows
// other tests are also writing.
func openStore(t *testing.T) *jobspg.Store {
	t.Helper()
	db := testdb.Shared(t)

	pool, err := keelpg.Open(context.Background(), keelpg.Options{URL: db.URL, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	migration, err := os.ReadFile("migrations/001_job_runs.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), string(migration))
	require.NoError(t, err)

	return jobspg.New(pool)
}

// TestStore_SchedulerRunHistory is the acceptance test for the run-history
// store: two scheduled entries run against it, and the query API reports both
// runs with their counts and computed statuses.
//
// The entries tick hourly, so each runs exactly once — the immediate run —
// before the context below ends the scheduler.
func TestStore_SchedulerRunHistory(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	s := jobs.NewScheduler(jobs.SchedulerOptions{})
	require.NoError(t, s.Register(jobs.Entry{
		Name:     "nightly-sync",
		Interval: time.Hour,
		History:  store,
		Func: func(context.Context) (jobs.Outcome, error) {
			return jobs.Outcome{Attempted: 2, Succeeded: 2}, nil
		},
	}))
	require.NoError(t, s.Register(jobs.Entry{
		Name:     "stale-pruner",
		Interval: time.Hour,
		History:  store,
		Func: func(context.Context) (jobs.Outcome, error) {
			return jobs.Outcome{Attempted: 2, Failed: 2}, nil
		},
	}))

	runCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	s.Run(runCtx)

	runs, err := store.List(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, runs, 2, "one recorded run per scheduled entry")

	byName := map[string]jobs.RunRecord{}
	for _, r := range runs {
		byName[r.Name] = r
	}

	sync, ok := byName["nightly-sync"]
	require.True(t, ok, "the successful entry must have a recorded run")
	assert.Equal(t, 2, sync.Attempted)
	assert.Equal(t, 2, sync.Succeeded)
	assert.Equal(t, 0, sync.Failed)
	assert.Equal(t, jobs.StatusSuccess, sync.Status)
	assert.Equal(t, 0, sync.ExitCode)

	pruner, ok := byName["stale-pruner"]
	require.True(t, ok, "the did-nothing entry must have a recorded run")
	assert.Equal(t, 2, pruner.Attempted)
	assert.Equal(t, 0, pruner.Succeeded)
	assert.Equal(t, 2, pruner.Failed)
	assert.Equal(t, jobs.StatusDidNothing, pruner.Status)
	assert.Equal(t, 1, pruner.ExitCode)

	for _, r := range runs {
		assert.False(t, r.StartedAt.IsZero(), "run %q must record when it started", r.Name)
		assert.False(t, r.FinishedAt.IsZero(), "run %q must record when it finished", r.Name)
		assert.False(t, r.FinishedAt.Before(r.StartedAt), "run %q must not finish before it starts", r.Name)
	}
}

func TestStore_ListFiltersByNameAndLimit(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	prefix := t.Name() + "-"

	for _, name := range []string{prefix + "a", prefix + "b", prefix + "a"} {
		require.NoError(t, store.Record(ctx, jobs.NewRunRecord(name,
			time.Now(), time.Now(), jobs.Outcome{Attempted: 1, Succeeded: 1})))
	}

	named, err := store.List(ctx, prefix+"a", 10)
	require.NoError(t, err)
	assert.Len(t, named, 2, "a name filter must return only that entry's runs")
	for _, r := range named {
		assert.Equal(t, prefix+"a", r.Name)
	}

	limited, err := store.List(ctx, "", 2)
	require.NoError(t, err)
	assert.Len(t, limited, 2, "the limit must bound the rows returned")
}
