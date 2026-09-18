package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryUserStoreCreateAndLookup(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()

	u := &User{Email: "buyer@example.com", PasswordHash: "hash"}
	require.NoError(t, store.Create(ctx, u))
	assert.NotEmpty(t, u.ID)

	byEmail, err := store.GetByEmail(ctx, "buyer@example.com")
	require.NoError(t, err)
	assert.Equal(t, u.ID, byEmail.ID)

	byID, err := store.GetByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "buyer@example.com", byID.Email)
}

func TestMemoryUserStoreDuplicateEmail(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()
	require.NoError(t, store.Create(ctx, &User{Email: "dup@example.com"}))

	err := store.Create(ctx, &User{Email: "dup@example.com"})
	assert.ErrorIs(t, err, ErrDuplicateEmail)
}

func TestMemoryUserStoreNotFound(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()

	_, err := store.GetByEmail(ctx, "nobody@example.com")
	assert.ErrorIs(t, err, ErrUserNotFound)

	_, err = store.GetByID(ctx, "missing")
	assert.ErrorIs(t, err, ErrUserNotFound)

	_, err = store.GetByExternalUID(ctx, "missing-uid")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestMemoryUserStoreByExternalUID(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()
	u := &User{Email: "sso@example.com", ExternalUID: "firebase-uid-1"}
	require.NoError(t, store.Create(ctx, u))

	got, err := store.GetByExternalUID(ctx, "firebase-uid-1")
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)
}

func TestMemoryUserStoreSetEmailVerified(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()
	u := &User{Email: "verify@example.com"}
	require.NoError(t, store.Create(ctx, u))

	require.NoError(t, store.SetEmailVerified(ctx, u.ID))

	got, err := store.GetByID(ctx, u.ID)
	require.NoError(t, err)
	assert.True(t, got.EmailVerified)
}

func TestMemoryUserStoreSetEmailVerifiedMissingUser(t *testing.T) {
	store := NewMemoryUserStore()
	err := store.SetEmailVerified(context.Background(), "does-not-exist")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestMemoryUserStoreSetPasswordHash(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()
	u := &User{Email: "changeme@example.com", PasswordHash: "old-hash"}
	require.NoError(t, store.Create(ctx, u))

	require.NoError(t, store.SetPasswordHash(ctx, u.ID, "new-hash"))

	got, err := store.GetByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "new-hash", got.PasswordHash)
}

func TestMemoryUserStoreSetPasswordHashMissingUser(t *testing.T) {
	err := NewMemoryUserStore().SetPasswordHash(context.Background(), "missing", "hash")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestMemoryUserStoreLinkExternalUID(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()
	u := &User{Email: "link@example.com"}
	require.NoError(t, store.Create(ctx, u))

	require.NoError(t, store.LinkExternalUID(ctx, u.ID, "ext-uid-99"))

	got, err := store.GetByExternalUID(ctx, "ext-uid-99")
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)
}

func TestMemoryUserStoreLinkExternalUIDMissingUser(t *testing.T) {
	err := NewMemoryUserStore().LinkExternalUID(context.Background(), "missing", "ext-uid")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestMemoryUserStoreDelete(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()
	u := &User{Email: "gone@example.com", ExternalUID: "ext-uid-gone"}
	require.NoError(t, store.Create(ctx, u))

	require.NoError(t, store.Delete(ctx, u.ID))

	_, err := store.GetByID(ctx, u.ID)
	assert.ErrorIs(t, err, ErrUserNotFound)
	_, err = store.GetByEmail(ctx, "gone@example.com")
	assert.ErrorIs(t, err, ErrUserNotFound)
	_, err = store.GetByExternalUID(ctx, "ext-uid-gone")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestMemoryUserStoreDeleteMissingUser(t *testing.T) {
	err := NewMemoryUserStore().Delete(context.Background(), "missing")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestMemoryUserStoreMutatingReturnedUserDoesNotAffectStore(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()
	u := &User{Email: "isolated@example.com"}
	require.NoError(t, store.Create(ctx, u))

	got, err := store.GetByID(ctx, u.ID)
	require.NoError(t, err)
	got.Email = "mutated@example.com"

	fresh, err := store.GetByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "isolated@example.com", fresh.Email)
}
