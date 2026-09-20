package pg_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/flags"
	flagspg "github.com/ManavA/keel/flags/pg"
	"github.com/ManavA/keel/log"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// openStore gives a test its own rows in the shared table, applying the
// migration first. The migration is idempotent (CREATE TABLE IF NOT
// EXISTS), so running it once per test is cheap and safe against a table
// other tests are also using.
func openStore(t *testing.T) *flagspg.Store {
	t.Helper()
	db := testdb.Shared(t)

	pool, err := keelpg.Open(context.Background(), keelpg.Options{URL: db.URL, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	migration, err := os.ReadFile("migrations/001_feature_flags.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), string(migration))
	require.NoError(t, err)

	return flagspg.New(pool)
}

// uniqueKey gives each test its own rows in the shared table.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

func TestStore_UpsertGetList(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	key := uniqueKey(t)

	_, err := store.Get(ctx, key)
	assert.ErrorIs(t, err, flags.ErrNotFound)

	require.NoError(t, store.Upsert(ctx, flags.Flag{
		Key: key, Enabled: true, Percentage: 50, Allow: []string{"vip-1", "vip-2"},
	}))

	got, err := store.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, key, got.Key)
	assert.True(t, got.Enabled)
	assert.Equal(t, 50, got.Percentage)
	assert.Equal(t, []string{"vip-1", "vip-2"}, got.Allow)

	// Evaluation over a stored flag is deterministic: the acceptance shape
	// from the core package, against a definition that crossed Postgres.
	for i := 0; i < 50; i++ {
		subject := fmt.Sprintf("user-%d", i)
		assert.Equal(t, flags.Evaluate(got, subject), flags.Evaluate(got, subject))
	}
	assert.True(t, flags.Evaluate(got, "vip-1"))

	// Upsert replaces the definition, including clearing the allowlist.
	require.NoError(t, store.Upsert(ctx, flags.Flag{Key: key, Enabled: false}))
	got, err = store.Get(ctx, key)
	require.NoError(t, err)
	assert.False(t, got.Enabled)
	assert.Empty(t, got.Allow)
	assert.False(t, flags.Evaluate(got, "vip-1"))

	all, err := store.List(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, all)
	for i := 1; i < len(all); i++ {
		assert.Less(t, all[i-1].Key, all[i].Key, "List must come back ordered by key")
	}
}

func TestStore_UpsertRejectsInvalid(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	assert.Error(t, store.Upsert(ctx, flags.Flag{Key: uniqueKey(t), Percentage: 101}))
	_, err := store.Get(ctx, "never-stored")
	assert.ErrorIs(t, err, flags.ErrNotFound)
}
