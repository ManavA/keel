package pg_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/admin"
	adminpg "github.com/ManavA/keel/admin/pg"
)

func TestAdminStoreCreateAndLookup(t *testing.T) {
	pool := newTestPool(t)
	store := adminpg.NewAdminStore(pool)
	ctx := context.Background()

	email := uniqueEmail(t)
	a := &admin.Admin{Email: email, Name: "Ops", Role: "admin", PasswordHash: "hash"}
	require.NoError(t, store.Create(ctx, a))
	assert.NotEmpty(t, a.ID)
	assert.False(t, a.CreatedAt.IsZero())

	byEmail, err := store.GetByEmail(ctx, email)
	require.NoError(t, err)
	assert.Equal(t, a.ID, byEmail.ID)
	assert.Equal(t, "Ops", byEmail.Name)

	byID, err := store.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, email, byID.Email)
}

func TestAdminStoreEmailLookupIsCaseInsensitive(t *testing.T) {
	pool := newTestPool(t)
	store := adminpg.NewAdminStore(pool)
	ctx := context.Background()

	email := uniqueEmail(t)
	a := &admin.Admin{Email: email, PasswordHash: "hash"}
	require.NoError(t, store.Create(ctx, a))

	got, err := store.GetByEmail(ctx, upperCaseEmail(email))
	require.NoError(t, err, "citext must make this lookup case-insensitive")
	assert.Equal(t, a.ID, got.ID)
}

func TestAdminStoreDuplicateEmailIsCaseInsensitive(t *testing.T) {
	pool := newTestPool(t)
	store := adminpg.NewAdminStore(pool)
	ctx := context.Background()

	email := uniqueEmail(t)
	require.NoError(t, store.Create(ctx, &admin.Admin{Email: email, PasswordHash: "hash"}))

	err := store.Create(ctx, &admin.Admin{Email: upperCaseEmail(email), PasswordHash: "hash"})
	assert.ErrorIs(t, err, admin.ErrDuplicateEmail)
}

func TestAdminStoreNotFound(t *testing.T) {
	pool := newTestPool(t)
	store := adminpg.NewAdminStore(pool)
	ctx := context.Background()

	_, err := store.GetByEmail(ctx, uniqueEmail(t))
	assert.ErrorIs(t, err, admin.ErrAdminNotFound)

	_, err = store.GetByID(ctx, "00000000-0000-0000-0000-000000000000")
	assert.ErrorIs(t, err, admin.ErrAdminNotFound)
}

func TestAdminStoreUpdateLastLogin(t *testing.T) {
	pool := newTestPool(t)
	store := adminpg.NewAdminStore(pool)
	ctx := context.Background()

	a := &admin.Admin{Email: uniqueEmail(t), PasswordHash: "hash"}
	require.NoError(t, store.Create(ctx, a))
	assert.True(t, a.LastLoginAt.IsZero())

	require.NoError(t, store.UpdateLastLogin(ctx, a.ID))

	got, err := store.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.False(t, got.LastLoginAt.IsZero())
}

func TestAdminStoreUpdateLastLoginMissingAdmin(t *testing.T) {
	pool := newTestPool(t)
	store := adminpg.NewAdminStore(pool)
	err := store.UpdateLastLogin(context.Background(), "00000000-0000-0000-0000-000000000000")
	assert.ErrorIs(t, err, admin.ErrAdminNotFound)
}

func upperCaseEmail(email string) string {
	upper := ""
	for _, r := range email {
		if r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		upper += string(r)
	}
	return upper
}

func TestAdminStoreListAndRevokeSessions(t *testing.T) {
	pool := newTestPool(t)
	store := adminpg.NewAdminStore(pool)
	ctx := context.Background()

	first := &admin.Admin{Email: uniqueEmail(t), Name: "First", Role: "admin", PasswordHash: "hash"}
	require.NoError(t, store.Create(ctx, first))
	second := &admin.Admin{Email: uniqueEmail(t), Name: "Second", Role: "viewer", PasswordHash: "hash"}
	require.NoError(t, store.Create(ctx, second))

	admins, err := store.List(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(admins), 2, "the list holds every admin")
	emails := make([]string, 0, len(admins))
	for _, a := range admins {
		emails = append(emails, a.Email)
	}
	assert.Contains(t, emails, first.Email)
	assert.Contains(t, emails, second.Email)
	assert.True(t, isSortedStrings(emails), "the list is ordered by email")

	got, err := store.GetByID(ctx, first.ID)
	require.NoError(t, err)
	assert.Zero(t, got.SessionEpoch)

	require.NoError(t, store.RevokeSessions(ctx, first.ID))
	got, err = store.GetByID(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.SessionEpoch)

	other, err := store.GetByID(ctx, second.ID)
	require.NoError(t, err)
	assert.Zero(t, other.SessionEpoch, "revoking one admin leaves the others alone")
}

func TestAdminStoreRevokeSessionsMissingAdmin(t *testing.T) {
	pool := newTestPool(t)
	store := adminpg.NewAdminStore(pool)
	err := store.RevokeSessions(context.Background(), "00000000-0000-0000-0000-000000000000")
	assert.ErrorIs(t, err, admin.ErrAdminNotFound)
}

func isSortedStrings(in []string) bool {
	for i := 1; i < len(in); i++ {
		if in[i-1] > in[i] {
			return false
		}
	}
	return true
}
