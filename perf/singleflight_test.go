package perf

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSingleFlight_Do(t *testing.T) {
	t.Run("the zero value works with no constructor", func(t *testing.T) {
		var sf SingleFlight
		v, _, err := sf.Do(context.Background(), "k", func(context.Context) (any, error) {
			return "result", nil
		})
		require.NoError(t, err)
		require.Equal(t, "result", v)
	})

	t.Run("concurrent calls for the same key collapse into one underlying call", func(t *testing.T) {
		var sf SingleFlight
		var calls int64
		release := make(chan struct{})

		const n = 20
		var wg sync.WaitGroup
		results := make([]any, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				v, _, err := sf.Do(context.Background(), "shared-key", func(context.Context) (any, error) {
					atomic.AddInt64(&calls, 1)
					<-release // hold every concurrent caller here until they have all arrived
					return "value", nil
				})
				require.NoError(t, err)
				results[i] = v
			}(i)
		}

		// Give the goroutines a moment to all reach Do and block on release —
		// this is what proves they were actually concurrent, not serialized.
		time.Sleep(50 * time.Millisecond)
		close(release)
		wg.Wait()

		require.Equal(t, int64(1), atomic.LoadInt64(&calls), "only one call should have reached fn")
		for _, v := range results {
			require.Equal(t, "value", v)
		}
	})

	t.Run("different keys do not collapse into each other", func(t *testing.T) {
		var sf SingleFlight
		var calls int64
		fn := func(context.Context) (any, error) {
			atomic.AddInt64(&calls, 1)
			return nil, nil
		}
		_, _, _ = sf.Do(context.Background(), "a", fn)
		_, _, _ = sf.Do(context.Background(), "b", fn)
		require.Equal(t, int64(2), atomic.LoadInt64(&calls))
	})

	t.Run("Forget makes the next Do start a fresh call instead of reusing a finished one", func(t *testing.T) {
		var sf SingleFlight
		var calls int64
		fn := func(context.Context) (any, error) {
			return atomic.AddInt64(&calls, 1), nil
		}

		v1, _, _ := sf.Do(context.Background(), "k", fn)
		sf.Forget("k")
		v2, _, _ := sf.Do(context.Background(), "k", fn)

		require.NotEqual(t, v1, v2)
	})

	t.Run("a follower whose own context is cancelled returns immediately with its own ctx.Err()", func(t *testing.T) {
		var sf SingleFlight
		release := make(chan struct{})
		leaderStarted := make(chan struct{})

		leaderDone := make(chan struct{})
		go func() {
			defer close(leaderDone)
			_, _, _ = sf.Do(context.Background(), "shared-key", func(context.Context) (any, error) {
				close(leaderStarted)
				<-release
				return "leader-result", nil
			})
		}()

		<-leaderStarted // the leader's fn is now running and holding release

		followerCtx, cancel := context.WithCancel(context.Background())
		cancel() // already cancelled before Do is even called

		done := make(chan struct{})
		var followerErr error
		go func() {
			defer close(done)
			_, _, followerErr = sf.Do(followerCtx, "shared-key", func(context.Context) (any, error) {
				t.Error("a follower must never re-run fn")
				return nil, nil
			})
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("a follower with an already-cancelled context must not block waiting for the leader")
		}
		require.ErrorIs(t, followerErr, context.Canceled)

		close(release)
		<-leaderDone
	})

	t.Run("a cancelled caller does not affect the shared call for other waiters", func(t *testing.T) {
		var sf SingleFlight
		release := make(chan struct{})
		started := make(chan struct{})

		cancelledCtx, cancel := context.WithCancel(context.Background())

		cancelledDone := make(chan struct{})
		var cancelledErr error
		go func() {
			defer close(cancelledDone)
			_, _, cancelledErr = sf.Do(cancelledCtx, "shared-key", func(context.Context) (any, error) {
				close(started)
				<-release
				return "value", nil
			})
		}()

		<-started
		cancel() // cancel the caller whose context is backing the in-flight call
		<-cancelledDone
		require.ErrorIs(t, cancelledErr, context.Canceled)

		// A second, live caller must still get the real result once the
		// shared call finishes — the first caller's cancellation must not
		// have poisoned or aborted the underlying work.
		liveDone := make(chan struct{})
		var liveVal any
		var liveErr error
		go func() {
			defer close(liveDone)
			liveVal, _, liveErr = sf.Do(context.Background(), "shared-key", func(context.Context) (any, error) {
				t.Error("a caller joining before the shared call finishes must not re-run fn")
				return nil, nil
			})
		}()

		// Give the live goroutine a moment to actually reach DoChan and join
		// the in-flight call before release is closed — otherwise this
		// goroutine might not have registered yet when fn returns, and it
		// would start a brand new (unwanted) call instead of joining.
		time.Sleep(20 * time.Millisecond)
		close(release)
		select {
		case <-liveDone:
		case <-time.After(2 * time.Second):
			t.Fatal("the live caller must not be blocked by the cancelled caller's departure")
		}
		require.NoError(t, liveErr)
		require.Equal(t, "value", liveVal)
	})

	t.Run("fn runs detached from the leader's context: the leader cancelling does not cancel fn", func(t *testing.T) {
		var sf SingleFlight
		leaderCtx, cancel := context.WithCancel(context.Background())

		release := make(chan struct{})
		fnStarted := make(chan struct{})
		fnCtxCancelled := make(chan bool, 1)
		go func() {
			_, _, _ = sf.Do(leaderCtx, "k", func(fnCtx context.Context) (any, error) {
				close(fnStarted)
				<-release
				select {
				case <-fnCtx.Done():
					fnCtxCancelled <- true
				default:
					fnCtxCancelled <- false
				}
				return nil, nil
			})
		}()

		<-fnStarted
		cancel()                          // cancel the leader's own context while fn is still running
		time.Sleep(20 * time.Millisecond) // give a wrongly-propagated cancellation a chance to land
		close(release)

		require.False(t, <-fnCtxCancelled, "fn's context must not be cancelled by the leader's own context cancellation")
	})

	t.Run("a panic in fn is recovered and returned as an error, not a process crash", func(t *testing.T) {
		var sf SingleFlight
		v, shared, err := sf.Do(context.Background(), "k", func(context.Context) (any, error) {
			panic("boom")
		})
		require.Nil(t, v)
		require.False(t, shared)
		var panicErr *ErrPanicked
		require.ErrorAs(t, err, &panicErr)
		require.Equal(t, "boom", panicErr.Value)
		require.NotEmpty(t, panicErr.Stack)
	})

	t.Run("a follower waiting on a call whose fn panics gets the same error, not a hang or a second panic", func(t *testing.T) {
		var sf SingleFlight
		release := make(chan struct{})
		started := make(chan struct{})

		leaderDone := make(chan struct{})
		var leaderErr error
		go func() {
			defer close(leaderDone)
			_, _, leaderErr = sf.Do(context.Background(), "shared-key", func(context.Context) (any, error) {
				close(started)
				<-release
				panic("boom")
			})
		}()
		<-started

		followerDone := make(chan struct{})
		var followerErr error
		go func() {
			defer close(followerDone)
			_, _, followerErr = sf.Do(context.Background(), "shared-key", func(context.Context) (any, error) {
				t.Error("a follower must never re-run fn")
				return nil, nil
			})
		}()

		// Give the follower a moment to actually join the in-flight call
		// before it is allowed to panic — the same reasoning as the
		// existing "does not affect other waiters" test above.
		time.Sleep(20 * time.Millisecond)
		close(release)

		select {
		case <-leaderDone:
		case <-time.After(2 * time.Second):
			t.Fatal("the leader must return the recovered panic, not hang or crash the process")
		}
		select {
		case <-followerDone:
		case <-time.After(2 * time.Second):
			t.Fatal("the follower must receive the shared panic result, not hang")
		}

		var leaderPanicErr, followerPanicErr *ErrPanicked
		require.ErrorAs(t, leaderErr, &leaderPanicErr)
		require.ErrorAs(t, followerErr, &followerPanicErr)
		require.Equal(t, "boom", leaderPanicErr.Value)
		require.Equal(t, "boom", followerPanicErr.Value)
	})
}
