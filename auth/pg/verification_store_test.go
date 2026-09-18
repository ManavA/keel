package pg_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/auth"
	authpg "github.com/ManavA/keel/auth/pg"
)

// uniqueTokenHash gives each test its own token hash, for the same reason
// uniqueEmail exists: every test in this package shares one database.
func uniqueTokenHash(t *testing.T) string {
	t.Helper()
	return auth.HashVerificationToken(fmt.Sprintf("%s-%d", t.Name(), testCounter.next()))
}

func TestVerificationStoreConsumeValid(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	verifications := authpg.NewVerificationStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))

	hash := uniqueTokenHash(t)
	require.NoError(t, verifications.Create(ctx, u.ID, hash, time.Now().Add(time.Hour)))

	userID, err := verifications.ConsumeValid(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, u.ID, userID)
}

func TestVerificationStoreCannotConsumeTwice(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	verifications := authpg.NewVerificationStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))
	hash := uniqueTokenHash(t)
	require.NoError(t, verifications.Create(ctx, u.ID, hash, time.Now().Add(time.Hour)))

	_, err := verifications.ConsumeValid(ctx, hash)
	require.NoError(t, err)
	_, err = verifications.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, auth.ErrVerificationTokenInvalid)
}

func TestVerificationStoreRejectsExpired(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	verifications := authpg.NewVerificationStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))
	hash := uniqueTokenHash(t)
	require.NoError(t, verifications.Create(ctx, u.ID, hash, time.Now().Add(-time.Minute)))

	_, err := verifications.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, auth.ErrVerificationTokenInvalid)
}

func TestVerificationStoreRejectsUnknownToken(t *testing.T) {
	pool := newTestPool(t)
	verifications := authpg.NewVerificationStore(pool)
	_, err := verifications.ConsumeValid(context.Background(), uniqueTokenHash(t))
	assert.ErrorIs(t, err, auth.ErrVerificationTokenInvalid)
}

func TestVerificationStoreInvalidateForUser(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	verifications := authpg.NewVerificationStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))
	hash := uniqueTokenHash(t)
	require.NoError(t, verifications.Create(ctx, u.ID, hash, time.Now().Add(time.Hour)))

	require.NoError(t, verifications.InvalidateForUser(ctx, u.ID))

	_, err := verifications.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, auth.ErrVerificationTokenInvalid)
}
