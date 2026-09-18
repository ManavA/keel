// Package pubsub implements [events.Publisher] and [events.Subscriber] for
// Google Cloud Pub/Sub.
//
// It is a separate package from events, rather than a type inside it, so
// that a caller who only needs [events.InMemoryBus] — the in-process
// default, with no external broker — does not pull in
// cloud.google.com/go/pubsub/v2 and its gRPC and OpenTelemetry dependency
// tree merely by importing events. Import this package only when a service
// actually needs cross-process delivery.
//
// Delivery here is at-least-once, matching Pub/Sub itself: a handler error
// nacks the message and Pub/Sub redelivers it, possibly more than once and
// possibly out of order. Compare [events.InMemoryBus], which is
// at-most-once. A Handler written for one is not automatically correct for
// the other — see [events.Subscriber]'s doc.
package pubsub

import (
	"context"
	"fmt"
	"log/slog"

	gpubsub "cloud.google.com/go/pubsub/v2"

	"github.com/ManavA/keel/events"
)

// PublisherOptions configures a [Publisher]. The zero value works: logging
// falls back to [slog.Default].
type PublisherOptions struct {
	// Logger receives one Info line per published message. Nil falls back
	// to slog.Default(); this package never calls slog.SetDefault.
	Logger *slog.Logger
}

// Publisher publishes events to Google Cloud Pub/Sub.
type Publisher struct {
	client *gpubsub.Client
	opts   PublisherOptions
}

var _ events.Publisher = (*Publisher)(nil)

// NewPublisher wraps an already-constructed Pub/Sub client. The client is
// the caller's to close.
func NewPublisher(client *gpubsub.Client, opts PublisherOptions) *Publisher {
	return &Publisher{client: client, opts: opts}
}

// Publish marshals event with [events.Marshal] and publishes it to topic,
// waiting for Pub/Sub to acknowledge the publish before returning.
func (p *Publisher) Publish(ctx context.Context, topic string, event any) error {
	data, err := events.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event for topic %s: %w", topic, err)
	}

	result := p.client.Publisher(topic).Publish(ctx, &gpubsub.Message{Data: data})
	id, err := result.Get(ctx)
	if err != nil {
		return fmt.Errorf("publish to %s: %w", topic, err)
	}

	logger := p.opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("published event", "topic", topic, "message_id", id)
	return nil
}

// SubscriberOptions configures a [Subscriber]. The zero value works:
// logging falls back to [slog.Default].
type SubscriberOptions struct {
	// Logger receives one Error line per handler failure. Nil falls back
	// to slog.Default(); this package never calls slog.SetDefault.
	Logger *slog.Logger
}

// Subscriber delivers messages from Google Cloud Pub/Sub subscriptions.
type Subscriber struct {
	client *gpubsub.Client
	opts   SubscriberOptions
}

var _ events.Subscriber = (*Subscriber)(nil)

// NewSubscriber wraps an already-constructed Pub/Sub client. The client is
// the caller's to close.
func NewSubscriber(client *gpubsub.Client, opts SubscriberOptions) *Subscriber {
	return &Subscriber{client: client, opts: opts}
}

// Subscribe receives messages on subscriptionID and calls handler for each.
// A message is acked only when handler returns nil; a handler error nacks
// the message so Pub/Sub redelivers it. Subscribe blocks until ctx is
// canceled, at which point it returns ctx.Err() (or nil, if Receive itself
// returned first for some other reason).
func (s *Subscriber) Subscribe(ctx context.Context, subscriptionID string, handler events.Handler) error {
	logger := s.opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	sub := s.client.Subscriber(subscriptionID)
	err := sub.Receive(ctx, func(msgCtx context.Context, m *gpubsub.Message) {
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
