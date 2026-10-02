package pg_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	agentpg "github.com/ManavA/keel/agent/pg"
)

func TestPurge(t *testing.T) {
	pool := newDatabase(t).pool(t)
	store := agentpg.New(pool)
	k := kitOver(t, store)

	// An old root run that ended, with a journal, an approval, a child that
	// ended and a grandchild that never did.
	old, oldLease := k.proposed()
	k.ask(oldLease, 2, nil)
	oldChild := k.createChild(old)
	oldGrandchild := k.createChild(oldChild)
	oldChildLease := k.claim(workerB, oldChild.ID)
	k.reply(oldChildLease)
	k.finish(oldChildLease, agent.StatusCompleted)
	k.finish(oldLease, agent.StatusFailed)

	// An old root run that is still in flight, however long ago it started.
	flying, flyingLease := k.proposed()
	// An old run that waits on a person.
	waiting, _ := k.parked(nil)
	// An old child that ended, under a parent that has not.
	keptChild := k.createChild(flying)
	k.finish(k.claim(workerB, keptChild.ID), agent.StatusCompleted)

	k.clock.Advance(time.Hour)
	cut := k.clock.Now()
	k.clock.Advance(time.Hour)

	// A root run that ended after the cut, and one that ended at it.
	newer, newerLease := k.proposed()
	k.finish(newerLease, agent.StatusCompleted)
	atCut, atCutLease := k.held()
	require.NoError(t, store.Finish(k.ctx, atCutLease, agent.FinishRequest{Status: agent.StatusCompleted, Now: cut}))

	count := func(table, column string, id string) int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(k.ctx, "select count(*) from "+table+" where "+column+" = $1", id).Scan(&n))
		return n
	}
	require.Equal(t, 2, count(agentpg.StepsTable, "run_id", old.ID))
	require.Equal(t, 1, count(agentpg.ApprovalsTable, "run_id", old.ID))

	// The keys tools recorded for their effects: one for a step of each run
	// that will go, one for a run that will stay, and two that are no
	// step's key.
	goes := []string{agent.StepKey(old.ID, 2), agent.StepKey(oldChild.ID, 1), agent.StepKey(oldGrandchild.ID, 14)}
	stays := []string{agent.StepKey(flying.ID, 2), agent.StepKey(keptChild.ID, 1), "another-tool:7", old.ID}
	effects := begin(t, pool)
	for _, key := range append(goes, stays...) {
		first, err := agentpg.Once(k.ctx, effects, key)
		require.NoError(t, err)
		require.True(t, first)
	}
	require.NoError(t, effects.Commit(k.ctx))

	removed, err := store.Purge(k.ctx, cut)

	require.NoError(t, err)
	assert.Equal(t, int64(1), removed, "the runs counted are the ones nobody started")
	for _, gone := range []agent.Run{old, oldChild, oldGrandchild} {
		_, err := store.GetRun(k.ctx, gone.ID)
		assert.ErrorIs(t, err, agent.ErrNotFound)
		assert.Zero(t, count(agentpg.StepsTable, "run_id", gone.ID), "its steps")
		assert.Zero(t, count(agentpg.ApprovalsTable, "run_id", gone.ID), "its approvals")
	}
	for name, kept := range map[string]agent.Run{
		"a run that ended after the cut": newer,
		"a run that ended at the cut":    atCut,
		"a run still in flight":          flying,
		"a run that waits":               waiting,
		"a child whose parent remains":   keptChild,
	} {
		_, err := store.GetRun(k.ctx, kept.ID)
		assert.NoError(t, err, name)
	}
	for _, key := range goes {
		assert.Zero(t, count(agentpg.EffectsTable, "key", key), "the key of a step that went: %s", key)
	}
	for _, key := range stays {
		assert.Equal(t, 1, count(agentpg.EffectsTable, "key", key), "a key that is no removed step's: %s", key)
	}
	assert.Equal(t, 2, count(agentpg.StepsTable, "run_id", flying.ID), "a kept run keeps its journal")
	assert.Equal(t, 1, count(agentpg.ApprovalsTable, "run_id", waiting.ID), "and its approvals")
	require.NoError(t, store.BeginModel(k.ctx, flyingLease, 3, k.tick()), "and can be carried on")

	// Asked again, there is nothing more to remove.
	removed, err = store.Purge(k.ctx, cut)
	require.NoError(t, err)
	assert.Zero(t, removed)

	// A later cut takes what has ended since, in every final status.
	k.finish(flyingLease, agent.StatusCancelled)
	removed, err = store.Purge(k.ctx, k.clock.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(3), removed, "the three root runs that have ended")
	left, err := store.ListRuns(k.ctx, agent.RunFilter{})
	require.NoError(t, err)
	require.Len(t, left, 1)
	assert.Equal(t, waiting.ID, left[0].ID)
}

