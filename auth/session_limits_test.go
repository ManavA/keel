package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemorySessionStoreIdleTimeoutExpiresIdleSession is the acceptance check
// for configurable idle timeout: a session left unused past the idle window
// must stop validating, even though its base ttl has not elapsed. A stolen
// token must not stay usable until explicit revocation just because nobody
// revoked it.
func TestMemorySessionStoreIdleTimeoutExpiresIdleSession(t *testing.T) {
	store := NewMemorySessionStoreWithLimits(SessionLimits{IdleTimeout: time.Minute})
	ctx := context.Background()

	now := time.Now()
	store.nowFunc = func() time.Time { return now }

	token, err := store.Create(ctx, "user_1", time.Hour)
	require.NoError(t, err)

	// Still within the idle window: validates.
	now = now.Add(30 * time.Second)
	_, err = store.Validate(ctx, token)
	require.NoError(t, err)

	// Past the idle window, within the base ttl: must not validate.
	now = now.Add(2 * time.Minute)
	_, err = store.Validate(ctx, token)
	assert.ErrorIs(t, err, ErrSessionNotFound, "a session idle past the window must stop validating")
}

// TestMemorySessionStoreActiveSessionExtendsIdleWindow pins the sliding part
// of the window: each successful validation moves the idle deadline forward,
// so a session in steady use survives past the idle duration measured from
// creation, while one that goes quiet still expires.
func TestMemorySessionStoreActiveSessionExtendsIdleWindow(t *testing.T) {
	store := NewMemorySessionStoreWithLimits(SessionLimits{IdleTimeout: time.Minute})
	ctx := context.Background()

	now := time.Now()
	store.nowFunc = func() time.Time { return now }

	token, err := store.Create(ctx, "user_1", time.Hour)
	require.NoError(t, err)

	// Activity just inside the window slides the deadline forward.
	now = now.Add(50 * time.Second)
	_, err = store.Validate(ctx, token)
	require.NoError(t, err)

	// 100 seconds after creation — past the idle window measured from
	// creation, but within it measured from the last validation.
	now = now.Add(50 * time.Second)
	_, err = store.Validate(ctx, token)
	require.NoError(t, err, "activity must extend the idle window")

	// Quiet for longer than the window after the last validation: expired.
	now = now.Add(61 * time.Second)
	_, err = store.Validate(ctx, token)
	assert.ErrorIs(t, err, ErrSessionNotFound)
}

// TestMemorySessionStoreAbsoluteLifetimeCapsActiveSession pins the hard cap:
// no amount of activity extends a session past its absolute lifetime. The
// base ttl still bounds the session when no absolute lifetime is configured.
func TestMemorySessionStoreAbsoluteLifetimeCapsActiveSession(t *testing.T) {
	store := NewMemorySessionStoreWithLimits(SessionLimits{
		IdleTimeout:      time.Minute,
		AbsoluteLifetime: 90 * time.Second,
	})
	ctx := context.Background()

	now := time.Now()
	store.nowFunc = func() time.Time { return now }

	token, err := store.Create(ctx, "user_1", time.Hour)
	require.NoError(t, err)

	now = now.Add(50 * time.Second)
	_, err = store.Validate(ctx, token)
	require.NoError(t, err)

	// Active right up to the cap, but past it: must not validate.
	now = now.Add(50 * time.Second)
	_, err = store.Validate(ctx, token)
	assert.ErrorIs(t, err, ErrSessionNotFound, "activity must not extend a session past its absolute lifetime")
}

// TestMemorySessionStoreZeroLimitsDisablesWindows pins the default: a store
// built without limits behaves exactly as before — only the base ttl bounds
// the session, so existing callers see no behavior change.
func TestMemorySessionStoreZeroLimitsDisablesWindows(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()

	now := time.Now()
	store.nowFunc = func() time.Time { return now }

	token, err := store.Create(ctx, "user_1", time.Hour)
	require.NoError(t, err)

	now = now.Add(59 * time.Minute)
	_, err = store.Validate(ctx, token)
	require.NoError(t, err, "without limits, only the base ttl bounds the session")
}
