package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sampleEvent struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func TestNoopPublisher_RecordsWithoutDelivering(t *testing.T) {
	p := NewNoopPublisher()

	err := p.Publish(context.Background(), "topic-a", sampleEvent{Name: "x", N: 1})
	require.NoError(t, err)

	require.Len(t, p.Published, 1)
	assert.Equal(t, "topic-a", p.Published[0].Topic)

	var got sampleEvent
	require.NoError(t, json.Unmarshal(p.Published[0].Data, &got))
	assert.Equal(t, sampleEvent{Name: "x", N: 1}, got)
}

func TestNoopPublisher_MultiplePublishesAccumulate(t *testing.T) {
	p := NewNoopPublisher()
	require.NoError(t, p.Publish(context.Background(), "t", sampleEvent{Name: "a"}))
	require.NoError(t, p.Publish(context.Background(), "t", sampleEvent{Name: "b"}))
	assert.Len(t, p.Published, 2)
}

// unmarshalable cannot be marshaled to JSON (a channel field), to exercise
// the marshal-failure path.
type unmarshalable struct {
	C chan int
}

func TestNoopPublisher_MarshalFailureIsAnError(t *testing.T) {
	p := NewNoopPublisher()
	err := p.Publish(context.Background(), "t", unmarshalable{C: make(chan int)})
	require.Error(t, err)
	assert.Empty(t, p.Published)
}
