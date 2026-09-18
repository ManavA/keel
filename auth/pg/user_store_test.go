package pg_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/auth"
	authpg "github.com/ManavA/keel/auth/pg"
)

// uniqueEmail gives each test its own address, since every test in this
// package shares one database (see testdb.Shared's doc comment on why: a
// per-test container is a couple of seconds each, which adds up across a
// package with more than a handful of tests).
func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d@example.com", t.Name(), testCounter.next())
}

func TestUserStoreCreateAndLookup(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()

	email := uniqueEmail(t)
	u := &auth.User{Email: email, PasswordHash: "hash", Name: "Person"}
	require.NoError(t, store.Create(ctx, u))
	assert.NotEmpty(t, u.ID)
	assert.False(t, u.CreatedAt.IsZero())

	byEmail, err := store.GetByEmail(ctx, email)
	require.NoError(t, err)
	assert.Equal(t, u.ID, byEmail.ID)
	assert.Equal(t, "Person", byEmail.Name)

	byID, err := store.GetByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, email, byID.Email)
}

func TestUserStoreEmailLookupIsCaseInsensitive(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()

	email := uniqueEmail(t)
	u := &auth.User{Email: email}
	require.NoError(t, store.Create(ctx, u))

	got, err := store.GetByEmail(ctx, upperCaseEmail(email))
	require.NoError(t, err, "citext must make this lookup case-insensitive")
	assert.Equal(t, u.ID, got.ID)
}

func TestUserStoreDuplicateEmailIsCaseInsensitive(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()

	email := uniqueEmail(t)
	require.NoError(t, store.Create(ctx, &auth.User{Email: email}))

	err := store.Create(ctx, &auth.User{Email: upperCaseEmail(email)})
	assert.ErrorIs(t, err, auth.ErrDuplicateEmail)
}

func TestUserStoreNotFound(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()

	_, err := store.GetByEmail(ctx, uniqueEmail(t))
	assert.ErrorIs(t, err, auth.ErrUserNotFound)

	_, err = store.GetByID(ctx, "00000000-0000-0000-0000-000000000000")
	assert.ErrorIs(t, err, auth.ErrUserNotFound)
}

func TestUserStoreByExternalUID(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()

	externalUID := fmt.Sprintf("ext-%d", testCounter.next())
	u := &auth.User{Email: uniqueEmail(t), ExternalUID: externalUID}
	require.NoError(t, store.Create(ctx, u))

	got, err := store.GetByExternalUID(ctx, externalUID)
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)
}

func TestUserStoreSetEmailVerified(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, store.Create(ctx, u))
	require.NoError(t, store.SetEmailVerified(ctx, u.ID))

	got, err := store.GetByID(ctx, u.ID)
	require.NoError(t, err)
	assert.True(t, got.EmailVerified)
}

func TestUserStoreSetPasswordHash(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t), PasswordHash: "old"}
	require.NoError(t, store.Create(ctx, u))
	require.NoError(t, store.SetPasswordHash(ctx, u.ID, "new"))

	got, err := store.GetByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "new", got.PasswordHash)
}

func TestUserStoreLinkExternalUID(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, store.Create(ctx, u))

	externalUID := fmt.Sprintf("linked-%d", testCounter.next())
	require.NoError(t, store.LinkExternalUID(ctx, u.ID, externalUID))

	got, err := store.GetByExternalUID(ctx, externalUID)
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)
}

func TestUserStoreDelete(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, store.Create(ctx, u))
	require.NoError(t, store.Delete(ctx, u.ID))

	_, err := store.GetByID(ctx, u.ID)
	assert.ErrorIs(t, err, auth.ErrUserNotFound)
}

func TestUserStoreOperationsOnMissingUserReturnNotFound(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewUserStore(pool)
	ctx := context.Background()
	const missing = "00000000-0000-0000-0000-000000000000"

	assert.ErrorIs(t, store.SetEmailVerified(ctx, missing), auth.ErrUserNotFound)
	assert.ErrorIs(t, store.SetPasswordHash(ctx, missing, "x"), auth.ErrUserNotFound)
	assert.ErrorIs(t, store.LinkExternalUID(ctx, missing, "x"), auth.ErrUserNotFound)
	assert.ErrorIs(t, store.Delete(ctx, missing), auth.ErrUserNotFound)
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
