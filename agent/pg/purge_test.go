package pg_test

import (
	"testing"
	"time"

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
