package pg_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/log"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// openPool gives a test its own pool and its own table namespace.
func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Shared(t)

	pool, err := pg.Open(context.Background(), pg.Options{URL: db.URL, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestOpen(t *testing.T) {
	pool := openPool(t)

	var answer int
	require.NoError(t, pool.QueryRow(context.Background(), "select 7").Scan(&answer))
	assert.Equal(t, 7, answer)
}

func TestOpenFailsOnAnUnreachableDatabase(t *testing.T) {
	// pgxpool.New on its own returns a usable pool having contacted nothing, so
	// without the verifying query this would succeed and the service would
	// start.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := pg.Open(ctx, pg.Options{
		URL:            "postgres://nobody:nobody@127.0.0.1:1/nothing?sslmode=disable",
		ConnectTimeout: 2 * time.Second,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unreachable")
}

func TestOpenRejectsAMalformedURL(t *testing.T) {
	_, err := pg.Open(context.Background(), pg.Options{URL: "://not a url"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse connection string")
}

func TestHealthCheck(t *testing.T) {
	pool := openPool(t)

	check := pg.HealthCheck(pool)
	require.NoError(t, check(context.Background()))

	assert.Error(t, pg.HealthCheck(nil)(context.Background()))

	pool.Close()
	assert.Error(t, check(context.Background()), "a closed pool must not report healthy")
}

// scratchTable makes a table only this test uses, since every test shares one
// database.
func scratchTable(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	name := fmt.Sprintf("scratch_%d", time.Now().UnixNano())
	_, err := pool.Exec(context.Background(),
		fmt.Sprintf("create table %s (id int primary key)", name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "drop table if exists "+name)
	})
	return name
}

func TestInTxCommits(t *testing.T) {
	pool := openPool(t)
	table := scratchTable(t, pool)
	ctx := context.Background()

	require.NoError(t, pg.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into "+table+" (id) values (1), (2)")
		return err
	}))

	assert.Equal(t, 2, count(t, pool, table))
}

func TestInTxRollsBackOnError(t *testing.T) {
	pool := openPool(t)
	table := scratchTable(t, pool)
	ctx := context.Background()

	want := errors.New("changed my mind")
	err := pg.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "insert into "+table+" (id) values (1)"); err != nil {
			return err
		}
		return want
	})

	require.ErrorIs(t, err, want, "fn's error must reach the caller unchanged")
	assert.Zero(t, count(t, pool, table))
}

func TestInTxRollsBackOnPanicAndRepanics(t *testing.T) {
	pool := openPool(t)
	table := scratchTable(t, pool)
	ctx := context.Background()

	assert.PanicsWithValue(t, "boom", func() {
		_ = pg.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "insert into "+table+" (id) values (1)")
			require.NoError(t, err)
			panic("boom")
		})
	})

	assert.Zero(t, count(t, pool, table), "a panic must not leave the transaction open")
}

func TestInTxReportsAFailedStatement(t *testing.T) {
	pool := openPool(t)
	table := scratchTable(t, pool)
	ctx := context.Background()

	err := pg.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "insert into "+table+" (id) values (1)"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "insert into "+table+" (id) values (1)") // duplicate key
		return err
	})

	require.Error(t, err)
	assert.Zero(t, count(t, pool, table))
}

func TestInTxDoesNotReportAnErrorAfterACleanCommit(t *testing.T) {
	// A deferred Rollback after Commit returns pgx.ErrTxClosed. Reporting it
	// turns every successful transaction into a failure.
	pool := openPool(t)
	table := scratchTable(t, pool)
	ctx := context.Background()

	for i := range 5 {
		require.NoError(t, pg.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "insert into "+table+" (id) values ($1)", i)
			return err
		}))
	}
	assert.Equal(t, 5, count(t, pool, table))
}

func TestInTxOptionsReadOnly(t *testing.T) {
	pool := openPool(t)
	table := scratchTable(t, pool)
	ctx := context.Background()

	err := pg.InTxOptions(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into "+table+" (id) values (1)")
		return err
	})
	require.Error(t, err)
	assert.Zero(t, count(t, pool, table))
}

func TestInTxRollsBackWhenTheContextIsCancelled(t *testing.T) {
	pool := openPool(t)
	table := scratchTable(t, pool)

	ctx, cancel := context.WithCancel(context.Background())
	err := pg.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "insert into "+table+" (id) values (1)"); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})

	require.Error(t, err)
	assert.Zero(t, count(t, pool, table),
		"the rollback must not itself be cancelled by the context that failed")
}

func TestInTxReportsABeginFailure(t *testing.T) {
	pool := openPool(t)
	pool.Close()

	err := pg.InTx(context.Background(), pool, func(pgx.Tx) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "begin transaction")
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), "select count(*) from "+table).Scan(&n))
	return n
}

// failingRollbackTx makes Rollback fail so that InTx's joining of the two
// errors can be observed. There is no way to provoke it from a real
// transaction that is otherwise behaving.
type failingRollbackTx struct {
	pgx.Tx
	err error
}

// The real rollback still runs, so the connection goes back to the pool; only
// the reported outcome is a failure.
func (t failingRollbackTx) Rollback(ctx context.Context) error {
	_ = t.Tx.Rollback(ctx)
	return t.err
}

