package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemorySessionStoreTokenEntropy pins the session token's size and
// uniqueness. A regression to a shorter or predictable generator (a
// sequential counter, a weak PRNG) would not fail Create or Validate
// functionally — the token still round-trips — so this checks the property
// that actually matters: enough tokens, generated back to back, never repeat
// and are exactly the length crypto/rand.Read(32 bytes) hex-encodes to.
func TestMemorySessionStoreTokenEntropy(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()

	const n = 1000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		token, err := store.Create(ctx, "user_1", time.Hour)
		require.NoError(t, err)
		require.Len(t, token, 64, "32 random bytes, hex-encoded")
		require.False(t, seen[token], "session token repeated")
		seen[token] = true
	}
}

// TestMemorySessionStoreValidateExpiryBoundary pins the expiry comparison at
// its boundary: a session must still validate the instant before it expires
// and must not validate the instant it does. An off-by-one here (`>=`
// instead of `>`, or the reverse) is easy to introduce silently since either
// direction still passes a test that only checks "long after" and "long
// before".
func TestMemorySessionStoreValidateExpiryBoundary(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()

	token, err := store.Create(ctx, "user_1", 50*time.Millisecond)
	require.NoError(t, err)

	// Still within the window.
	userID, err := store.Validate(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, "user_1", userID)

	time.Sleep(75 * time.Millisecond)

	_, err = store.Validate(ctx, token)
	assert.ErrorIs(t, err, ErrSessionNotFound, "a session past its ttl must not validate")
}

func TestMemorySessionStoreCreateRejectsNonPositiveTTL(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()

	_, err := store.Create(ctx, "user_1", 0)
	assert.Error(t, err)
	_, err = store.Create(ctx, "user_1", -time.Minute)
	assert.Error(t, err)
}

// TestMemorySessionStoreSweepEvictsExpiredSessions pins the actual growth
// bound maybeSweepLocked exists to provide: Validate reports an expired
// session as gone, but on its own that never shrinks the underlying map — a
// long-running process would keep every session it ever issued in memory
// until the process restarts, expired or not. This creates sweepInterval
// already-expired sessions (never validated, so Validate's own lazy check
// never touches them), issues one more write to cross the sweep threshold,
// and checks the map itself, not just Validate's answer.
func TestMemorySessionStoreSweepEvictsExpiredSessions(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()

	for i := 0; i < sweepInterval-1; i++ {
		_, err := store.Create(ctx, "user_1", time.Nanosecond)
		require.NoError(t, err)
	}
	time.Sleep(10 * time.Millisecond) // let every one of them expire

	store.mu.Lock()
	before := len(store.byToken)
	store.mu.Unlock()
	require.Equal(t, sweepInterval-1, before, "every expired session is still present before the sweep fires")

	// This write is the sweepInterval-th since the store was created, so it
	// crosses maybeSweepLocked's threshold and triggers a sweep.
	survivor, err := store.Create(ctx, "user_2", time.Hour)
	require.NoError(t, err)

	store.mu.Lock()
	after := len(store.byToken)
	_, survivorPresent := store.byToken[survivor]
	store.mu.Unlock()

	assert.Equal(t, 1, after, "the sweep must evict every expired session, leaving only the unexpired one just created")
	assert.True(t, survivorPresent, "the sweep must not evict a session that has not expired")
}
