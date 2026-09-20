package otel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"

	"github.com/ManavA/keel/metrics"
)

type fakeCounter struct {
	embedded.Int64Counter
	name   string
	values []int64
	optLen []int
}

func (c *fakeCounter) Add(_ context.Context, v int64, opts ...otelmetric.AddOption) {
	c.values = append(c.values, v)
	c.optLen = append(c.optLen, len(opts))
}

func (c *fakeCounter) Enabled(context.Context) bool { return true }

type fakeHistogram struct {
	embedded.Float64Histogram
	name   string
	values []float64
	optLen []int
}

func (h *fakeHistogram) Record(_ context.Context, v float64, opts ...otelmetric.RecordOption) {
	h.values = append(h.values, v)
	h.optLen = append(h.optLen, len(opts))
}

func (h *fakeHistogram) Enabled(context.Context) bool { return true }

type fakeMeter struct {
	embedded.Meter
	counters   map[string]*fakeCounter
	histograms map[string]*fakeHistogram
}

func newFakeMeter() *fakeMeter {
	return &fakeMeter{counters: map[string]*fakeCounter{}, histograms: map[string]*fakeHistogram{}}
}

func (m *fakeMeter) Int64Counter(name string, _ ...otelmetric.Int64CounterOption) (otelmetric.Int64Counter, error) {
	c := &fakeCounter{name: name}
	m.counters[name] = c
	return c, nil
}

func (m *fakeMeter) Float64Histogram(name string, _ ...otelmetric.Float64HistogramOption) (otelmetric.Float64Histogram, error) {
	h := &fakeHistogram{name: name}
	m.histograms[name] = h
	return h, nil
}

func (m *fakeMeter) Int64UpDownCounter(string, ...otelmetric.Int64UpDownCounterOption) (otelmetric.Int64UpDownCounter, error) {
	return nil, nil
}

func (m *fakeMeter) Int64Histogram(string, ...otelmetric.Int64HistogramOption) (otelmetric.Int64Histogram, error) {
	return nil, nil
}

func (m *fakeMeter) Int64Gauge(string, ...otelmetric.Int64GaugeOption) (otelmetric.Int64Gauge, error) {
	return nil, nil
}

func (m *fakeMeter) Int64ObservableCounter(string, ...otelmetric.Int64ObservableCounterOption) (otelmetric.Int64ObservableCounter, error) {
	return nil, nil
}

func (m *fakeMeter) Int64ObservableUpDownCounter(string, ...otelmetric.Int64ObservableUpDownCounterOption) (otelmetric.Int64ObservableUpDownCounter, error) {
	return nil, nil
}

func (m *fakeMeter) Int64ObservableGauge(string, ...otelmetric.Int64ObservableGaugeOption) (otelmetric.Int64ObservableGauge, error) {
	return nil, nil
}

func (m *fakeMeter) Float64Counter(string, ...otelmetric.Float64CounterOption) (otelmetric.Float64Counter, error) {
	return nil, nil
}

func (m *fakeMeter) Float64UpDownCounter(string, ...otelmetric.Float64UpDownCounterOption) (otelmetric.Float64UpDownCounter, error) {
	return nil, nil
}

func (m *fakeMeter) Float64Gauge(string, ...otelmetric.Float64GaugeOption) (otelmetric.Float64Gauge, error) {
	return nil, nil
}

func (m *fakeMeter) Float64ObservableCounter(string, ...otelmetric.Float64ObservableCounterOption) (otelmetric.Float64ObservableCounter, error) {
	return nil, nil
}

func (m *fakeMeter) Float64ObservableUpDownCounter(string, ...otelmetric.Float64ObservableUpDownCounterOption) (otelmetric.Float64ObservableUpDownCounter, error) {
	return nil, nil
}

func (m *fakeMeter) Float64ObservableGauge(string, ...otelmetric.Float64ObservableGaugeOption) (otelmetric.Float64ObservableGauge, error) {
	return nil, nil
}

func (m *fakeMeter) RegisterCallback(otelmetric.Callback, ...otelmetric.Observable) (otelmetric.Registration, error) {
	return nil, nil
}

// TestInstrumentsBuildsEveryKeelInstrument pins the bridge: one OTel
// instrument per documented keel name, and measurements flowing through
// with their attributes attached.
func TestInstrumentsBuildsEveryKeelInstrument(t *testing.T) {
	fake := newFakeMeter()
	inst, err := Instruments(fake)
	require.NoError(t, err)

	m := metrics.New(inst)
	ctx := context.Background()
	m.ObserveHTTP(ctx, "/jobs/{name}", 200, 0)
	m.ObservePoolAcquire(ctx, 0)
	m.ObserveJob(ctx, "nightly", "success")
	m.ObserveOutboxPublished(ctx, "t", 0, 1)
	m.ObserveOutboxFailed(ctx, "t", 1)
	m.ObserveEventPublished(ctx, "t")
	m.ObserveEventsDropped(ctx, "t", 2)
	m.ObserveGeocode(ctx, metrics.GeocodeStore, "address")

	for _, name := range []string{
		metrics.NameHTTPRequests, metrics.NamePoolAcquires, metrics.NameJobRuns,
		metrics.NameOutboxPublished, metrics.NameOutboxFailed,
		metrics.NameEventsPublished, metrics.NameEventsDropped, metrics.NameGeocodeLookups,
	} {
		c, ok := fake.counters[name]
		require.True(t, ok, "no OTel counter built for %s", name)
		require.NotEmpty(t, c.values, "nothing recorded on %s", name)
		for _, n := range c.optLen {
			assert.Equal(t, 1, n, "attributes must reach OTel as one option on %s", name)
		}
	}
	for _, name := range []string{
		metrics.NameHTTPRequestDur, metrics.NamePoolAcquireDur,
		metrics.NameOutboxLag, metrics.NameOutboxAttempts,
	} {
		h, ok := fake.histograms[name]
		require.True(t, ok, "no OTel histogram built for %s", name)
		require.NotEmpty(t, h.values, "nothing recorded on %s", name)
	}

	assert.Equal(t, []int64{2}, fake.counters[metrics.NameEventsDropped].values)
}

// TestInstrumentsRejectsNilMeter fails fast on a caller bug: a nil
// meter must error, not build instruments that record into nothing.
func TestInstrumentsRejectsNilMeter(t *testing.T) {
	_, err := Instruments(nil)
	require.Error(t, err)
}

// TestToKeyValuesMapsAttributesOneToOne pins the dimension mapping: the
// OTel series carry the documented attribute names with string values.
func TestToKeyValuesMapsAttributesOneToOne(t *testing.T) {
	kvs := toKeyValues([]metrics.Attr{
		metrics.String(metrics.AttrHTTPRoute, "/jobs/{name}"),
		metrics.Status(418),
	})
	require.Len(t, kvs, 2)
	assert.Equal(t, metrics.AttrHTTPRoute, string(kvs[0].Key))
	assert.Equal(t, "/jobs/{name}", kvs[0].Value.String())
	assert.Equal(t, metrics.AttrHTTPStatus, string(kvs[1].Key))
	assert.Equal(t, "418", kvs[1].Value.String())
}
