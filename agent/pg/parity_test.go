package pg_test

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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

	t.Run("a cursor that names no run is a position all the same", func(t *testing.T) {
		for _, s := range stores {
			k := s.kit
			instants := []time.Time{k.tick(), k.tick(), k.tick()}
			var stored []agent.Run
			created := map[string]time.Time{}
			for _, at := range instants {
				for range 3 {
					run := k.newRun("paged")
					run.CreatedAt, run.UpdatedAt = at, at
					stored = append(stored, k.insert(run))
					created[run.ID] = at
				}
			}
			for _, id := range []string{
				// Below every id a run has, above every one, and among them.
				"00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff", newID(),
			} {
				for _, at := range []time.Time{instants[1], instants[1].Add(time.Microsecond), instants[0].Add(-time.Hour)} {
					// Newest first, then by id descending, and older than
					// the position.
					var want []string
					for _, run := range stored {
						if run.CreatedAt.Before(at) || run.CreatedAt.Equal(at) && run.ID < id {
							want = append(want, run.ID)
						}
					}
					slices.SortFunc(want, func(a, b string) int {
						return cmp.Or(created[b].Compare(created[a]), cmp.Compare(b, a))
					})

					runs, err := k.store.ListRuns(k.ctx, agent.RunFilter{Agent: "paged", Before: &agent.Cursor{CreatedAt: at, ID: id}})

					require.NoError(t, err, "%s: cursor %s at %s", s.name, id, at)
					var got []string
					for _, run := range runs {
						got = append(got, run.ID)
					}
					assert.Equal(t, want, got, "%s: cursor %s at %s", s.name, id, at)
				}
			}
		}
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

// A cursor is an argument and may have come from a client. One whose id is
// not a UUID in the one form is refused for what it is, with nothing asked
// of the database, so that what comes back is never the database's
// complaint about the id's form.
//
// The memory store has no such check: it compares a cursor's id as the
// string it is, and lists what sorts below it. The suite pins neither, so
// this is a place where the two stores differ.
func TestListRuns_ACursorWhoseIDIsNotAUUIDIsRefused(t *testing.T) {
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

		// The cursor is judged before the parent filter, which would
		// otherwise answer with nothing listed.
		_, err = store.ListRuns(t.Context(), agent.RunFilter{ParentID: "no-such-id", Before: &agent.Cursor{CreatedAt: at, ID: id}})
		require.Error(t, err, "cursor id %q, with a parent that is no UUID", id)
	}

	// What the memory store does with the same cursors: it compares the id
	// as the string it is. No id sorts below the empty one, so that cursor
	// lists only what is older than its time; every id sorts below one that
	// begins with a letter past f, so that cursor lists the runs of its own
	// instant as well.
	k := kitOver(t, agent.NewMemoryStore())
	older := k.create()
	later := k.clock.Now().Add(time.Hour)
	run := k.newRun(agentAlpha)
	run.CreatedAt, run.UpdatedAt = later, later
	k.insert(run)
	for id, want := range map[string][]string{"": {older.ID}, "no-such-id": {run.ID, older.ID}} {
		runs, err := k.store.ListRuns(k.ctx, agent.RunFilter{Before: &agent.Cursor{CreatedAt: later, ID: id}})
		require.NoError(t, err)
		var got []string
		for _, r := range runs {
			got = append(got, r.ID)
		}
		assert.Equal(t, want, got, "the memory store, given cursor id %q", id)
	}
}
