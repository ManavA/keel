package events

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInMemoryBus_PublishIsDeliveredToSubscriber(t *testing.T) {
	bus := NewInMemoryBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan sampleEvent, 1)
	go func() {
		_ = bus.Subscribe(ctx, "orders", func(_ context.Context, data []byte) error {
			var e sampleEvent
			if err := json.Unmarshal(data, &e); err != nil {
				return err
			}
			received <- e
			return nil
		})
	}()

	// Give Subscribe a moment to register before publishing — InMemoryBus
	// does not queue for a subscriber that has not joined yet.
	waitForSubscriber(t, bus, "orders")

	require.NoError(t, bus.Publish(context.Background(), "orders", sampleEvent{Name: "widget", N: 3}))

	select {
	case got := <-received:
		assert.Equal(t, sampleEvent{Name: "widget", N: 3}, got)
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never received the published message")
	}
}

func TestInMemoryBus_PublishBeforeSubscribeIsNotQueued(t *testing.T) {
	bus := NewInMemoryBus()

	// Nobody is subscribed yet; this must not panic or block.
	require.NoError(t, bus.Publish(context.Background(), "orders", sampleEvent{Name: "early"}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	received := make(chan struct{}, 1)
	_ = bus.Subscribe(ctx, "orders", func(context.Context, []byte) error {
		received <- struct{}{}
		return nil
	})

	select {
	case <-received:
		t.Fatal("a message published before any subscriber joined must not be delivered later")
	default:
	}
}

func TestInMemoryBus_MultipleSubscribersEachReceive(t *testing.T) {
	bus := NewInMemoryBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var gotA, gotB bool

	go func() {
		_ = bus.Subscribe(ctx, "fanout", func(context.Context, []byte) error {
			mu.Lock()
			gotA = true
			mu.Unlock()
			return nil
		})
	}()
	go func() {
		_ = bus.Subscribe(ctx, "fanout", func(context.Context, []byte) error {
			mu.Lock()
			gotB = true
			mu.Unlock()
			return nil
		})
	}()

	waitForSubscriberCount(t, bus, "fanout", 2)
	require.NoError(t, bus.Publish(context.Background(), "fanout", sampleEvent{Name: "x"}))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotA && gotB
	}, 2*time.Second, 5*time.Millisecond, "both subscribers on the same topic must receive the message")
}

func TestInMemoryBus_SubscribeReturnsContextErrOnCancel(t *testing.T) {
	bus := NewInMemoryBus()
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Subscribe(ctx, "topic", func(context.Context, []byte) error { return nil })
	}()

	waitForSubscriber(t, bus, "topic")
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return after context cancellation")
	}
}

func TestInMemoryBus_HandlerErrorDoesNotStopTheLoop(t *testing.T) {
	bus := NewInMemoryBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	var mu sync.Mutex
	go func() {
		_ = bus.Subscribe(ctx, "topic", func(context.Context, []byte) error {
			mu.Lock()
			calls++
			mu.Unlock()
			return errors.New("handler chose to fail")
		})
	}()

	waitForSubscriber(t, bus, "topic")
	require.NoError(t, bus.Publish(context.Background(), "topic", sampleEvent{Name: "1"}))
	require.NoError(t, bus.Publish(context.Background(), "topic", sampleEvent{Name: "2"}))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 2
	}, 2*time.Second, 5*time.Millisecond, "a handler error on one message must not stop later messages from being delivered")
}

func waitForSubscriber(t *testing.T, bus *InMemoryBus, topic string) {
	t.Helper()
	waitForSubscriberCount(t, bus, topic, 1)
}

func waitForSubscriberCount(t *testing.T, bus *InMemoryBus, topic string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		bus.mu.Lock()
		count := len(bus.subs[topic])
		bus.mu.Unlock()
		if count >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d subscriber(s) on %q", n, topic)
}
