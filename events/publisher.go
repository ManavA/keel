package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"cloud.google.com/go/pubsub/v2"
)

// Publisher publishes an event to a named topic. event is marshaled to JSON
// by the implementation; see the package doc for why it is not unmarshaled
// back into a caller type on the subscribe side.
type Publisher interface {
	Publish(ctx context.Context, topic string, event any) error
}

// PubSubPublisherOptions configures a [PubSubPublisher]. The zero value
// works: logging falls back to [slog.Default].
type PubSubPublisherOptions struct {
	// Logger receives one Info line per published message. Nil falls back
	// to slog.Default(); this package never calls slog.SetDefault.
	Logger *slog.Logger
}

// PubSubPublisher publishes events to Google Cloud Pub/Sub.
type PubSubPublisher struct {
	client *pubsub.Client
	opts   PubSubPublisherOptions
}

// NewPubSubPublisher wraps an already-constructed Pub/Sub client. The client
// is the caller's to close.
func NewPubSubPublisher(client *pubsub.Client, opts PubSubPublisherOptions) *PubSubPublisher {
	return &PubSubPublisher{client: client, opts: opts}
}

// Publish marshals event to JSON and publishes it to topic, waiting for
// Pub/Sub to acknowledge the publish before returning.
func (p *PubSubPublisher) Publish(ctx context.Context, topic string, event any) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event for topic %s: %w", topic, err)
	}

	result := p.client.Publisher(topic).Publish(ctx, &pubsub.Message{Data: data})
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

// PublishedMessage records one message published through [NoopPublisher].
type PublishedMessage struct {
	Topic string
	Data  []byte
}

// NoopPublisher records published events in memory instead of delivering
// them anywhere. Use it in a test that only needs to assert "X was
// published", without needing a working subscribe loop on the other end —
// for that, use [InMemoryBus] instead.
type NoopPublisher struct {
	Published []PublishedMessage
}

// NewNoopPublisher builds an empty NoopPublisher.
func NewNoopPublisher() *NoopPublisher {
	return &NoopPublisher{}
}

// Publish marshals event to JSON and appends it to Published. It never
// returns an error except a marshal failure, and it never delivers the
// message to any subscriber.
func (p *NoopPublisher) Publish(_ context.Context, topic string, event any) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event for topic %s: %w", topic, err)
	}
	p.Published = append(p.Published, PublishedMessage{Topic: topic, Data: data})
	return nil
}
