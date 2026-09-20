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

// TestSessionStoreIdleTimeoutExpiresIdleSession pins the idle window on the
// Postgres store: a session left unused past IdleTimeout stops validating
// even though its base ttl has not elapsed.
func TestSessionStoreIdleTimeoutExpiresIdleSession(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	sessions := authpg.NewSessionStoreWithLimits(pool, auth.SessionLimits{IdleTimeout: 200 * time.Millisecond})
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))

	token, err := sessions.Create(ctx, u.ID, time.Hour)
	require.NoError(t, err)

	_, err = sessions.Validate(ctx, token)
	require.NoError(t, err)

	time.Sleep(400 * time.Millisecond)

	_, err = sessions.Validate(ctx, token)
	assert.ErrorIs(t, err, auth.ErrSessionNotFound, "a session idle past the window must stop validating")
}

// TestSessionStoreActiveSessionExtendsIdleWindow pins the sliding part of the
// window on the Postgres store: each successful validation moves last_seen_at
// forward, so a session in steady use survives past the idle duration measured
// from creation.
func TestSessionStoreActiveSessionExtendsIdleWindow(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	sessions := authpg.NewSessionStoreWithLimits(pool, auth.SessionLimits{IdleTimeout: 200 * time.Millisecond})
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))

	token, err := sessions.Create(ctx, u.ID, time.Hour)
	require.NoError(t, err)

	time.Sleep(150 * time.Millisecond)
	_, err = sessions.Validate(ctx, token)
	require.NoError(t, err)

	// 300ms after creation — past the window measured from creation, but
	// within it measured from the last validation.
	time.Sleep(150 * time.Millisecond)
	_, err = sessions.Validate(ctx, token)
	require.NoError(t, err, "activity must extend the idle window")

	time.Sleep(300 * time.Millisecond)
	_, err = sessions.Validate(ctx, token)
	assert.ErrorIs(t, err, auth.ErrSessionNotFound)
}

// TestSessionStoreAbsoluteLifetimeCapsActiveSession pins the hard cap on the
// Postgres store: no amount of activity extends a session past its absolute
// lifetime.
func TestSessionStoreAbsoluteLifetimeCapsActiveSession(t *testing.T) {
	pool := newTestPool(t)
	users := authpg.NewUserStore(pool)
	sessions := authpg.NewSessionStoreWithLimits(pool, auth.SessionLimits{
		IdleTimeout:      200 * time.Millisecond,
		AbsoluteLifetime: 300 * time.Millisecond,
	})
	ctx := context.Background()

	u := &auth.User{Email: uniqueEmail(t)}
	require.NoError(t, users.Create(ctx, u))

	token, err := sessions.Create(ctx, u.ID, time.Hour)
	require.NoError(t, err)

	time.Sleep(150 * time.Millisecond)
	_, err = sessions.Validate(ctx, token)
	require.NoError(t, err)

	time.Sleep(200 * time.Millisecond)
	_, err = sessions.Validate(ctx, token)
	assert.ErrorIs(t, err, auth.ErrSessionNotFound, "activity must not extend a session past its absolute lifetime")
}
