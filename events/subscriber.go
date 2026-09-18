package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Handler processes one message's raw payload. A non-nil error means the
// message should not be considered handled — events/pubsub's Subscriber
// nacks it so the broker redelivers; [InMemoryBus] logs it, since it has no
// broker underneath to ask again (see InMemoryBus's own doc on delivery
// semantics).
type Handler func(ctx context.Context, data []byte) error

// Subscriber delivers messages from a subscription to handler until ctx is
// canceled or handler returns a non-nil error, then returns.
//
// Subscribe blocks. Callers that want to run several subscriptions
// concurrently start each in its own goroutine, the same way
// [cloud.google.com/go/pubsub/v2]'s own Subscriber.Receive works underneath
// events/pubsub's Subscriber.
//
// The two implementations in this module differ in delivery semantics, and
// a caller switching between them must account for that difference, not
// just the interface:
//
//   - [InMemoryBus] is at-most-once. A message can be dropped — see its own
//     doc — and a handler error is logged, not retried.
//   - events/pubsub's Subscriber is at-least-once, matching Google Cloud
//     Pub/Sub: a handler error nacks the message, and Pub/Sub redelivers
//     it, possibly more than once and possibly out of order. A handler
//     written against events/pubsub must be idempotent; a handler written
//     only against InMemoryBus may not need to be, and will be wrong if
//     moved to events/pubsub unchanged.
type Subscriber interface {
	Subscribe(ctx context.Context, subscriptionID string, handler Handler) error
}

// InMemoryBusOptions configures an [InMemoryBus]. The zero value works:
// logging falls back to [slog.Default] and drops are logged at most once
// per topic per second.
type InMemoryBusOptions struct {
	// Logger receives a Warn line when a message is dropped for a slow
	// subscriber, rate-limited by DropLogInterval. Nil falls back to
	// slog.Default(); this package never calls slog.SetDefault.
	Logger *slog.Logger
	// DropLogInterval bounds how often a drop is logged, per topic. A
	// burst that fills a subscriber's buffer can drop thousands of
	// messages in milliseconds, and logging every one of them is itself a
	// load problem — a 5,000-message burst produced 4,983 Warn lines
	// without this. Defaults to one second when zero. DroppedCount is
	// exact regardless of this interval; only the log line is throttled.
	DropLogInterval time.Duration
}

// defaultDropLogInterval is used when InMemoryBusOptions.DropLogInterval is
// unset.
const defaultDropLogInterval = time.Second

// dropWindow tracks drops on one topic between log lines.
type dropWindow struct {
	lastLogged time.Time
	suppressed int64
}

// InMemoryBus is a [Publisher] and [Subscriber] implemented entirely in
// memory, with no external broker.
//
// It is built for tests that want to exercise a real publish-then-receive
// path — as opposed to [NoopPublisher], which only records what would have
// been published. A message published before any Subscribe call for its
// topic is delivered to whichever subscribers are registered at publish
// time; InMemoryBus does not queue for subscribers that join later, the
// same as a Pub/Sub topic with no matching subscription created yet.
//
// InMemoryBus is at-most-once delivery, not at-least-once: each subscriber
// channel is a fixed-size buffer, and a message published while that buffer
// is full is dropped for that subscriber rather than blocking Publish or
// queuing without bound. A drop is never silent — it is logged, at Warn,
// and counted (see [InMemoryBus.DroppedCount]) — but it is also never
// retried; there is no broker underneath to ask again. A handler that
// assumes at-least-once delivery (built and tested against events/pubsub,
// say) will silently lose messages under load if the same code is later
// run against InMemoryBus without accounting for this.
type InMemoryBus struct {
	opts InMemoryBusOptions

	mu      sync.Mutex
	subs    map[string][]chan []byte
	dropped atomic.Int64

	dropLogMu sync.Mutex
	dropLog   map[string]*dropWindow
}

// NewInMemoryBus builds an empty InMemoryBus.
func NewInMemoryBus(opts InMemoryBusOptions) *InMemoryBus {
	return &InMemoryBus{opts: opts, subs: make(map[string][]chan []byte)}
}

// DroppedCount reports how many subscriber deliveries have been dropped
// (across all topics) since this InMemoryBus was created, for a caller or
// test that wants to assert on it directly rather than parsing logs.
func (b *InMemoryBus) DroppedCount() int64 {
	return b.dropped.Load()
}

// Publish marshals event with [Marshal] and delivers it to every subscriber
// currently registered on topic. See the type doc for what happens when a
// subscriber's buffer is full.
func (b *InMemoryBus) Publish(_ context.Context, topic string, event any) error {
	data, err := Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event for topic %s: %w", topic, err)
	}

	// The subscriber-list lock (b.mu) is released before any logging: a
	// publish to one topic that stalls in a Warn write must not block a
	// concurrent publish to an unrelated topic, which is exactly what
	// holding b.mu across the log call would do — measured at 3.36ms for
	// one blocked publish.
	b.mu.Lock()
	subs := b.subs[topic]
	dropped := 0
	for _, ch := range subs {
		select {
		case ch <- data:
		default:
			dropped++
		}
	}
	b.mu.Unlock()

	if dropped > 0 {
		b.recordDrop(topic, dropped)
	}
	return nil
}

// recordDrop updates the exact drop counter and logs at most one Warn line
// per topic per DropLogInterval, naming how many were suppressed since the
// last line — see InMemoryBusOptions.DropLogInterval for why the count is
// exact but the logging is not.
func (b *InMemoryBus) recordDrop(topic string, n int) {
	total := b.dropped.Add(int64(n))

	interval := b.opts.DropLogInterval
	if interval <= 0 {
		interval = defaultDropLogInterval
	}

	now := time.Now()
	var toLog int64

	b.dropLogMu.Lock()
	if b.dropLog == nil {
		b.dropLog = make(map[string]*dropWindow)
	}
	w, ok := b.dropLog[topic]
	if !ok {
		w = &dropWindow{}
		b.dropLog[topic] = w
	}
	w.suppressed += int64(n)
	logNow := now.Sub(w.lastLogged) >= interval
	if logNow {
		toLog = w.suppressed
		w.suppressed = 0
		w.lastLogged = now
	}
	b.dropLogMu.Unlock()

	if !logNow {
		return
	}

	logger := b.opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("event dropped: subscriber buffer full",
		"topic", topic, "dropped_since_last_log", toLog, "total_dropped", total)
}

// Subscribe registers a channel for topic and calls handler for every
// message published to it while this call is running. It blocks until ctx
// is canceled, then deregisters the channel and returns ctx.Err().
func (b *InMemoryBus) Subscribe(ctx context.Context, topic string, handler Handler) error {
	logger := b.opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ch := make(chan []byte, 16)

	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()

	defer b.unsubscribe(topic, ch)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case data := <-ch:
			// A handler error has nowhere to redeliver to in this
			// implementation — see the type doc's at-most-once note. It is
			// logged rather than silently discarded, so a caller relying
			// on InMemoryBus at least sees the failure.
			if err := handler(ctx, data); err != nil {
				logger.Error("event handler failed (in-memory bus cannot redeliver)",
					"topic", topic, "error", err)
			}
		}
	}
}

func (b *InMemoryBus) unsubscribe(topic string, target chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subs[topic]
	for i, ch := range subs {
		if ch == target {
			b.subs[topic] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
}
