package metrics_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/metrics"
)

// TestNilAndZeroAreNoOp pins the default: hooks take an optional
// *Metrics, and an unset one must never panic or need a backend.
func TestNilAndZeroAreNoOp(t *testing.T) {
	ctx := context.Background()
	var nilMetrics *metrics.Metrics
	zero := &metrics.Metrics{}

	for _, m := range []*metrics.Metrics{nilMetrics, zero} {
		m.ObserveHTTP(ctx, "/x", 200, time.Millisecond)
		m.ObservePoolAcquire(ctx, time.Millisecond)
		m.ObserveJob(ctx, "nightly", "success")
		m.ObserveOutboxPublished(ctx, "t", time.Second, 1)
		m.ObserveOutboxFailed(ctx, "t", 2)
		m.ObserveEventPublished(ctx, "t")
		m.ObserveEventsDropped(ctx, "t", 3)
		m.ObserveGeocode(ctx, metrics.GeocodeStore, "address")
	}
}

// TestObserveMethodsRecordDocumentedInstruments drives every Observe
// method once and asserts the instrument name, value and attributes
// each one promises in the package doc.
func TestObserveMethodsRecordDocumentedInstruments(t *testing.T) {
	ctx := context.Background()
	mem := metrics.NewInMemory()
	m := mem.Metrics()

	m.ObservePoolAcquire(ctx, 250*time.Millisecond)
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NamePoolAcquires))
	got := mem.HistogramValues(metrics.NamePoolAcquireDur)
	assert.Len(t, got, 1)
	assert.InDelta(t, 0.25, got[0], 1e-9)

	m.ObserveJob(ctx, "nightly", "partial")
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameJobRuns,
		metrics.String(metrics.AttrJobName, "nightly"),
		metrics.String(metrics.AttrJobStatus, "partial")))
	assert.Equal(t, int64(0), mem.CounterTotal(metrics.NameJobRuns,
		metrics.String(metrics.AttrJobName, "nightly"),
		metrics.String(metrics.AttrJobStatus, "success")),
		"attribute filtering must distinguish status words")

	m.ObserveOutboxPublished(ctx, "widgets.created", 2*time.Second, 3)
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameOutboxPublished,
		metrics.String(metrics.AttrTopic, "widgets.created")))
	lag := mem.HistogramValues(metrics.NameOutboxLag,
		metrics.String(metrics.AttrTopic, "widgets.created"))
	assert.Len(t, lag, 1)
	assert.InDelta(t, 2.0, lag[0], 1e-9)
	assert.Equal(t, []float64{3}, mem.HistogramValues(metrics.NameOutboxAttempts,
		metrics.String(metrics.AttrTopic, "widgets.created")))

	m.ObserveOutboxFailed(ctx, "widgets.created", 2)
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameOutboxFailed,
		metrics.String(metrics.AttrTopic, "widgets.created")))
	assert.Equal(t, []float64{2}, mem.HistogramValues(metrics.NameOutboxAttempts,
		metrics.String(metrics.AttrTopic, "widgets.created"))[1:])

	m.ObserveEventPublished(ctx, "t")
	m.ObserveEventsDropped(ctx, "t", 0)
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameEventsPublished,
		metrics.String(metrics.AttrTopic, "t")))
	assert.Equal(t, int64(0), mem.CounterTotal(metrics.NameEventsDropped,
		metrics.String(metrics.AttrTopic, "t")),
		"dropping nothing must not record a drop")
	m.ObserveEventsDropped(ctx, "t", 4)
	assert.Equal(t, int64(4), mem.CounterTotal(metrics.NameEventsDropped,
		metrics.String(metrics.AttrTopic, "t")))

	m.ObserveGeocode(ctx, metrics.GeocodeStore, "postcode")
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameGeocodeLookups,
		metrics.String(metrics.AttrGeocodeSource, metrics.GeocodeStore),
		metrics.String(metrics.AttrGeocodePrecision, "postcode")))
	m.ObserveGeocode(ctx, metrics.GeocodeMiss, "")
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameGeocodeLookups,
		metrics.String(metrics.AttrGeocodeSource, metrics.GeocodeMiss)),
		"a miss carries no precision attribute")
}
