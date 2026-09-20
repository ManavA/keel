package notifyprefs_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/notifyprefs"
)

func TestMemoryStore_DefaultsAllowEverything(t *testing.T) {
	ctx := context.Background()
	store := notifyprefs.NewMemoryStore()

	for _, cat := range []notifyprefs.Category{
		notifyprefs.CategorySecurity,
		notifyprefs.CategoryTransactional,
		notifyprefs.CategoryMarketing,
		notifyprefs.CategoryProduct,
	} {
		allowed, err := store.Allowed(ctx, "new-user", cat, notifyprefs.ChannelEmail)
		require.NoError(t, err)
		assert.True(t, allowed, "category %q should send by default", cat)
	}
}

func TestMemoryStore_OptOutBlocksOnlyThatPair(t *testing.T) {
	ctx := context.Background()
	store := notifyprefs.NewMemoryStore()

	prefs := notifyprefs.Preferences{}
	prefs.Set(notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail, true)
	require.NoError(t, store.Set(ctx, "user-1", prefs))

	allowed, err := store.Allowed(ctx, "user-1", notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail)
	require.NoError(t, err)
	assert.False(t, allowed)

	// Other channels and categories for the same user are unaffected.
	allowed, err = store.Allowed(ctx, "user-1", notifyprefs.CategoryMarketing, notifyprefs.ChannelPush)
	require.NoError(t, err)
	assert.True(t, allowed)

	allowed, err = store.Allowed(ctx, "user-1", notifyprefs.CategoryProduct, notifyprefs.ChannelEmail)
	require.NoError(t, err)
	assert.True(t, allowed)

	// The opt-out round-trips through Get.
	got, err := store.Get(ctx, "user-1")
	require.NoError(t, err)
	assert.True(t, got.OptedOut(notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail))
}

func TestMemoryStore_SecurityAndTransactionalAlwaysOn(t *testing.T) {
	ctx := context.Background()
	store := notifyprefs.NewMemoryStore()

	prefs := notifyprefs.Preferences{}
	prefs.Set(notifyprefs.CategorySecurity, notifyprefs.ChannelEmail, true)
	prefs.Set(notifyprefs.CategoryTransactional, notifyprefs.ChannelEmail, true)
	require.NoError(t, store.Set(ctx, "user-1", prefs))

	for _, cat := range []notifyprefs.Category{
		notifyprefs.CategorySecurity,
		notifyprefs.CategoryTransactional,
	} {
		allowed, err := store.Allowed(ctx, "user-1", cat, notifyprefs.ChannelEmail)
		require.NoError(t, err)
		assert.True(t, allowed, "category %q must always send", cat)
	}
}

func TestMemoryStore_EmptyUserIDIsAnError(t *testing.T) {
	ctx := context.Background()
	store := notifyprefs.NewMemoryStore()

	_, err := store.Get(ctx, "")
	assert.Error(t, err)

	assert.Error(t, store.Set(ctx, "", notifyprefs.Preferences{}))

	_, err = store.Allowed(ctx, "", notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail)
	assert.Error(t, err)
}

func TestAllowedBy_PureFunction(t *testing.T) {
	prefs := notifyprefs.Preferences{}
	prefs.Set(notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail, true)
	// Even a hand-built value claiming a security opt-out cannot suppress it.
	prefs.OptOuts[notifyprefs.CategorySecurity] = map[notifyprefs.Channel]bool{notifyprefs.ChannelEmail: true}

	assert.False(t, notifyprefs.AllowedBy(prefs, notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail))
	assert.True(t, notifyprefs.AllowedBy(prefs, notifyprefs.CategoryMarketing, notifyprefs.ChannelSMS))
	assert.True(t, notifyprefs.AllowedBy(prefs, notifyprefs.CategorySecurity, notifyprefs.ChannelEmail))
	assert.True(t, notifyprefs.AllowedBy(notifyprefs.Preferences{}, notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail))
}
