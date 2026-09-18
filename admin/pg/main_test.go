package pg_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	adminpg "github.com/ManavA/keel/admin/pg"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Shared(t)
	ctx := context.Background()

	pool, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := migrate.Run(ctx, pool, migrate.Options{FS: adminpg.MigrationsFS, Dir: "migrations"}); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return pool
}

var testCounter atomicCounter

type atomicCounter struct{ n atomic.Int64 }

func (c *atomicCounter) next() int64 { return c.n.Add(1) }

func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d@example.com", t.Name(), testCounter.next())
}
