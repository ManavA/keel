package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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

func TestMarshal_BytesPassThroughUnchanged(t *testing.T) {
	raw := []byte(`{"already":"encoded"}`)
	got, err := Marshal(raw)
	require.NoError(t, err)
	assert.Same(t, &raw[0], &got[0], "a []byte value must pass through, not be re-encoded as a JSON string")
}

func TestMarshal_StructIsJSONEncoded(t *testing.T) {
	got, err := Marshal(sampleEvent{Name: "x", N: 1})
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"x","n":1}`, string(got))
}

// TestMarshal_NoopAndInMemoryBusEncodeIdentically is the regression test
// for the independent review's finding: NoopPublisher used to call
// json.Marshal directly while InMemoryBus special-cased []byte, so the same
// event encoded two different ways depending which implementation
// published it. Both now call Marshal, so this asserts they agree.
func TestMarshal_NoopAndInMemoryBusEncodeIdentically(t *testing.T) {
	event := sampleEvent{Name: testWidgetName, N: 3}

	noop := NewNoopPublisher()
	require.NoError(t, noop.Publish(context.Background(), "t", event))

	bus := NewInMemoryBus(InMemoryBusOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan []byte, 1)
	go func() {
		_ = bus.Subscribe(ctx, "t", func(_ context.Context, data []byte) error {
			received <- data
			return nil
		})
	}()
	waitForSubscriber(t, bus, "t")
	require.NoError(t, bus.Publish(context.Background(), "t", event))

	var busData []byte
	select {
	case busData = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("InMemoryBus never delivered the message")
	}

	assert.Equal(t, noop.Published[0].Data, busData)
}

// testWidgetName is reused across event package test files as the
// canonical sample event name, pulled out because a goconst check applies
// package-wide, not per file.
const testWidgetName = "widget"
