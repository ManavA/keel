package metrics

import (
	"context"
	"strconv"
	"time"
)

// Attribute keys recorded with the instruments. They are constants so a
// dashboard and the code that feeds it cannot spell them differently.
const (
	// AttrHTTPRoute is the matched route pattern ("/jobs/{name}"), never
	// the concrete path: concrete paths have unbounded cardinality and
	// make one time series per id.
	AttrHTTPRoute = "http.route"
	// AttrHTTPStatus is the numeric status code as a string, following
	// OpenTelemetry's http.response.status_code convention.
	AttrHTTPStatus = "http.response.status_code"

	// AttrJobName identifies the job; AttrJobStatus carries its status
	// word (see jobs.Outcome.Status).
	AttrJobName   = "job.name"
	AttrJobStatus = "job.status"

	// AttrTopic names the event topic, following OpenTelemetry's
	// messaging.destination.name convention.
	AttrTopic = "messaging.destination.name"

	// AttrGeocodeSource is how a geocode lookup was answered: store,
	// miss, or provider. AttrGeocodePrecision is the precision tier that
	// resolved it, when one did.
	AttrGeocodeSource    = "geocode.source"
	AttrGeocodePrecision = "geocode.precision"
)

// Geocode sources for AttrGeocodeSource.
const (
	GeocodeStore    = "store"
	GeocodeMiss     = "miss"
	GeocodeProvider = "provider"
)

// Instrument names, shared by the in-process recorder and the OTel
// adapter so both report the same time series.
const (
	NameHTTPRequests    = "http.server.request.count"
	NameHTTPRequestDur  = "http.server.request.duration"
	NamePoolAcquires    = "db.pool.acquire.count"
	NamePoolAcquireDur  = "db.pool.acquire.duration"
	NameJobRuns         = "job.run.count"
	NameOutboxPublished = "outbox.relay.publish.count"
	NameOutboxFailed    = "outbox.relay.fail.count"
	NameOutboxLag       = "outbox.relay.lag"
	NameOutboxAttempts  = "outbox.relay.attempts"
	NameEventsPublished = "events.publish.count"
	NameEventsDropped   = "events.drop.count"
	NameGeocodeLookups  = "geocode.lookup.count"
)

// Attr is one key-value attribute recorded with a measurement. Values
// are strings: the instruments' dimensions (routes, status words,
// topics) are all discrete, and a string keeps the OTel adapter a
// direct mapping.
type Attr struct {
	Key   string
	Value string
}

// String builds an Attr.
func String(key, value string) Attr {
	return Attr{Key: key, Value: value}
}

// Status builds the AttrHTTPStatus attribute from a numeric status code.
func Status(code int) Attr {
	return Attr{Key: AttrHTTPStatus, Value: strconv.Itoa(code)}
}

// Counter is a monotonically increasing int64 sum. It is declared here,
// where it is used, rather than imported from an observability
// framework, so the default path carries no dependency.
type Counter interface {
	Add(ctx context.Context, delta int64, attrs ...Attr)
}

// Histogram is a distribution of float64 samples, in seconds for the
// duration and lag instruments.
type Histogram interface {
	Record(ctx context.Context, value float64, attrs ...Attr)
}

// Instruments are the sinks one Metrics records into. A nil Counter or
// Histogram is a valid sink that drops its measurements, so a Metrics
// built with only some fields set records only those.
type Instruments struct {
	HTTPRequests    Counter
	HTTPRequestDur  Histogram
	PoolAcquires    Counter
	PoolAcquireDur  Histogram
	JobRuns         Counter
	OutboxPublished Counter
	OutboxFailed    Counter
	OutboxLag       Histogram
	OutboxAttempts  Histogram
	EventsPublished Counter
	EventsDropped   Counter
	GeocodeLookups  Counter
}

// Metrics records keel's counters and histograms into Instruments. The
// zero value is a valid no-op recorder, and so is a nil *Metrics, so
// callers keep one in an options struct and leave it unset until they
// want numbers.
type Metrics struct {
	Sinks Instruments
}

