package pg_test

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	authpg "github.com/ManavA/keel/auth/pg"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
)

// testCounter gives each test a unique integer, for building emails and
// external ids that cannot collide across tests sharing one database.
var testCounter atomicCounter

type atomicCounter struct{ n atomic.Int64 }

func (c *atomicCounter) next() int64 { return c.n.Add(1) }

func TestMain(m *testing.M) {
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// newTestPool returns a pool against the shared test database with this
// package's migrations applied. migrate.Run is safe to call repeatedly —
// its ledger skips what is already applied — so every test calling this can
// share one schema.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Shared(t)
	ctx := context.Background()

	pool, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := migrate.Run(ctx, pool, migrate.Options{FS: authpg.MigrationsFS, Dir: "migrations"}); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return pool
}
