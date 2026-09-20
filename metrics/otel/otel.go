// Package otel adapts metrics.Instruments to OpenTelemetry: one OTel
// instrument per keel instrument, under the names and attributes the
// metrics package documents.
//
// Importing metrics alone pulls in no observability dependency. Import
// this subpackage only where the OTel backend is actually wired, so a
// binary that never exports metrics never links the OTel API either:
//
//	meter := otelSDK.NewMeterProvider(...).Meter("github.com/ManavA/keel/metrics")
//	inst, err := keelotel.Instruments(meter)
//	svc := NewService(..., metrics.New(inst))
//
// Durations and lags report in seconds, matching the
// http.server.request.duration semantic convention.
package otel

import (
	"context"
	"fmt"

	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/ManavA/keel/metrics"
)

// ScopeName is the recommended meter name when building the Meter
// passed to Instruments: the import path of the instrumented package.
const ScopeName = "github.com/ManavA/keel/metrics"

type counter struct {
	inst otelmetric.Int64Counter
}

func (c counter) Add(ctx context.Context, delta int64, attrs ...metrics.Attr) {
	c.inst.Add(ctx, delta, otelmetric.WithAttributes(toKeyValues(attrs)...))
}

type histogram struct {
	inst otelmetric.Float64Histogram
}

func (h histogram) Record(ctx context.Context, value float64, attrs ...metrics.Attr) {
	h.inst.Record(ctx, value, otelmetric.WithAttributes(toKeyValues(attrs)...))
}

// Instruments builds every keel instrument against meter. An error
// names the instrument that failed to build; a nil meter is a caller
// bug and fails fast rather than recording into nothing.
func Instruments(meter otelmetric.Meter) (metrics.Instruments, error) {
	if meter == nil {
		return metrics.Instruments{}, fmt.Errorf("otel: meter is nil")
	}

	var out metrics.Instruments
	build := []struct {
		name string
		fn   func() error
	}{
		{metrics.NameHTTPRequests, func() error {
			c, err := meter.Int64Counter(metrics.NameHTTPRequests,
				otelmetric.WithDescription("Served HTTP requests."),
				otelmetric.WithUnit("1"))
			if err == nil {
				out.HTTPRequests = counter{c}
			}
			return err
		}},
		{metrics.NameHTTPRequestDur, func() error {
			h, err := meter.Float64Histogram(metrics.NameHTTPRequestDur,
				otelmetric.WithDescription("Served HTTP request duration."),
				otelmetric.WithUnit("s"))
			if err == nil {
				out.HTTPRequestDur = histogram{h}
			}
			return err
		}},
		{metrics.NamePoolAcquires, func() error {
			c, err := meter.Int64Counter(metrics.NamePoolAcquires,
				otelmetric.WithDescription("pg pool connection acquisitions."),
				otelmetric.WithUnit("1"))
			if err == nil {
				out.PoolAcquires = counter{c}
			}
			return err
		}},
		{metrics.NamePoolAcquireDur, func() error {
			h, err := meter.Float64Histogram(metrics.NamePoolAcquireDur,
				otelmetric.WithDescription("Time waited for a pg pool connection."),
				otelmetric.WithUnit("s"))
			if err == nil {
				out.PoolAcquireDur = histogram{h}
			}
			return err
		}},
		{metrics.NameJobRuns, func() error {
			c, err := meter.Int64Counter(metrics.NameJobRuns,
				otelmetric.WithDescription("Finished job runs."),
				otelmetric.WithUnit("1"))
			if err == nil {
				out.JobRuns = counter{c}
			}
			return err
		}},
		{metrics.NameOutboxPublished, func() error {
			c, err := meter.Int64Counter(metrics.NameOutboxPublished,
				otelmetric.WithDescription("Outbox rows relayed to the publisher."),
				otelmetric.WithUnit("1"))
			if err == nil {
				out.OutboxPublished = counter{c}
			}
			return err
		}},
		{metrics.NameOutboxFailed, func() error {
			c, err := meter.Int64Counter(metrics.NameOutboxFailed,
				otelmetric.WithDescription("Outbox row publish attempts that failed."),
				otelmetric.WithUnit("1"))
			if err == nil {
				out.OutboxFailed = counter{c}
			}
			return err
		}},
		{metrics.NameOutboxLag, func() error {
			h, err := meter.Float64Histogram(metrics.NameOutboxLag,
				otelmetric.WithDescription("Outbox row age at publish."),
				otelmetric.WithUnit("s"))
			if err == nil {
				out.OutboxLag = histogram{h}
			}
			return err
		}},
		{metrics.NameOutboxAttempts, func() error {
			h, err := meter.Float64Histogram(metrics.NameOutboxAttempts,
				otelmetric.WithDescription("Outbox row publish attempts per row."),
				otelmetric.WithUnit("1"))
			if err == nil {
				out.OutboxAttempts = histogram{h}
			}
			return err
		}},
		{metrics.NameEventsPublished, func() error {
			c, err := meter.Int64Counter(metrics.NameEventsPublished,
				otelmetric.WithDescription("Accepted event publishes."),
				otelmetric.WithUnit("1"))
			if err == nil {
				out.EventsPublished = counter{c}
			}
			return err
		}},
		{metrics.NameEventsDropped, func() error {
			c, err := meter.Int64Counter(metrics.NameEventsDropped,
				otelmetric.WithDescription("Dropped event subscriber deliveries."),
				otelmetric.WithUnit("1"))
			if err == nil {
				out.EventsDropped = counter{c}
			}
			return err
		}},
		{metrics.NameGeocodeLookups, func() error {
			c, err := meter.Int64Counter(metrics.NameGeocodeLookups,
				otelmetric.WithDescription("Geocode lookups."),
				otelmetric.WithUnit("1"))
			if err == nil {
				out.GeocodeLookups = counter{c}
			}
			return err
		}},
	}
	for _, b := range build {
		if err := b.fn(); err != nil {
			return metrics.Instruments{}, fmt.Errorf("otel: build %s: %w", b.name, err)
		}
	}
	return out, nil
}
