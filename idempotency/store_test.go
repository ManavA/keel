package idempotency_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/idempotency"
)

func TestMemoryStore_ClaimThenComplete(t *testing.T) {
	s := idempotency.NewMemoryStore()
	ctx := context.Background()

	claimID, rec, err := s.Claim(ctx, "k", "hash-a", time.Hour)
	require.NoError(t, err)
	assert.NotEmpty(t, claimID)
	assert.Nil(t, rec)

	require.NoError(t, s.Complete(ctx, "k", claimID, idempotency.Record{
		RequestHash: "hash-a",
		Status:      http.StatusOK,
		Header:      http.Header{"X-Test": {"1"}},
		Body:        []byte("ok"),
		ExpiresAt:   time.Now().Add(time.Hour),
	}))

	claimID, rec, err = s.Claim(ctx, "k", "hash-a", time.Hour)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Empty(t, claimID)
	assert.Equal(t, "ok", string(rec.Body))
	assert.Equal(t, "hash-a", rec.RequestHash)
}

func TestMemoryStore_ClaimTwiceReturnsInProgress(t *testing.T) {
	s := idempotency.NewMemoryStore()
	ctx := context.Background()

	_, _, err := s.Claim(ctx, "k", "hash-a", time.Hour)
	require.NoError(t, err)

	_, rec, err := s.Claim(ctx, "k", "hash-b", time.Hour)
	assert.ErrorIs(t, err, idempotency.ErrInProgress)
	assert.Nil(t, rec)
}

func TestMemoryStore_ReleaseAllowsReclaim(t *testing.T) {
	s := idempotency.NewMemoryStore()
	ctx := context.Background()

	claimID, _, err := s.Claim(ctx, "k", "hash-a", time.Hour)
	require.NoError(t, err)
	require.NoError(t, s.Release(ctx, "k", claimID))

	_, rec, err := s.Claim(ctx, "k", "hash-b", time.Hour)
	require.NoError(t, err)
	assert.Nil(t, rec, "a released key must be claimable again")
}

func TestMemoryStore_ReleaseLeavesACompletedRecord(t *testing.T) {
	s := idempotency.NewMemoryStore()
	ctx := context.Background()

	claimID, _, err := s.Claim(ctx, "k", "hash-a", time.Hour)
	require.NoError(t, err)
	require.NoError(t, s.Complete(ctx, "k", claimID, idempotency.Record{
		RequestHash: "hash-a", Body: []byte("kept"), ExpiresAt: time.Now().Add(time.Hour),
	}))
	require.NoError(t, s.Release(ctx, "k", claimID))

	_, rec, err := s.Claim(ctx, "k", "hash-a", time.Hour)
	require.NoError(t, err)
	require.NotNil(t, rec, "Release must not delete a completed record")
	assert.Equal(t, "kept", string(rec.Body))
}

func TestMemoryStore_CompleteWithoutClaimErrors(t *testing.T) {
	s := idempotency.NewMemoryStore()
	err := s.Complete(context.Background(), "never-claimed", "no-such-claim", idempotency.Record{})
	assert.ErrorIs(t, err, idempotency.ErrClaimLost)
}

// An abandoned claim, never completed or released, must become reclaimable
// when its lease passes and not before.
func TestMemoryStore_AbandonedClaimIsReclaimableAfterItsLease(t *testing.T) {
	s := idempotency.NewMemoryStore()
	ctx := context.Background()

	_, _, err := s.Claim(ctx, "k", "hash-a", 30*time.Millisecond)
	require.NoError(t, err)

	_, _, err = s.Claim(ctx, "k", "hash-b", time.Hour)
	require.ErrorIs(t, err, idempotency.ErrInProgress)

	time.Sleep(40 * time.Millisecond)

	claimID, rec, err := s.Claim(ctx, "k", "hash-c", time.Hour)
	require.NoError(t, err)
	assert.NotEmpty(t, claimID)
	assert.Nil(t, rec)
}

// A request that outlives its lease loses the key to a newer request. Its
// late Complete and Release must be rejected, so the stored record comes
// whole from the request that holds the key.
func TestMemoryStore_LateCompleteFromAStolenClaimIsRejected(t *testing.T) {
	s := idempotency.NewMemoryStore()
	ctx := context.Background()

	slow, _, err := s.Claim(ctx, "k", "hash-a", 10*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)

	thief, _, err := s.Claim(ctx, "k", "hash-b", time.Hour)
	require.NoError(t, err)
	require.NotEqual(t, slow, thief)

	err = s.Complete(ctx, "k", slow, idempotency.Record{
		RequestHash: "hash-a", Body: []byte("from a"), ExpiresAt: time.Now().Add(time.Hour),
	})
	require.ErrorIs(t, err, idempotency.ErrClaimLost)

	require.NoError(t, s.Release(ctx, "k", slow))
	_, _, err = s.Claim(ctx, "k", "hash-c", time.Hour)
	require.ErrorIs(t, err, idempotency.ErrInProgress, "a stale Release must not free the newer claim")

	require.NoError(t, s.Complete(ctx, "k", thief, idempotency.Record{
		RequestHash: "hash-b", Body: []byte("from b"), ExpiresAt: time.Now().Add(time.Hour),
	}))
	_, rec, err := s.Claim(ctx, "k", "hash-b", time.Hour)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "hash-b", rec.RequestHash)
	assert.Equal(t, "from b", string(rec.Body))
}

func TestMemoryStore_ExpiredRecordIsReclaimed(t *testing.T) {
	s := idempotency.NewMemoryStore()
	ctx := context.Background()

	claimID, _, err := s.Claim(ctx, "k", "hash-a", time.Hour)
	require.NoError(t, err)
	require.NoError(t, s.Complete(ctx, "k", claimID, idempotency.Record{
		RequestHash: "hash-a",
		ExpiresAt:   time.Now().Add(-time.Second),
	}))

	_, rec, err := s.Claim(ctx, "k", "hash-b", time.Hour)
	require.NoError(t, err)
	assert.Nil(t, rec, "an expired record must not be replayed")
}

// Expired entries must drain as new keys are claimed, and one Claim must do
// a bounded share of that work, not walk the whole map.
func TestMemoryStore_EvictsExpiredEntriesALittlePerClaim(t *testing.T) {
	s := idempotency.NewMemoryStore()
	ctx := context.Background()

	const expired = 2000
	const oldLease = 300 * time.Millisecond
	for i := range expired {
		_, _, err := s.Claim(ctx, fmt.Sprintf("old-%d", i), "h", oldLease)
		require.NoError(t, err)
	}
	require.Equal(t, expired, s.Len())
	time.Sleep(oldLease + 10*time.Millisecond)

	_, _, err := s.Claim(ctx, "fresh-first", "h", time.Hour)
	require.NoError(t, err)
	assert.Greater(t, s.Len(), expired-100, "one Claim must evict only a bounded number of entries")

	const fresh = 1000
	for i := range fresh {
		_, _, err := s.Claim(ctx, fmt.Sprintf("fresh-%d", i), "h", time.Hour)
		require.NoError(t, err)
	}
	assert.Less(t, s.Len(), fresh+1+expired/4, "most expired entries must be gone after many claims")
}
