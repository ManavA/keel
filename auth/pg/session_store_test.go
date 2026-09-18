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

func TestSessionStoreCreateAndValidate(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	sessions := authpg.NewSessionStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))

	token, err := sessions.Create(ctx, u.ID, time.Hour)
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	userID, err := sessions.Validate(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, u.ID, userID)
}

func TestSessionStoreValidateRejectsExpired(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	sessions := authpg.NewSessionStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))

	token, err := sessions.Create(ctx, u.ID, 20*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)

	_, err = sessions.Validate(ctx, token)
	assert.ErrorIs(t, err, auth.ErrSessionNotFound)
}

func TestSessionStoreCreateRejectsNonPositiveTTL(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	sessions := authpg.NewSessionStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))

	_, err := sessions.Create(ctx, u.ID, 0)
	assert.Error(t, err)
	_, err = sessions.Create(ctx, u.ID, -time.Minute)
	assert.Error(t, err)
}

func TestSessionStoreValidateRejectsUnknownToken(t *testing.T) {
	pool := newTestPool(t)
	sessions := authpg.NewSessionStore(pool)
	_, err := sessions.Validate(context.Background(), "never-issued")
	assert.ErrorIs(t, err, auth.ErrSessionNotFound)
}

func TestSessionStoreRevoke(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	sessions := authpg.NewSessionStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))
	token, err := sessions.Create(ctx, u.ID, time.Hour)
	require.NoError(t, err)

	require.NoError(t, sessions.Revoke(ctx, token))

	_, err = sessions.Validate(ctx, token)
	assert.ErrorIs(t, err, auth.ErrSessionNotFound)
}

func TestSessionStoreRevokeUnknownTokenIsNotAnError(t *testing.T) {
	pool := newTestPool(t)
	sessions := authpg.NewSessionStore(pool)
	assert.NoError(t, sessions.Revoke(context.Background(), "never-issued"))
}

func TestSessionStoreRevokeAllForUser(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	sessions := authpg.NewSessionStore(pool)
	ctx := context.Background()

	u1 := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u1))
	u2 := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u2))

	tokenA, err := sessions.Create(ctx, u1.ID, time.Hour)
	require.NoError(t, err)
	tokenB, err := sessions.Create(ctx, u1.ID, time.Hour)
	require.NoError(t, err)
	otherToken, err := sessions.Create(ctx, u2.ID, time.Hour)
	require.NoError(t, err)

	require.NoError(t, sessions.RevokeAllForUser(ctx, u1.ID))

	_, err = sessions.Validate(ctx, tokenA)
	assert.ErrorIs(t, err, auth.ErrSessionNotFound)
	_, err = sessions.Validate(ctx, tokenB)
	assert.ErrorIs(t, err, auth.ErrSessionNotFound)

	stillValid, err := sessions.Validate(ctx, otherToken)
	require.NoError(t, err)
	assert.Equal(t, u2.ID, stillValid)
}

func TestSessionStoreDeletingUserCascadesSessions(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	sessions := authpg.NewSessionStore(pool)
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))
	token, err := sessions.Create(ctx, u.ID, time.Hour)
	require.NoError(t, err)

	require.NoError(t, users.Delete(ctx, u.ID))

	_, err = sessions.Validate(ctx, token)
	assert.ErrorIs(t, err, auth.ErrSessionNotFound, "ON DELETE CASCADE must remove the session with its user")
}
