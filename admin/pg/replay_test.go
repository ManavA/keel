package pg_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	adminpg "github.com/ManavA/keel/admin/pg"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
)

// replaySchemaPool gives Replay its own schema on the shared test database,
// isolated from the one main_test.go's newTestPool already migrated.
func replaySchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Shared(t)
	ctx := context.Background()

	schema := fmt.Sprintf("replay_%d", testCounter.next())
	admin, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL})
	require.NoError(t, err)
	defer admin.Close()

	_, err = admin.Exec(ctx, "drop schema if exists "+schema+" cascade")
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)

	pool, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL + "&search_path=" + schema})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// TestMigrationsSurviveReplay mirrors auth/pg's identical test: Run alone
// only ever goes forward and would never have caught a down migration that
// fails to reverse cleanly.
func TestMigrationsSurviveReplay(t *testing.T) {
	pool := replaySchemaPool(t)
	ctx := context.Background()

	_, err := migrate.Replay(ctx, pool, migrate.ReplayOptions{
		Current:    adminpg.MigrationsFS,
		CurrentDir: "migrations",
	})
	require.NoError(t, err)
}
