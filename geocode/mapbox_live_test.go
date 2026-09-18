//go:build live

package geocode

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMapboxProvider_Live geocodes a real, well-known address against the
// live Mapbox API. It requires:
//
//	MAPBOX_TOKEN  a valid Mapbox access token
//
// Run with: go test -tags=live ./geocode/... -run Live
//
// This never runs in CI — there is no token CI could safely hold — and
// exists so a change to the Mapbox-facing request or response handling has
// one command that proves it against the real service before it ships.
func TestMapboxProvider_Live(t *testing.T) {
	token := os.Getenv("MAPBOX_TOKEN")
	if token == "" {
		t.Skip("MAPBOX_TOKEN not set — skipping live Mapbox test")
	}

	p := NewMapboxProvider(token, MapboxOptions{})
	coords, err := p.Geocode(context.Background(), "1600 Pennsylvania Ave NW", "Washington", "DC", "20500")
	require.NoError(t, err)
	require.NotNil(t, coords)
	require.InDelta(t, 38.897, coords.Latitude, 0.05)
	require.InDelta(t, -77.036, coords.Longitude, 0.05)
}
