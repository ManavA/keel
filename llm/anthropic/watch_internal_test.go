package anthropic

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closed reports whether ch is closed, without waiting for it.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// When close returns, the watch's goroutine has returned too. Stream defers
// close, so this is what keeps a goroutine from outliving a Stream.
func TestIdleWatch_CloseWaitsForItsGoroutine(t *testing.T) {
	for range 2000 {
		w := watchIdle(time.Minute, func() {})
		w.begin(time.Minute)
		w.end()
		w.close()
		require.True(t, closed(w.gone), "close returned while the watch's goroutine was still running")
	}
}

func TestIdleWatch_Expiry(t *testing.T) {
	const limit = 60 * time.Millisecond

	t.Run("a wait that outlasts the limit ends the stream once", func(t *testing.T) {
		var expired atomic.Int64
		w := watchIdle(limit, func() { expired.Add(1) })
		defer w.close()

		w.begin(limit)
		require.Eventually(t, func() bool { return expired.Load() == 1 }, 5*time.Second, time.Millisecond)
		assert.True(t, w.end())
		// The goroutine went when it gave the stream up.
		require.Eventually(t, func() bool { return closed(w.gone) }, 5*time.Second, time.Millisecond)
		assert.Equal(t, int64(1), expired.Load())
	})

	t.Run("bytes from the server start the wait's time again", func(t *testing.T) {
		// A limit long enough that a busy machine losing half a second
		// between two bytes does not look like silence.
		const patient = 750 * time.Millisecond
		var expired atomic.Int64
		w := watchIdle(patient, func() { expired.Add(1) })
		defer w.close()

		w.begin(patient)
		// Twice the limit passes, and never the limit without a byte.
		for range 100 {
			time.Sleep(patient / 50)
			w.touch()
		}
		assert.False(t, w.end())
		assert.Zero(t, expired.Load())
	})

	t.Run("the time between waits is not watched", func(t *testing.T) {
		var expired atomic.Int64
		w := watchIdle(limit, func() { expired.Add(1) })
		defer w.close()

		for range 3 {
			w.begin(limit)
			assert.False(t, w.end())
			// The caller's fn, taking its time.
			time.Sleep(3 * limit)
		}
		assert.Zero(t, expired.Load())
	})

	t.Run("a watch with no limit never gives up, and a shorter wait still can", func(t *testing.T) {
		var expired atomic.Int64
		w := watchIdle(0, func() { expired.Add(1) })
		defer w.close()

		w.begin(0)
		time.Sleep(3 * limit)
		assert.False(t, w.end())
		assert.Zero(t, expired.Load())

		// The drain's wait, on a watch that has been idle with no timer.
		w.begin(limit)
		require.Eventually(t, func() bool { return expired.Load() == 1 }, 5*time.Second, time.Millisecond)
		assert.True(t, w.end())
	})

	t.Run("a wait shorter than the one the watch is set for is still kept", func(t *testing.T) {
		var expired atomic.Int64
		w := watchIdle(time.Hour, func() { expired.Add(1) })
		defer w.close()

		w.begin(time.Hour)
		assert.False(t, w.end())
		started := time.Now()
		w.begin(limit)
		require.Eventually(t, func() bool { return expired.Load() == 1 }, 5*time.Second, time.Millisecond)
		assert.Less(t, time.Since(started), 5*time.Second)
	})
}

func TestOrigin(t *testing.T) {
	tests := []struct {
		a, b string
		same bool
	}{
		{a: "https://api.keel.test", b: "https://api.keel.test:443/v1/messages", same: true},
		{a: "http://api.keel.test", b: "http://API.KEEL.TEST:80", same: true},
		{a: "HTTPS://api.keel.test", b: "https://api.keel.test", same: true},
		{a: "https://api.keel.test", b: "https://api.keel.test:80"},
		{a: "https://api.keel.test", b: "http://api.keel.test"},
		{a: "https://api.keel.test", b: "http://api.keel.test:443"},
		{a: "https://api.keel.test", b: "https://api.keel.test.other.test"},
		{a: "http://[::1]:8080", b: "http://[::1]:8081"},
		{a: "http://[::1]:8080", b: "http://[::1]:8080/x", same: true},
	}
	for _, tt := range tests {
		t.Run(tt.a+" and "+tt.b, func(t *testing.T) {
			ca, err := New(Options{APIKey: "test-key", BaseURL: tt.a})
			require.NoError(t, err)
			cb, err := New(Options{APIKey: "test-key", BaseURL: tt.b})
			require.NoError(t, err)
			assert.Equal(t, tt.same, origin(ca.base) == origin(cb.base), "%s, %s", origin(ca.base), origin(cb.base))
		})
	}
}