type failingRollbackPool struct {
	*pgxpool.Pool
	err error
}

func (p failingRollbackPool) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.Pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return failingRollbackTx{Tx: tx, err: p.err}, nil
}

func TestInTxJoinsARollbackFailureToTheOriginalError(t *testing.T) {
	pool := openPool(t)

	rollbackErr := errors.New("rollback exploded")
	fnErr := errors.New("the real reason")

	err := pg.InTx(context.Background(), failingRollbackPool{Pool: pool, err: rollbackErr},
		func(pgx.Tx) error { return fnErr })

	require.Error(t, err)
	assert.ErrorIs(t, err, fnErr, "fn's error is what the caller needs and must survive")
	assert.ErrorIs(t, err, rollbackErr, "the rollback failure is real and must not be dropped")
}

func TestInTxDoesNotRollBackAfterACommit(t *testing.T) {
	// A rollback after a successful commit returns pgx.ErrTxClosed, and
	// reporting it turns every successful transaction into a failure.
	pool := openPool(t)
	table := scratchTable(t, pool)

	err := pg.InTx(context.Background(), failingRollbackPool{Pool: pool, err: errors.New("must not be called")},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), "insert into "+table+" (id) values (1)")
			return err
		})
	require.NoError(t, err)
	assert.Equal(t, 1, count(t, pool, table))
}

func TestOpenKeepsPoolSettingsFromTheURL(t *testing.T) {
	// Assigning the defaults unconditionally overwrites what the connection
	// string set, so a deployment tuning these through the environment sees no
	// effect and nothing says why.
	db := testdb.Shared(t)
	url := db.URL +
		"&pool_max_conns=7" +
		"&pool_max_conn_lifetime=15m" +
		"&pool_max_conn_idle_time=2m" +
		"&connect_timeout=25"

	pool, err := pg.Open(context.Background(), pg.Options{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	cfg := pool.Config()
	assert.EqualValues(t, 7, cfg.MaxConns)
	assert.Equal(t, 15*time.Minute, cfg.MaxConnLifetime)
	assert.Equal(t, 2*time.Minute, cfg.MaxConnIdleTime)
	assert.Equal(t, 25*time.Second, cfg.ConnConfig.ConnectTimeout)
}

func TestOpenOptionsOverrideTheURL(t *testing.T) {
	db := testdb.Shared(t)
	url := db.URL + "&pool_max_conn_lifetime=15m&pool_max_conn_idle_time=2m&connect_timeout=25"

	pool, err := pg.Open(context.Background(), pg.Options{
		URL:             url,
		MaxConnLifetime: 45 * time.Minute,
		MaxConnIdleTime: 9 * time.Minute,
		ConnectTimeout:  3 * time.Second,
	})
	require.NoError(t, err)
	defer pool.Close()

	cfg := pool.Config()
	assert.Equal(t, 45*time.Minute, cfg.MaxConnLifetime)
	assert.Equal(t, 9*time.Minute, cfg.MaxConnIdleTime)
	assert.Equal(t, 3*time.Second, cfg.ConnConfig.ConnectTimeout)
}

func TestOpenDefaultsWhenNeitherSetsThem(t *testing.T) {
	db := testdb.Shared(t)

	pool, err := pg.Open(context.Background(), pg.Options{URL: db.URL})
	require.NoError(t, err)
	defer pool.Close()

	cfg := pool.Config()
	assert.Equal(t, time.Hour, cfg.MaxConnLifetime)
	assert.Equal(t, 30*time.Minute, cfg.MaxConnIdleTime)
	assert.Equal(t, 10*time.Second, cfg.ConnConfig.ConnectTimeout)
}

func TestOpenRefusesAnEmptyURL(t *testing.T) {
	// libpq's defaults would otherwise aim the service at a local Unix socket
	// and report a failure that names neither the configuration nor the host.
	_, err := pg.Open(context.Background(), pg.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "URL is empty")

	_, err = pg.Open(context.Background(), pg.Options{URL: "   "})
	assert.Error(t, err)
}

func TestOpenDefaultsMatchPgxpool(t *testing.T) {
	// pg.Open's MaxConnLifetime and MaxConnIdleTime defaults duplicate
	// pgxpool's, which makes the third argument of pick unreachable for those
	// two. That is fine while they agree; this fails if they ever diverge, so
	// the duplication cannot rot into a silent difference.
	db := testdb.Shared(t)

	bare, err := pgxpool.ParseConfig(db.URL)
	require.NoError(t, err)

	pool, err := pg.Open(context.Background(), pg.Options{URL: db.URL})
	require.NoError(t, err)
	defer pool.Close()

	assert.Equal(t, bare.MaxConnLifetime, pool.Config().MaxConnLifetime)
	assert.Equal(t, bare.MaxConnIdleTime, pool.Config().MaxConnIdleTime)

	// ConnectTimeout is the one that genuinely needs a default: pgx leaves it
	// at zero, which means no timeout at all.
	assert.Zero(t, bare.ConnConfig.ConnectTimeout)
	assert.Equal(t, 10*time.Second, pool.Config().ConnConfig.ConnectTimeout)
}
