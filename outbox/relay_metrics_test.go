package outbox_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/metrics"
	"github.com/ManavA/keel/outbox"
)

// TestRelay_MetricsRecordsPublishAndFailure pins the metrics hook: a
// relayed row counts once with its lag and attempt count, and a row
// whose publish fails counts as a failure with its attempt count.
func TestRelay_MetricsRecordsPublishAndFailure(t *testing.T) {
	pool := openEmptyPool(t)
	enqueueOne(t, pool, "widgets.created", []byte(`{"a":1}`))
	bad := enqueueOne(t, pool, "widgets.created", []byte(`{"a":2}`))

	publisher := newFakePublisher()
	publisher.failAlways(bad)

	mem := metrics.NewInMemory()
	relay, err := outbox.NewRelay(pool, outbox.Options{Publisher: publisher, Metrics: mem.Metrics()})
	require.NoError(t, err)

	published, err := relay.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, published)

	topic := metrics.String(metrics.AttrTopic, "widgets.created")
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameOutboxPublished, topic))
	lag := mem.HistogramValues(metrics.NameOutboxLag, topic)
	require.Len(t, lag, 1)
	assert.GreaterOrEqual(t, lag[0], 0.0)
	assert.ElementsMatch(t, []float64{1, 1}, mem.HistogramValues(metrics.NameOutboxAttempts, topic),
		"both rows needed one attempt each: the good row published, the bad row failed once")

	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameOutboxFailed, topic))
}
