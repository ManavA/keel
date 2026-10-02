package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// Where the suite is silent and a test might still tell the two stores apart,
// the Postgres store answers as the memory store does. Each case here runs
// against both and compares the answers.
func TestStore_AnswersAsTheMemoryStoreDoesWhereTheSuiteIsSilent(t *testing.T) {
	pool := newDatabase(t).pool(t)
	stores := []struct {
		name string
		kit  *kit
	}{
		{"memory", kitOver(t, agent.NewMemoryStore())},
		{"postgres", kitOver(t, agentpg.New(pool))},
	}

	t.Run("a listing or a change set with nothing in it", func(t *testing.T) {
		type emptiness struct{ runs, steps, approvals, changedSteps, changedApprovals bool }
		var got []emptiness
		for _, s := range stores {
			k := s.kit
			run := k.create()
			runs, err := k.store.ListRuns(k.ctx, agent.RunFilter{Agent: "gamma"})
			require.NoError(t, err)
			steps, err := k.store.Steps(k.ctx, run.ID)
			require.NoError(t, err)
			approvals, err := k.store.ListApprovals(k.ctx, agent.ApprovalFilter{RunID: run.ID})
			require.NoError(t, err)
			changes, err := k.store.Changes(k.ctx, run.ID, 0)
			require.NoError(t, err)
			require.Empty(t, runs)
			require.Empty(t, steps)
			require.Empty(t, approvals)
			require.Empty(t, changes.Steps)
			require.Empty(t, changes.Approvals)
			got = append(got, emptiness{runs == nil, steps == nil, approvals == nil, changes.Steps == nil, changes.Approvals == nil})
		}
		assert.Equal(t, got[0], got[1], "which empty lists are nil")
	})

	t.Run("a create whose key is taken, by a run that could not have been stored", func(t *testing.T) {
		for _, s := range stores {
			k := s.kit
			first := k.newRun("keyed")
			first.Key = "start-1"
			k.insert(first)

			// The id is in use and the parent does not exist, and neither
			// is looked at: the key is found first.
			again := k.newRun("keyed")
			again.Key, again.ID, again.ParentID = "start-1", first.ID, newID()
			stored, created, err := k.store.CreateRun(k.ctx, again)
			require.NoError(t, err, s.name)
			require.False(t, created, s.name)
			require.Equal(t, first.ID, stored.ID, s.name)

			// With no key to find: the id in use is what is wrong, before
			// the parent.
			again.Key = ""
			_, _, err = k.store.CreateRun(k.ctx, again)
			require.ErrorContains(t, err, "already in use", s.name)

			again.ID = newID()
			_, _, err = k.store.CreateRun(k.ctx, again)
			require.ErrorContains(t, err, "does not exist", s.name)
		}
	})
}

// neverBegun is a database that fails the test if a transaction is begun on
// it: what a store refuses for the request alone never reaches it.
type neverBegun struct{ t *testing.T }

func (n neverBegun) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	n.t.Error("the store opened a transaction for a request it should have refused first")
	return nil, errors.New("not reached")
}

// A cursor is an argument and may have come from a client. One the store
// refuses, it refuses with nothing asked of the database, so that what comes
// back is never the database's complaint about the id's form. The suite
// holds both stores to which cursors are refused; that no transaction is
// opened for one is this store's to show.
func TestListRuns_ACursorIsRefusedWithoutATransaction(t *testing.T) {
	store := agentpg.New(neverBegun{t})
	at := testStart
	canonical := newID()

	for _, id := range []string{"", "no-such-id", "0", strings.ToUpper(canonical), strings.ReplaceAll(canonical, "-", ""), canonical + " "} {
		runs, err := store.ListRuns(t.Context(), agent.RunFilter{Before: &agent.Cursor{CreatedAt: at, ID: id}})

		require.Error(t, err, "cursor id %q", id)
		assert.Nil(t, runs)
		assert.NotErrorIs(t, err, agent.ErrNotFound)
		var fromDatabase *pgconn.PgError
		assert.NotErrorAs(t, err, &fromDatabase)

		// The cursor is judged before the filters, which would otherwise
		// answer with nothing listed.
		_, err = store.ListRuns(t.Context(), agent.RunFilter{ParentID: "no-such-id", Before: &agent.Cursor{CreatedAt: at, ID: id}})
		require.Error(t, err, "cursor id %q, with a parent that is no UUID", id)
	}

	// Nor is one opened for a filter that names what no run could have.
	for _, f := range []agent.RunFilter{{ParentID: "no-such-id"}, {Agent: "a\x00b"}, {Status: "caf\xff"}} {
		runs, err := store.ListRuns(t.Context(), f)
		require.NoError(t, err)
		assert.Empty(t, runs)
	}
	approvals, err := store.ListApprovals(t.Context(), agent.ApprovalFilter{Status: "a\x00b"})
	require.NoError(t, err)
	assert.Empty(t, approvals)
}
