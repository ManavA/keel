package geocode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/metrics"
)

// TestCached_MetricsRecordsSourceAndPrecision pins the metrics hook:
// each lookup counts once under the source that answered it, with the
// precision tier when the lookup resolved.
func TestCached_MetricsRecordsSourceAndPrecision(t *testing.T) {
	ctx := context.Background()
	mem := metrics.NewInMemory()

	store := NewMemoryStore()
	hit := Coordinates{Latitude: 1, Longitude: 2, Precision: PrecisionPostcode}
	require.NoError(t, store.Set(ctx, NormalizeKey("1 Main St", "Oakland", "CA", "94601"), hit))

	provider := &fakeProvider{coords: &Coordinates{Latitude: 3, Longitude: 4, Precision: PrecisionAddress}}
	c := NewCached(provider, store, CachedOptions{Metrics: mem.Metrics()})

	_, err := c.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
	require.NoError(t, err)
	_, err = c.Geocode(ctx, "2 Main St", "Oakland", "CA", "94601")
	require.NoError(t, err)

	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameGeocodeLookups,
		metrics.String(metrics.AttrGeocodeSource, metrics.GeocodeStore),
		metrics.String(metrics.AttrGeocodePrecision, PrecisionPostcode)))
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameGeocodeLookups,
		metrics.String(metrics.AttrGeocodeSource, metrics.GeocodeProvider),
		metrics.String(metrics.AttrGeocodePrecision, PrecisionAddress)))

	unresolvable := NewCached(&fakeProvider{}, NewMemoryStore(), CachedOptions{Metrics: mem.Metrics()})
	_, err = unresolvable.Geocode(ctx, "1 Main St", "Nowhere", "CA", "00000")
	require.NoError(t, err)
	_, err = unresolvable.Geocode(ctx, "1 Main St", "Nowhere", "CA", "00000")
	require.NoError(t, err)
	assert.Equal(t, int64(2), mem.CounterTotal(metrics.NameGeocodeLookups,
		metrics.String(metrics.AttrGeocodeSource, metrics.GeocodeProvider)),
		"the first lookup plus the earlier provider hit")
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameGeocodeLookups,
		metrics.String(metrics.AttrGeocodeSource, metrics.GeocodeMiss)),
		"the remembered miss short-circuits the provider and counts as a miss")
}
