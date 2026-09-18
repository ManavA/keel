package geocode

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeClock lets a test control "now" and observe every requested sleep
// duration without a real wait, keeping the rate-limit assertions exact and
// the test itself instant.
type fakeClock struct {
	now        time.Time
	sleptFor   []time.Duration
	sleepFires bool // whether the returned channel ever sends
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(d time.Duration) <-chan time.Time {
	c.sleptFor = append(c.sleptFor, d)
	ch := make(chan time.Time, 1)
	if c.sleepFires {
		ch <- c.now
	}
	return ch
}

func TestRateLimited_Geocode(t *testing.T) {
	ctx := context.Background()

	t.Run("the first call never waits", func(t *testing.T) {
		clock := &fakeClock{now: time.Unix(0, 0), sleepFires: true}
		provider := &fakeProvider{coords: &Coordinates{Latitude: 1}}
		rl := NewRateLimited(provider, time.Second, RateLimitedOptions{now: clock.Now, sleep: clock.Sleep})

		_, err := rl.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)
		require.Empty(t, clock.sleptFor, "no prior call means nothing to wait on")
	})

	t.Run("a second call inside the interval waits the remainder", func(t *testing.T) {
		clock := &fakeClock{now: time.Unix(0, 0), sleepFires: true}
		provider := &fakeProvider{coords: &Coordinates{Latitude: 1}}
		rl := NewRateLimited(provider, time.Second, RateLimitedOptions{now: clock.Now, sleep: clock.Sleep})

		_, err := rl.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)

		clock.now = clock.now.Add(300 * time.Millisecond) // still inside the 1s window
		_, err = rl.Geocode(ctx, "2 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)

		require.Len(t, clock.sleptFor, 1)
		require.Equal(t, 700*time.Millisecond, clock.sleptFor[0])
	})

	t.Run("a call after the interval has fully elapsed does not wait", func(t *testing.T) {
		clock := &fakeClock{now: time.Unix(0, 0), sleepFires: true}
		provider := &fakeProvider{coords: &Coordinates{Latitude: 1}}
		rl := NewRateLimited(provider, time.Second, RateLimitedOptions{now: clock.Now, sleep: clock.Sleep})

		_, err := rl.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)

		clock.now = clock.now.Add(2 * time.Second)
		_, err = rl.Geocode(ctx, "2 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)

		require.Empty(t, clock.sleptFor, "the interval had already fully elapsed")
	})

	t.Run("a cancelled context stops the wait instead of blocking forever", func(t *testing.T) {
		clock := &fakeClock{now: time.Unix(0, 0), sleepFires: false} // the sleep channel never fires
		provider := &fakeProvider{coords: &Coordinates{Latitude: 1}}
		rl := NewRateLimited(provider, time.Second, RateLimitedOptions{now: clock.Now, sleep: clock.Sleep})

		_, err := rl.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)

		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		_, err = rl.Geocode(cancelCtx, "2 Main St", "Oakland", "CA", "94601")
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, provider.calls, "the underlying provider must not be called once the wait is cancelled")
	})
}
