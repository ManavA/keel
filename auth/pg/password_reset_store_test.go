package pg_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/auth"
	authpg "github.com/ManavA/keel/auth/pg"
)

func TestPasswordResetStoreConsumeValid(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	resets := authpg.NewPasswordResetStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))

	hash := uniqueTokenHash(t)
	require.NoError(t, resets.Create(ctx, u.ID, hash, time.Now().Add(time.Hour)))

	userID, err := resets.ConsumeValid(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, u.ID, userID)
}

func TestPasswordResetStoreCannotConsumeTwice(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	resets := authpg.NewPasswordResetStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))
	hash := uniqueTokenHash(t)
	require.NoError(t, resets.Create(ctx, u.ID, hash, time.Now().Add(time.Hour)))

	_, err := resets.ConsumeValid(ctx, hash)
	require.NoError(t, err)
	_, err = resets.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, auth.ErrPasswordResetTokenInvalid)
}

func TestPasswordResetStoreRejectsExpired(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	resets := authpg.NewPasswordResetStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))
	hash := uniqueTokenHash(t)
	require.NoError(t, resets.Create(ctx, u.ID, hash, time.Now().Add(-time.Minute)))

	_, err := resets.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, auth.ErrPasswordResetTokenInvalid)
}

func TestPasswordResetStoreInvalidateForUser(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	resets := authpg.NewPasswordResetStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))
	hash := uniqueTokenHash(t)
	require.NoError(t, resets.Create(ctx, u.ID, hash, time.Now().Add(time.Hour)))

	require.NoError(t, resets.InvalidateForUser(ctx, u.ID))

	_, err := resets.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, auth.ErrPasswordResetTokenInvalid)
}
