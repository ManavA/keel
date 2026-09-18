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
		v, err, _ := sf.Do(context.Background(), "k", func(context.Context) (any, error) {
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
				v, err, _ := sf.Do(context.Background(), "shared-key", func(context.Context) (any, error) {
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
}
