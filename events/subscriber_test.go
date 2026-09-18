package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInMemoryBus_PublishIsDeliveredToSubscriber(t *testing.T) {
	bus := NewInMemoryBus(InMemoryBusOptions{})
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

	require.NoError(t, bus.Publish(context.Background(), "orders", sampleEvent{Name: testWidgetName, N: 3}))

	select {
	case got := <-received:
		assert.Equal(t, sampleEvent{Name: testWidgetName, N: 3}, got)
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never received the published message")
	}
}

func TestInMemoryBus_PublishBeforeSubscribeIsNotQueued(t *testing.T) {
	bus := NewInMemoryBus(InMemoryBusOptions{})

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
	bus := NewInMemoryBus(InMemoryBusOptions{})
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
	bus := NewInMemoryBus(InMemoryBusOptions{})
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
	bus := NewInMemoryBus(InMemoryBusOptions{})
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

// TestInMemoryBus_FullBufferDropsAreLoud is the regression test for the
// independent review's probe: publishing past a subscriber whose handler
// is blocked dropped 83 of 100 messages with no log line and no counter.
// A drop must be visible both in the log and in DroppedCount.
func TestInMemoryBus_FullBufferDropsAreLoud(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	bus := NewInMemoryBus(InMemoryBusOptions{Logger: logger})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocked := make(chan struct{})
	go func() {
		_ = bus.Subscribe(ctx, "topic", func(context.Context, []byte) error {
			<-blocked // hold the handler open so the subscriber's buffer fills
			return nil
		})
	}()
	waitForSubscriber(t, bus, "topic")

	for i := 0; i < 100; i++ {
		require.NoError(t, bus.Publish(context.Background(), "topic", sampleEvent{Name: "x"}))
	}
	close(blocked)

	assert.Greater(t, bus.DroppedCount(), int64(0), "publishing past a full subscriber buffer must be counted")
	assert.Contains(t, buf.String(), "event dropped", "a drop must be logged, not silent")
}

// TestInMemoryBus_DropLoggingIsRateLimited is the regression test for the
// review's finding that a 5,000-message burst past a full buffer produced
// 4,983 Warn lines. DroppedCount must stay exact; the log must not.
func TestInMemoryBus_DropLoggingIsRateLimited(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, nil))
	bus := NewInMemoryBus(InMemoryBusOptions{Logger: logger, DropLogInterval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocked := make(chan struct{})
	go func() {
		_ = bus.Subscribe(ctx, "topic", func(context.Context, []byte) error {
			<-blocked
			return nil
		})
	}()
	waitForSubscriber(t, bus, "topic")

	const n = 500
	for i := 0; i < n; i++ {
		require.NoError(t, bus.Publish(context.Background(), "topic", sampleEvent{Name: "x"}))
	}
	close(blocked)

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	lines := strings.Count(out, "event dropped")

	assert.Equal(t, 1, lines, "a burst within one DropLogInterval must produce exactly one Warn line, not one per drop")
	// The one line logged fires on the FIRST drop in the window (so a burst
	// is never silent), before most of this burst's drops have happened —
	// DroppedCount is the exact, unthrottled total; the log line is not,
	// and is expected to undercount here by design.
	assert.Contains(t, out, "dropped_since_last_log=1 ", "the first drop in a window logs immediately")
	assert.Greater(t, bus.DroppedCount(), int64(1),
		"DroppedCount must keep counting every drop even while the log itself is rate-limited")
}

// TestInMemoryBus_PublishDoesNotBlockOnASlowLogWrite is the regression test
// for holding the subscriber-list lock across the drop log call: a publish
// to one topic that triggers a slow Warn write must not stall a concurrent
// publish to an unrelated topic.
func TestInMemoryBus_PublishDoesNotBlockOnASlowLogWrite(t *testing.T) {
	release := make(chan struct{})
	logger := slog.New(&blockingHandler{release: release})
	bus := NewInMemoryBus(InMemoryBusOptions{Logger: logger})

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	blockedA := make(chan struct{})
	go func() {
		_ = bus.Subscribe(ctxA, "A", func(context.Context, []byte) error {
			<-blockedA
			return nil
		})
	}()
	waitForSubscriber(t, bus, "A")

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	receivedB := make(chan struct{}, 1)
	go func() {
		_ = bus.Subscribe(ctxB, "B", func(context.Context, []byte) error {
			receivedB <- struct{}{}
			return nil
		})
	}()
	waitForSubscriber(t, bus, "B")

	// Fill topic A's buffer, then one more publish to trigger a drop; the
	// resulting log write blocks on blockingHandler until release closes.
	for i := 0; i < 17; i++ {
		require.NoError(t, bus.Publish(context.Background(), "A", sampleEvent{Name: "x"}))
	}
	go func() {
		_ = bus.Publish(context.Background(), "A", sampleEvent{Name: "one-more"})
	}()

	// The publish to B must complete quickly even while A's drop log is
	// stuck, because it no longer shares a lock with the logging call.
	done := make(chan struct{})
	go func() {
		require.NoError(t, bus.Publish(context.Background(), "B", sampleEvent{Name: "y"}))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish to an unrelated topic blocked on another topic's slow log write")
	}

	close(blockedA)
	close(release)
}

// blockingHandler is a slog.Handler whose Handle call blocks until release
// is closed, used to simulate a slow log sink.
type blockingHandler struct {
	release chan struct{}
}

func (h *blockingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *blockingHandler) Handle(context.Context, slog.Record) error {
	<-h.release
	return nil
}
func (h *blockingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockingHandler) WithGroup(string) slog.Handler      { return h }

// syncWriter serializes writes from concurrent goroutines onto a shared
// buffer, so the test can read it without a data race.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
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
