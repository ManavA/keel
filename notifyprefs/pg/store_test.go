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

	"github.com/ManavA/keel/log"
	"github.com/ManavA/keel/notifyprefs"
	notifypg "github.com/ManavA/keel/notifyprefs/pg"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// openStore gives a test its own Store over the package's shared database,
// applying the migration first. The migration is idempotent (CREATE TABLE IF
// NOT EXISTS), so running it once per test is cheap and safe against a table
// other tests are also using.
func openStore(t *testing.T) *notifypg.Store {
	t.Helper()
	db := testdb.Shared(t)

	pool, err := keelpg.Open(context.Background(), keelpg.Options{URL: db.URL, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	migration, err := os.ReadFile("migrations/001_notification_preferences.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), string(migration))
	require.NoError(t, err)

	return notifypg.New(pool)
}

// uniqueUser gives each test its own row in the shared table.
func uniqueUser(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

func TestStore_UnknownUserGetsDefaults(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	got, err := store.Get(ctx, uniqueUser(t))
	require.NoError(t, err)
	assert.False(t, got.OptedOut(notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail))

	allowed, err := store.Allowed(ctx, uniqueUser(t), notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail)
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestStore_SetThenGetRoundTrip(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	user := uniqueUser(t)

	prefs := notifyprefs.Preferences{}
	prefs.Set(notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail, true)
	prefs.Set(notifyprefs.CategoryProduct, notifyprefs.ChannelPush, true)
	require.NoError(t, store.Set(ctx, user, prefs))

	got, err := store.Get(ctx, user)
	require.NoError(t, err)
	assert.True(t, got.OptedOut(notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail))
	assert.True(t, got.OptedOut(notifyprefs.CategoryProduct, notifyprefs.ChannelPush))
	assert.False(t, got.OptedOut(notifyprefs.CategoryMarketing, notifyprefs.ChannelPush))

	allowed, err := store.Allowed(ctx, user, notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail)
	require.NoError(t, err)
	assert.False(t, allowed)

	allowed, err = store.Allowed(ctx, user, notifyprefs.CategoryMarketing, notifyprefs.ChannelPush)
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestStore_SecurityOptOutDoesNotPersist(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	user := uniqueUser(t)

	prefs := notifyprefs.Preferences{}
	prefs.Set(notifyprefs.CategorySecurity, notifyprefs.ChannelEmail, true)
	prefs.Set(notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail, true)
	require.NoError(t, store.Set(ctx, user, prefs))

	got, err := store.Get(ctx, user)
	require.NoError(t, err)
	assert.False(t, got.OptedOut(notifyprefs.CategorySecurity, notifyprefs.ChannelEmail),
		"security opt-outs must be stripped on write")

	allowed, err := store.Allowed(ctx, user, notifyprefs.CategorySecurity, notifyprefs.ChannelEmail)
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestStore_SetReplacesRatherThanMerges(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	user := uniqueUser(t)

	prefs := notifyprefs.Preferences{}
	prefs.Set(notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail, true)
	require.NoError(t, store.Set(ctx, user, prefs))

	require.NoError(t, store.Set(ctx, user, notifyprefs.Preferences{}))

	allowed, err := store.Allowed(ctx, user, notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail)
	require.NoError(t, err)
	assert.True(t, allowed, "replacing with empty prefs re-subscribes the user")
}

func TestStore_EmptyUserIDIsAnError(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	_, err := store.Get(ctx, "")
	assert.Error(t, err)

	assert.Error(t, store.Set(ctx, "", notifyprefs.Preferences{}))

	_, err = store.Allowed(ctx, "", notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail)
	assert.Error(t, err)
}
