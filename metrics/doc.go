// Package metrics is the instrumentation other keel packages report
// through: HTTP request duration, pg pool acquire wait, job outcomes,
// outbox relay lag and attempts, event publish and drop counts, and
// geocode lookup outcomes.
//
// The default needs no external dependency. A nil *Metrics is a valid
// no-op recorder, so every hook takes an optional *Metrics that callers
// leave unset until they want numbers: httpx/middleware.Observe,
// jobs.RunnerOptions, pg.Acquire, outbox.Options, events
// InMemoryBusOptions and geocode.CachedOptions all accept one.
//
// # Instruments and attributes
//
// Each Observe method records the instruments below with the attributes
// beside them. An OTel backend reports them under these exact names;
// metrics/otel adapts them to a Meter, so wiring this package to an
// exporter is one constructor call.
//
//	ObserveHTTP            http.server.request.count (int64 counter) and
//	                       http.server.request.duration (float64 histogram, seconds)
//	                       with http.route and http.response.status_code
//	ObservePoolAcquire     db.pool.acquire.count and db.pool.acquire.duration
//	                       (seconds), with no attributes
//	ObserveJob             job.run.count with job.name and job.status
//	ObserveOutboxPublished outbox.relay.publish.count with
//	                       messaging.destination.name, plus
//	                       outbox.relay.lag (seconds) and
//	                       outbox.relay.attempts with the same attribute
//	ObserveOutboxFailed    outbox.relay.fail.count with
//	                       messaging.destination.name
//	ObserveEventPublished  events.publish.count with
//	                       messaging.destination.name
//	ObserveEventsDropped   events.drop.count with messaging.destination.name
//	ObserveGeocode         geocode.lookup.count with geocode.source and,
//	                       when the lookup resolved, geocode.precision
//
// The status words ObserveJob records are the caller's, not this
// package's: jobs passes Outcome.Status(), so the counter and the log
// line can never disagree. geocode.precision carries the precision tier
// that resolved the lookup (address, postcode, place), which is also
// the fallback depth: a place centroid is two degradations down from an
// exact match. geocode.source is store when the cache answered, miss
// when a remembered miss short-circuited the provider, and provider
// otherwise.
//
// # Testing
//
// InMemory captures every add and record behind a mutex. A test builds
// one, hands out its Metrics, drives the code, and asserts with
// CounterTotal and HistogramValues:
//
//	mem := metrics.NewInMemory()
//	svc := NewService(..., mem.Metrics())
//	...
//	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameJobRuns,
//		metrics.String(metrics.AttrJobName, "nightly"),
//		metrics.String(metrics.AttrJobStatus, jobs.StatusSuccess)))
package metrics