// New builds a Metrics recording into inst.
func New(inst Instruments) *Metrics {
	return &Metrics{Sinks: inst}
}

func (m *Metrics) add(sink Counter, ctx context.Context, delta int64, attrs ...Attr) {
	if m == nil || sink == nil {
		return
	}
	sink.Add(ctx, delta, attrs...)
}

func (m *Metrics) record(sink Histogram, ctx context.Context, value float64, attrs ...Attr) {
	if m == nil || sink == nil {
		return
	}
	sink.Record(ctx, value, attrs...)
}

// ObserveHTTP records one served request: a count and a duration in
// seconds, both with the route pattern and the status code.
func (m *Metrics) ObserveHTTP(ctx context.Context, route string, status int, d time.Duration) {
	attrs := []Attr{String(AttrHTTPRoute, route), Status(status)}
	m.add(m.sinks().HTTPRequests, ctx, 1, attrs...)
	m.record(m.sinks().HTTPRequestDur, ctx, d.Seconds(), attrs...)
}

// ObservePoolAcquire records one pool acquisition and how long the
// caller waited for a connection, in seconds. The wait is recorded
// whether acquisition succeeded or not: a failed acquire still waited.
func (m *Metrics) ObservePoolAcquire(ctx context.Context, wait time.Duration) {
	m.add(m.sinks().PoolAcquires, ctx, 1)
	m.record(m.sinks().PoolAcquireDur, ctx, wait.Seconds())
}

// ObserveJob records one finished job run under its status word.
func (m *Metrics) ObserveJob(ctx context.Context, name, status string) {
	m.add(m.sinks().JobRuns, ctx, 1,
		String(AttrJobName, name), String(AttrJobStatus, status))
}

// ObserveOutboxPublished records one relayed row: a count, the lag from
// the row's creation to its publish in seconds, and how many publish
// attempts the row needed, all with the topic.
func (m *Metrics) ObserveOutboxPublished(ctx context.Context, topic string, lag time.Duration, attempts int) {
	attr := String(AttrTopic, topic)
	m.add(m.sinks().OutboxPublished, ctx, 1, attr)
	m.record(m.sinks().OutboxLag, ctx, lag.Seconds(), attr)
	m.record(m.sinks().OutboxAttempts, ctx, float64(attempts), attr)
}

// ObserveOutboxFailed records one row whose publish failed on this
// attempt, with the topic. attempts is the row's attempt count after
// this failure.
func (m *Metrics) ObserveOutboxFailed(ctx context.Context, topic string, attempts int) {
	m.add(m.sinks().OutboxFailed, ctx, 1, String(AttrTopic, topic))
	m.record(m.sinks().OutboxAttempts, ctx, float64(attempts), String(AttrTopic, topic))
}

// ObserveEventPublished records one accepted publish with the topic.
func (m *Metrics) ObserveEventPublished(ctx context.Context, topic string) {
	m.add(m.sinks().EventsPublished, ctx, 1, String(AttrTopic, topic))
}

// ObserveEventsDropped records n dropped subscriber deliveries with the
// topic.
func (m *Metrics) ObserveEventsDropped(ctx context.Context, topic string, n int) {
	if n <= 0 {
		return
	}
	m.add(m.sinks().EventsDropped, ctx, int64(n), String(AttrTopic, topic))
}

// ObserveGeocode records one lookup with its source (see GeocodeStore
// and friends). precision is the tier that resolved the lookup and is
// empty when nothing resolved.
func (m *Metrics) ObserveGeocode(ctx context.Context, source, precision string) {
	attrs := []Attr{String(AttrGeocodeSource, source)}
	if precision != "" {
		attrs = append(attrs, String(AttrGeocodePrecision, precision))
	}
	m.add(m.sinks().GeocodeLookups, ctx, 1, attrs...)
}

func (m *Metrics) sinks() Instruments {
	if m == nil {
		return Instruments{}
	}
	return m.Sinks
}
