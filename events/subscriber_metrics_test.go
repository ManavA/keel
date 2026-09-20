package events

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/metrics"
)

// TestInMemoryBus_MetricsCountsPublishesAndDrops pins the metrics hook:
// an accepted publish counts once under its topic, and a delivery
// dropped for a full buffer counts as a drop, not a publish.
func TestInMemoryBus_MetricsCountsPublishesAndDrops(t *testing.T) {
	mem := metrics.NewInMemory()
	bus := NewInMemoryBus(InMemoryBusOptions{Metrics: mem.Metrics()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, bus.Publish(ctx, "orders", sampleEvent{Name: "x"}))
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameEventsPublished,
		metrics.String(metrics.AttrTopic, "orders")))
	assert.Equal(t, int64(0), mem.CounterTotal(metrics.NameEventsDropped,
		metrics.String(metrics.AttrTopic, "orders")))

	blocked := make(chan struct{})
	defer close(blocked)
	go func() {
		_ = bus.Subscribe(ctx, "slow", func(context.Context, []byte) error {
			<-blocked // hold the handler open so the subscriber's buffer fills
			return nil
		})
	}()
	waitForSubscriber(t, bus, "slow")

	dropped, accepted := 0, int64(0)
	for i := 0; i < 100 && dropped == 0; i++ {
		if err := bus.Publish(context.Background(), "slow", sampleEvent{Name: "x"}); err != nil {
			var fullErr *BufferFullError
			require.ErrorAs(t, err, &fullErr)
			dropped = fullErr.Dropped
		} else {
			accepted++
		}
	}
	require.Greater(t, dropped, 0, "publishing past a full buffer must drop")
	assert.Equal(t, int64(dropped), mem.CounterTotal(metrics.NameEventsDropped,
		metrics.String(metrics.AttrTopic, "slow")))
	assert.Equal(t, accepted, mem.CounterTotal(metrics.NameEventsPublished,
		metrics.String(metrics.AttrTopic, "slow")),
		"only the accepted publishes count as published")
}
