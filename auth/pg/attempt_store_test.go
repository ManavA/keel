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

// TestAttemptStoreRecordAndCount is the Postgres round trip for the
// login-attempt audit trail: failures count inside the window, successes and
// other addresses do not, and Recent comes back newest first.
func TestAttemptStoreRecordAndCount(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewAttemptStore(pool)
	ctx := context.Background()
	now := time.Now()

	email := fmt.Sprintf("attempts-%d@example.com", testCounter.next())
	require.NoError(t, store.Record(ctx, auth.LoginAttempt{Email: email, Success: false, IP: "10.0.0.1", At: now.Add(-time.Hour)}))
	require.NoError(t, store.Record(ctx, auth.LoginAttempt{Email: email, Success: false, IP: "10.0.0.2", At: now.Add(-time.Minute)}))
	require.NoError(t, store.Record(ctx, auth.LoginAttempt{Email: email, Success: true, IP: "10.0.0.3", At: now}))

	n, err := store.FailuresSince(ctx, email, now.Add(-30*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only failures inside the window count")

	other, err := store.FailuresSince(ctx, fmt.Sprintf("other-%d@example.com", testCounter.next()), now.Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, other)

	recent, err := store.Recent(ctx, email, 2)
	require.NoError(t, err)
	require.Len(t, recent, 2)
	assert.True(t, recent[0].Success, "the newest attempt was the success")
	assert.Equal(t, "10.0.0.3", recent[0].IP)
	assert.True(t, recent[0].At.After(recent[1].At) || recent[0].At.Equal(recent[1].At),
		"recent attempts come back newest first")
}

// TestAttemptStoreRecordsUnknownAddresses pins that the trail keeps attempts
// against addresses with no account — without a foreign key to auth_users,
// which is what makes that insert possible at all.
func TestAttemptStoreRecordsUnknownAddresses(t *testing.T) {
	pool := newTestPool(t)
	store := authpg.NewAttemptStore(pool)
	ctx := context.Background()

	email := fmt.Sprintf("ghost-%d@example.com", testCounter.next())
	require.NoError(t, store.Record(ctx, auth.LoginAttempt{Email: email, IP: "10.0.0.9"}))

	n, err := store.FailuresSince(ctx, email, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}
