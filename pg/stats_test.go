package pg_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

// TestStat_ReportsInUseRisingUnderConcurrentLoad is issue #39's acceptance
// check: while connections are held by concurrent acquirers, the reported
// in-use count rises to match, and falls back when they release.
func TestStat_ReportsInUseRisingUnderConcurrentLoad(t *testing.T) {
	pool := openPool(t) // MaxConns 4
	ctx := context.Background()

	base := pg.Stat(pool)
	assert.Zero(t, base.Acquired, "a fresh pool has nothing checked out")

	const held = 3
	release := make(chan struct{})
	acquired := make(chan struct{}, held)
	var wg sync.WaitGroup
	for range held {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Errorf("acquire under load: %v", err)
				return
			}
			defer conn.Release()
			acquired <- struct{}{}
			<-release
		}()
	}

	for range held {
		select {
		case <-acquired:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatal("timed out waiting for concurrent acquirers to hold connections")
		}
	}

	loaded := pg.Stat(pool)
	assert.Equal(t, int32(held), loaded.Acquired,
		"in-use count must rise to the number of held connections")
	assert.Equal(t, loaded.Total-loaded.Acquired, loaded.Idle,
		"idle plus in-use must account for every connection")
	assert.LessOrEqual(t, loaded.Total, loaded.Max,
		"the pool must never exceed its configured maximum")

	close(release)
	wg.Wait()

	assert.Zero(t, pg.Stat(pool).Acquired,
		"releasing every connection returns the in-use count to zero")
}

// TestStat_SurfacesWaitsAndTimeoutsOnAnExhaustedPool drives the two
// saturation counters through the paths that move them: a successful acquire
// that waited on an empty pool, and one that gave up waiting.
func TestStat_SurfacesWaitsAndTimeoutsOnAnExhaustedPool(t *testing.T) {
	db := testdb.Shared(t)

	pool, err := pg.Open(context.Background(), pg.Options{URL: db.URL, MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	first, err := pool.Acquire(ctx)
	require.NoError(t, err)
	second, err := pool.Acquire(ctx)
	require.NoError(t, err)

	before := pg.Stat(pool)

	// The pool is full, so this acquire waits until a connection is freed.
	// It succeeds, which is what the empty-wait counter counts.
	go func() {
		time.Sleep(100 * time.Millisecond)
		first.Release()
	}()
	waited, err := pool.Acquire(ctx)
	require.NoError(t, err, "a connection is freed while it waits")
	waited.Release()

	mid := pg.Stat(pool)
	assert.Greater(t, mid.EmptyAcquires, before.EmptyAcquires,
		"an acquire that waited on an empty pool and succeeded must be counted")

	// Full again, with nothing freed: this one must give up, which is what
	// the canceled counter counts. (A timed-out acquire is never counted as
	// an empty wait; only successful ones are.)
	refilled, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer refilled.Release()

	timeout, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err = pool.Acquire(timeout)
	require.Error(t, err, "no connection is freed, so the acquire must time out")

	after := pg.Stat(pool)
	assert.Greater(t, after.CanceledAcquires, mid.CanceledAcquires,
		"an acquire that timed out waiting must be counted")

	second.Release()
}

// TestStat_NilPoolIsZero documents that Stat is safe to call before the pool
// exists, reporting nothing rather than panicking.
func TestStat_NilPoolIsZero(t *testing.T) {
	assert.Equal(t, pg.Stats{}, pg.Stat(nil))
}
