package pg_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	authpg "github.com/ManavA/keel/auth/pg"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
)

// replaySchemaPool gives Replay its own schema on the shared test database,
// isolated from the one main_test.go's newTestPool already migrated — Replay
// tears its schema up and down repeatedly and must not do that to a schema
// other tests in this package are using at the same time.
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

// TestMigrationsSurviveReplay is the regression test for a down migration
// that fails to reverse cleanly: Replay applies every up migration, then
// every down migration in ascending order, then every up migration again.
// A DROP TABLE without CASCADE on a table three others reference by foreign
// key fails on that first down pass with Postgres error 2BP01, and nothing
// in Run alone would have caught it — Run only ever goes forward.
func TestMigrationsSurviveReplay(t *testing.T) {
	pool := replaySchemaPool(t)
	ctx := context.Background()

	_, err := migrate.Replay(ctx, pool, migrate.ReplayOptions{
		Current:    authpg.MigrationsFS,
		CurrentDir: "migrations",
	})
	require.NoError(t, err)
}
