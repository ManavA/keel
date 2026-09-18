package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"cloud.google.com/go/pubsub/v2"
)

// Handler processes one message's raw payload. A non-nil error means the
// message should not be considered handled — [PubSubSubscriber] nacks it so
// the broker redelivers.
type Handler func(ctx context.Context, data []byte) error

// Subscriber delivers messages from a subscription to handler until ctx is
// canceled or handler returns a non-nil error, then returns.
//
// Subscribe blocks. Callers that want to run several subscriptions
// concurrently start each in its own goroutine, the same way
// [cloud.google.com/go/pubsub]'s own Subscription.Receive works underneath
// [PubSubSubscriber].
type Subscriber interface {
	Subscribe(ctx context.Context, subscriptionID string, handler Handler) error
}

// PubSubSubscriberOptions configures a [PubSubSubscriber]. The zero value
// works: logging falls back to [slog.Default].
type PubSubSubscriberOptions struct {
	// Logger receives one Error line per handler failure. Nil falls back
	// to slog.Default(); this package never calls slog.SetDefault.
	Logger *slog.Logger
}

// PubSubSubscriber delivers messages from Google Cloud Pub/Sub
// subscriptions.
type PubSubSubscriber struct {
	client *pubsub.Client
	opts   PubSubSubscriberOptions
}

// NewPubSubSubscriber wraps an already-constructed Pub/Sub client. The
// client is the caller's to close.
func NewPubSubSubscriber(client *pubsub.Client, opts PubSubSubscriberOptions) *PubSubSubscriber {
	return &PubSubSubscriber{client: client, opts: opts}
}

// Subscribe receives messages on subscriptionID and calls handler for each.
// A message is acked only when handler returns nil; a handler error nacks
// the message so Pub/Sub redelivers it. Subscribe blocks until ctx is
// canceled, at which point it returns ctx.Err() (or nil, if Receive itself
// returned first for some other reason).
func (s *PubSubSubscriber) Subscribe(ctx context.Context, subscriptionID string, handler Handler) error {
	logger := s.opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	sub := s.client.Subscriber(subscriptionID)
	err := sub.Receive(ctx, func(msgCtx context.Context, m *pubsub.Message) {
		if err := handler(msgCtx, m.Data); err != nil {
			logger.Error("event handler failed; message will be redelivered",
				"subscription", subscriptionID, "error", err)
			m.Nack()
			return
		}
		m.Ack()
	})
	if err != nil {
		return fmt.Errorf("subscribe to %s: %w", subscriptionID, err)
	}
	return nil
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
type InMemoryBus struct {
	mu   sync.Mutex
	subs map[string][]chan []byte
}

// NewInMemoryBus builds an empty InMemoryBus.
func NewInMemoryBus() *InMemoryBus {
	return &InMemoryBus{subs: make(map[string][]chan []byte)}
}

// Publish marshals event to JSON and delivers it to every subscriber
// currently registered on topic. Delivery to each subscriber's channel is
// buffered and non-blocking up to a small internal buffer, so one slow
// subscriber cannot stall Publish for the others; if a subscriber's buffer
// is full the message is dropped for that subscriber only, matching a
// broker's own behavior under sustained backpressure rather than pretending
// unbounded delivery is free.
func (b *InMemoryBus) Publish(ctx context.Context, topic string, event any) error {
	data, err := marshalEvent(event)
	if err != nil {
		return fmt.Errorf("marshal event for topic %s: %w", topic, err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[topic] {
		select {
		case ch <- data:
		default:
		}
	}
	return nil
}

// Subscribe registers a channel for topic and calls handler for every
// message published to it while this call is running. It blocks until ctx
// is canceled, then deregisters the channel and returns ctx.Err().
func (b *InMemoryBus) Subscribe(ctx context.Context, topic string, handler Handler) error {
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
			// implementation — there is no broker underneath it to ask
			// again. It is logged rather than silently discarded, so a
			// test relying on InMemoryBus at least sees the failure.
			if err := handler(ctx, data); err != nil {
				slog.Default().Error("event handler failed (in-memory bus cannot redeliver)",
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

func marshalEvent(event any) ([]byte, error) {
	if b, ok := event.([]byte); ok {
		return b, nil
	}
	return json.Marshal(event)
}