// A purge takes the rows of the runs it will remove before it removes any,
// children before parents, which is the order a child's Finish takes its own
// row and then its parent's. Here a child is ending as its parent is purged:
// the child holds its own row and wants the parent's, and the purge wants
// both. Taken parent first, each would hold what the other waits for.
func TestPurge_AndAChildEndingTakeRowsInOneOrder(t *testing.T) {
	pool := newDatabase(t).pool(t, deadlockSettings...)
	store := agentpg.New(pool)
	k := kitOver(t, store)
	root, rootLease := k.held()
	child := k.createChild(root)
	childLease := k.claim(workerB, child.ID)
	k.finish(rootLease, agent.StatusFailed)
	k.clock.Advance(time.Hour)
	cut := k.clock.Now()

	// The child's Finish, stopped holding the child's row.
	holding := newGate(t)
	ending := agentpg.New(stepped{Beginner: pool, before: afterLock(holding)})
	ended := make(chan error, 1)
	now := k.tick()
	go func() {
		ended <- ending.Finish(k.ctx, childLease, agent.FinishRequest{Status: agent.StatusCancelled, Now: now})
	}()
	holding.arrived(t)

	type purge struct {
		removed int64
		err     error
	}
	purged := make(chan purge, 1)
	go func() {
		removed, err := store.Purge(k.ctx, cut)
		purged <- purge{removed, err}
	}()
	waits(t, pool, purged)

	holding.release()
	require.NoError(t, result(t, ended), "the child's end and the purge each held a row the other wanted")
	got := result(t, purged)
	require.NoError(t, got.err)
	assert.Equal(t, int64(1), got.removed)
	for _, gone := range []agent.Run{root, child} {
		_, err := store.GetRun(k.ctx, gone.ID)
		assert.ErrorIs(t, err, agent.ErrNotFound)
	}
}

// deadlock is what Postgres ends a transaction with when it and another each
// hold a lock the other waits for.
const deadlock = "40P01"

// A purge that Postgres ends for a deadlock is tried once more. Here the
// deadlock is real: another session holds a row of the run's journal and
// waits for the run's row, which the purge holds while it waits for the
// journal's. The other session does not look for a deadlock for an hour, so
// it is the purge that Postgres ends.
func TestPurge_IsTriedAgainAfterADeadlock(t *testing.T) {
	db := newDatabase(t)
	pool := db.pool(t, "deadlock_timeout=50ms", "statement_timeout=60s")
	patient := db.pool(t, "deadlock_timeout=1h")
	k := kitOver(t, agentpg.New(pool))
	run, lease := k.proposed()
	k.finish(lease, agent.StatusCompleted)
	k.clock.Advance(time.Hour)
	cut := k.clock.Now()

	// The other session, holding the run's journal.
	other := begin(t, patient)
	_, err := other.Exec(k.ctx, "select 1 from "+agentpg.StepsTable+" where run_id = $1 for update", run.ID)
	require.NoError(t, err)

	// The purge, stopped holding the run's row.
	var tries atomic.Int32
	holding := newGate(t)
	stop := afterLock(holding)
	purging := agentpg.New(stepped{Beginner: pool, before: func(st statement) {
		if st.n == 1 {
			tries.Add(1)
		}
		stop(st)
	}})
	type purge struct {
		removed int64
		err     error
	}
	purged := make(chan purge, 1)
	go func() {
		removed, err := purging.Purge(k.ctx, cut)
		purged <- purge{removed, err}
	}()
	holding.arrived(t)

	wanted := make(chan error, 1)
	go func() {
		_, err := other.Exec(k.ctx, "select 1 from "+agentpg.RunsTable+" where id = $1 for update", run.ID)
		wanted <- err
	}()
	waits(t, pool, wanted)

	// The purge goes on, to the journal the other session holds. Postgres
	// ends it, the other session is given the run's row, and the purge's
	// second try waits for that session to finish.
	holding.release()
	require.NoError(t, result(t, wanted), "the other session was the one Postgres ended")
	require.NoError(t, other.Commit(k.ctx))

	got := result(t, purged)
	require.NoError(t, got.err)
	assert.Equal(t, int64(1), got.removed)
	assert.Equal(t, int32(2), tries.Load(), "one try that Postgres ended, and one more")
	_, err = k.store.GetRun(k.ctx, run.ID)
	assert.ErrorIs(t, err, agent.ErrNotFound)
}

// It is tried once more and no more than that, and only for a deadlock.
func TestPurge_IsTriedAgainOnceAndOnlyForADeadlock(t *testing.T) {
	pool := newDatabase(t).pool(t)
	tests := []struct {
		name      string
		code      string
		wantTries int32
	}{
		{"a deadlock every time: two tries, and the error", deadlock, 2},
		{"another failure: one try", "57014", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tries atomic.Int32
			store := agentpg.New(stepped{Beginner: pool, failing: func(st statement) error {
				if st.n == 1 {
					tries.Add(1)
					return &pgconn.PgError{Code: tt.code, Message: "made to fail"}
				}
				return nil
			}})

			removed, err := store.Purge(t.Context(), testStart)

			var failure *pgconn.PgError
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, tt.code, failure.Code)
			assert.Zero(t, removed)
			assert.Equal(t, tt.wantTries, tries.Load())
		})
	}
}
