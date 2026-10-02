package pg_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// The contract, case for case as agent.MemoryStore passes it.
//
// The suite asks for an empty store some three hundred times. A schema for
// each costs about fifty milliseconds, and emptying the tables of one schema
// costs under one, so the cases share a schema and every row is deleted
// between them. The suite runs its cases one at a time, so nothing else is
// using the tables when they are emptied.
func TestStore_PassesTheStoreSuite(t *testing.T) {
	pool := newDatabase(t).pool(t)
	store := agentpg.New(pool)

	agenttest.RunStoreSuite(t, func(t *testing.T) agent.Store {
		t.Helper()
		// Steps, approvals and child runs go with their run.
		_, err := pool.Exec(context.Background(),
			"delete from "+agentpg.RunsTable+"; delete from "+agentpg.EffectsTable)
		require.NoError(t, err)
		return store
	})
}
