package admin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryAdminStoreCreateAndLookup(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryAdminStore()

	a := &Admin{Email: "ops@example.com", Name: "Ops", Role: "admin", PasswordHash: "hash"}
	require.NoError(t, store.Create(ctx, a))
	assert.NotEmpty(t, a.ID)

	byEmail, err := store.GetByEmail(ctx, "ops@example.com")
	require.NoError(t, err)
	assert.Equal(t, a.ID, byEmail.ID)

	byID, err := store.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "ops@example.com", byID.Email)
}

func TestMemoryAdminStoreDuplicateEmail(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryAdminStore()
	require.NoError(t, store.Create(ctx, &Admin{Email: "dup@example.com"}))

	err := store.Create(ctx, &Admin{Email: "dup@example.com"})
	assert.ErrorIs(t, err, ErrDuplicateEmail)
}

func TestMemoryAdminStoreNotFound(t *testing.T) {
	store := NewMemoryAdminStore()
	_, err := store.GetByEmail(context.Background(), "nobody@example.com")
	assert.ErrorIs(t, err, ErrAdminNotFound)
	_, err = store.GetByID(context.Background(), "missing")
	assert.ErrorIs(t, err, ErrAdminNotFound)
}

func TestMemoryAdminStoreUpdateLastLogin(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryAdminStore()
	a := &Admin{Email: "seen@example.com"}
	require.NoError(t, store.Create(ctx, a))
	assert.True(t, a.LastLoginAt.IsZero())

	require.NoError(t, store.UpdateLastLogin(ctx, a.ID))

	got, err := store.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.False(t, got.LastLoginAt.IsZero())
}

func TestMemoryAdminStoreUpdateLastLoginMissingAdmin(t *testing.T) {
	err := NewMemoryAdminStore().UpdateLastLogin(context.Background(), "missing")
	assert.ErrorIs(t, err, ErrAdminNotFound)
}

func TestMemoryAdminStoreMutatingReturnedAdminDoesNotAffectStore(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryAdminStore()
	a := &Admin{Email: "isolated@example.com"}
	require.NoError(t, store.Create(ctx, a))

	got, err := store.GetByID(ctx, a.ID)
	require.NoError(t, err)
	got.Email = "mutated@example.com"

	fresh, err := store.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "isolated@example.com", fresh.Email)
}
