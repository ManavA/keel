package pg_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/idempotency"
	idempg "github.com/ManavA/keel/idempotency/pg"
	"github.com/ManavA/keel/log"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// openStore gives a test its own Store over the package's shared database,
// applying the migration first. The migration is idempotent (CREATE TABLE
// IF NOT EXISTS), so running it once per test is cheap and safe against a
// table other tests are also using.
func openStore(t *testing.T) *idempg.Store {
	t.Helper()
	db := testdb.Shared(t)

	pool, err := keelpg.Open(context.Background(), keelpg.Options{URL: db.URL, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	migration, err := os.ReadFile("migrations/001_idempotency_keys.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), string(migration))
	require.NoError(t, err)

	return idempg.New(pool)
}

// uniqueKey gives each test its own row in the shared table.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

func TestStore_ClaimThenComplete(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	key := uniqueKey(t)

	claimID, rec, err := store.Claim(ctx, key, "hash-a", time.Hour)
	require.NoError(t, err)
	assert.NotEmpty(t, claimID)
	assert.Nil(t, rec)

	require.NoError(t, store.Complete(ctx, key, claimID, idempotency.Record{
		RequestHash: "hash-a",
		Status:      http.StatusCreated,
		Header:      http.Header{"X-Test": {"1"}},
		Body:        []byte("stored body"),
		ExpiresAt:   time.Now().Add(time.Hour),
	}))

	claimID, rec, err = store.Claim(ctx, key, "hash-a", time.Hour)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Empty(t, claimID)
	assert.Equal(t, http.StatusCreated, rec.Status)
	assert.Equal(t, "stored body", string(rec.Body))
	assert.Equal(t, []string{"1"}, rec.Header["X-Test"])
	assert.Equal(t, "hash-a", rec.RequestHash)
}

func TestStore_ClaimTwiceReturnsInProgress(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	key := uniqueKey(t)

	_, _, err := store.Claim(ctx, key, "hash-a", time.Hour)
	require.NoError(t, err)

	_, rec, err := store.Claim(ctx, key, "hash-b", time.Hour)
	assert.ErrorIs(t, err, idempotency.ErrInProgress)
	assert.Nil(t, rec)
}

func TestStore_ReleaseAllowsReclaim(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	key := uniqueKey(t)

	claimID, _, err := store.Claim(ctx, key, "hash-a", time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.Release(ctx, key, claimID))

	_, rec, err := store.Claim(ctx, key, "hash-b", time.Hour)
	require.NoError(t, err)
	assert.Nil(t, rec, "a released key must be claimable again")
}

func TestStore_ReleaseLeavesACompletedRecord(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	key := uniqueKey(t)

	claimID, _, err := store.Claim(ctx, key, "hash-a", time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.Complete(ctx, key, claimID, idempotency.Record{
		RequestHash: "hash-a", Body: []byte("kept"), ExpiresAt: time.Now().Add(time.Hour),
	}))
	require.NoError(t, store.Release(ctx, key, claimID))

	_, rec, err := store.Claim(ctx, key, "hash-a", time.Hour)
	require.NoError(t, err)
	require.NotNil(t, rec, "Release must not delete a completed record")
	assert.Equal(t, "kept", string(rec.Body))
}

func TestStore_ExpiredRecordIsReclaimed(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	key := uniqueKey(t)

	claimID, _, err := store.Claim(ctx, key, "hash-a", time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.Complete(ctx, key, claimID, idempotency.Record{
		RequestHash: "hash-a",
		ExpiresAt:   time.Now().Add(-time.Second),
	}))

	_, rec, err := store.Claim(ctx, key, "hash-b", time.Hour)
	require.NoError(t, err)
	assert.Nil(t, rec, "an expired record must not be replayed")
}

func TestStore_CompleteWithoutClaimErrors(t *testing.T) {
	store := openStore(t)
	err := store.Complete(context.Background(), uniqueKey(t), "no-such-claim", idempotency.Record{})
	assert.ErrorIs(t, err, idempotency.ErrClaimLost)
}

// An abandoned claim, never completed or released, must become reclaimable
// when its lease passes and not before.
func TestStore_AbandonedClaimIsReclaimableAfterItsLease(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	key := uniqueKey(t)

	_, _, err := store.Claim(ctx, key, "hash-a", 300*time.Millisecond)
	require.NoError(t, err)

	_, _, err = store.Claim(ctx, key, "hash-b", time.Hour)
	require.ErrorIs(t, err, idempotency.ErrInProgress)

	require.Eventually(t, func() bool {
		claimID, rec, err := store.Claim(ctx, key, "hash-c", time.Hour)
		return err == nil && rec == nil && claimID != ""
	}, 3*time.Second, 20*time.Millisecond, "the claim must become reclaimable once its lease expires")
}

// A request that outlives its lease loses the key to a newer request. Its
// late Complete and Release must be rejected, so the stored record comes
// whole from the request that holds the key.
func TestStore_LateCompleteFromAStolenClaimIsRejected(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	key := uniqueKey(t)

	slow, _, err := store.Claim(ctx, key, "hash-a", 50*time.Millisecond)
	require.NoError(t, err)

	var thief string
	require.Eventually(t, func() bool {
		id, rec, err := store.Claim(ctx, key, "hash-b", time.Hour)
		thief = id
		return err == nil && rec == nil
	}, 3*time.Second, 20*time.Millisecond)
	require.NotEqual(t, slow, thief)

	err = store.Complete(ctx, key, slow, idempotency.Record{
		RequestHash: "hash-a", Body: []byte("from a"), ExpiresAt: time.Now().Add(time.Hour),
	})
	require.ErrorIs(t, err, idempotency.ErrClaimLost)

	require.NoError(t, store.Release(ctx, key, slow))
	_, _, err = store.Claim(ctx, key, "hash-c", time.Hour)
	require.ErrorIs(t, err, idempotency.ErrInProgress, "a stale Release must not free the newer claim")

	require.NoError(t, store.Complete(ctx, key, thief, idempotency.Record{
		RequestHash: "hash-b", Body: []byte("from b"), ExpiresAt: time.Now().Add(time.Hour),
	}))
	_, rec, err := store.Claim(ctx, key, "hash-b", time.Hour)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "hash-b", rec.RequestHash)
	assert.Equal(t, "from b", string(rec.Body))
}

// TestStore_ConcurrentClaimOnlyOneWins races two goroutines claiming the
// same key against real Postgres, under -race. The claim SQL is a single
// INSERT ... ON CONFLICT statement, so Postgres itself serializes the two
// attempts against the same row: exactly one must see (nil, false, nil)
// and the other ErrInProgress.
func TestStore_ConcurrentClaimOnlyOneWins(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	key := uniqueKey(t)

	const n = 8
	var wg sync.WaitGroup
	wins := make([]bool, n)
	errs := make([]error, n)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimID, _, err := store.Claim(ctx, key, "hash-a", time.Hour)
			wins[i] = err == nil && claimID != ""
			errs[i] = err
		}(i)
	}
	wg.Wait()

	winCount := 0
	for i, w := range wins {
		if w {
			winCount++
			continue
		}
		assert.ErrorIs(t, errs[i], idempotency.ErrInProgress, "a losing claim must report ErrInProgress")
	}
	assert.Equal(t, 1, winCount, "exactly one of %d concurrent claims for the same key must win", n)
}
