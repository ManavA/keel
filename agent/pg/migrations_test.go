package pg_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentpg "github.com/ManavA/keel/agent/pg"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
)

// The migration applies to an empty schema, applies again, and comes down and
// goes back up. Run alone only goes forward, and would not notice a down
// file that cannot run.
func TestMigrations_SurviveReplay(t *testing.T) {
	db := testdb.Shared(t)
	ctx := context.Background()
	const schema = "agent_replay"

	admin, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL, MaxConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+schema+" cascade")
		admin.Close()
	})
	_, err = admin.Exec(ctx, "drop schema if exists "+schema+" cascade")
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)

	pool, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL + "&search_path=" + schema})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = migrate.Replay(ctx, pool, migrate.ReplayOptions{Current: agentpg.MigrationsFS, CurrentDir: "migrations"})
	require.NoError(t, err)

	// After the replay the schema is up: the four tables the package names.
	for _, table := range []string{agentpg.RunsTable, agentpg.StepsTable, agentpg.ApprovalsTable, agentpg.EffectsTable} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			"select exists (select 1 from information_schema.tables where table_schema = $1 and table_name = $2)",
			schema, table).Scan(&exists))
		assert.True(t, exists, table)
	}
}
