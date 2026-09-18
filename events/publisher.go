package events

import (
	"context"
	"encoding/json"
	"fmt"
)

// Publisher publishes an event to a named topic. event is marshaled with
// [Marshal] by the implementation; see the package doc for why it is not
// unmarshaled back into a caller type on the subscribe side.
type Publisher interface {
	Publish(ctx context.Context, topic string, event any) error
}

// Marshal encodes event for delivery: a []byte value passes through
// unchanged, and anything else is encoded with [encoding/json.Marshal].
// [NoopPublisher], [InMemoryBus] and events/pubsub's Publisher all use this
// exact function, so the same event marshals identically regardless which
// implementation a caller has chosen — see the package doc for why that
// consistency matters.
func Marshal(event any) ([]byte, error) {
	if b, ok := event.([]byte); ok {
		return b, nil
	}
	return json.Marshal(event)
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

// Publish marshals event with [Marshal] and appends it to Published. It
// never returns an error except a marshal failure, and it never delivers
// the message to any subscriber.
func (p *NoopPublisher) Publish(_ context.Context, topic string, event any) error {
	data, err := Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event for topic %s: %w", topic, err)
	}
	p.Published = append(p.Published, PublishedMessage{Topic: topic, Data: data})
	return nil
}
