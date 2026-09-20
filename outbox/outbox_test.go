package outbox_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/log"
	"github.com/ManavA/keel/outbox"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// openPool gives a test its own pool over the package's shared database,
// applying the migrations first. Each migration is idempotent, so running
// them once per test is cheap and safe against a table other tests are
// also using.
func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Shared(t)

	pool, err := keelpg.Open(context.Background(), keelpg.Options{URL: db.URL, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	entries, err := os.ReadDir("pg/migrations")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		migration, err := os.ReadFile("pg/migrations/" + entry.Name())
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(migration))
		require.NoError(t, err, "apply migration %s", entry.Name())
	}

	return pool
}

// openEmptyPool is openPool for a test whose Relay.Tick calls fetch every
// unpublished row in the table, rather than one identified by its own
// topic — such a test cannot tolerate rows another test left behind.
func openEmptyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openPool(t)
	_, err := pool.Exec(context.Background(), "truncate table outbox_events")
	require.NoError(t, err)
	return pool
}

// enqueueOne enqueues one event and returns the id Postgres assigned it,
// for a test that needs to name a specific row.
func enqueueOne(t *testing.T, pool *pgxpool.Pool, topic string, payload []byte) string {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, keelpg.InTx(ctx, pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.Event{Topic: topic, Payload: payload})
	}))

	var id string
	require.NoError(t, pool.QueryRow(ctx,
		"select id from outbox_events where topic = $1 order by created_at desc limit 1", topic,
	).Scan(&id))
	return id
}

func assertPublished(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	var published bool
	require.NoError(t, pool.QueryRow(context.Background(),
		"select published_at is not null from outbox_events where id = $1", id,
	).Scan(&published))
	assert.True(t, published, "expected %s to be marked published", id)
}

func assertUnpublished(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	var published bool
	require.NoError(t, pool.QueryRow(context.Background(),
		"select published_at is not null from outbox_events where id = $1", id,
	).Scan(&published))
	assert.False(t, published, "expected %s to still be unpublished", id)
}

func TestEnqueue_CommitsWithCallersTransaction(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	require.NoError(t, keelpg.InTx(ctx, pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.Event{Topic: "orders.created", Payload: map[string]any{"id": 1}})
	}))

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"select count(*) from outbox_events where topic = $1", "orders.created").Scan(&count))
	assert.Equal(t, 1, count)
}

func TestEnqueue_RollsBackWithCallersTransaction(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	domainErr := errors.New("domain write failed")
	err := keelpg.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := outbox.Enqueue(ctx, tx, outbox.Event{Topic: "orders.rolledback", Payload: []byte("x")}); err != nil {
			return err
		}
		return domainErr
	})
	require.ErrorIs(t, err, domainErr)

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"select count(*) from outbox_events where topic = $1", "orders.rolledback").Scan(&count))
	assert.Equal(t, 0, count, "a rolled-back caller transaction must take the outbox row with it")
}

func TestEnqueue_EmptyTopicErrors(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	err := keelpg.InTx(ctx, pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.Event{Payload: []byte("x")})
	})
	assert.Error(t, err)
}

func TestEnqueue_BytePayloadStoredVerbatim(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	require.NoError(t, keelpg.InTx(ctx, pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.Event{Topic: "raw.bytes", Payload: []byte("not-json")})
	}))

	var payload []byte
	require.NoError(t, pool.QueryRow(ctx,
		"select payload from outbox_events where topic = $1", "raw.bytes").Scan(&payload))
	assert.Equal(t, []byte("not-json"), payload)
}
