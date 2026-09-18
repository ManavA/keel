package geocode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// mapboxStub answers a canned feature list per requested `types` value,
// letting a test drive exactly one tier of the degrade chain at a time.
func mapboxStub(t *testing.T, byType map[string][]mapboxFeature) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		types := r.URL.Query().Get("types")
		features, ok := byType[types]
		if !ok {
			features = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mapboxResponse{Features: features})
	}))
}

func newTestProvider(t *testing.T, srv *httptest.Server) *MapboxProvider {
	t.Helper()
	base := srv.URL + "/"
	return NewMapboxProvider("test-token", MapboxOptions{BaseURL: base})
}

func TestMapboxProvider_Geocode(t *testing.T) {
	t.Run("resolves at address precision when the first tier answers", func(t *testing.T) {
		srv := mapboxStub(t, map[string][]mapboxFeature{
			"address": {{Center: []float64{-122.27, 37.80}}},
		})
		defer srv.Close()

		p := newTestProvider(t, srv)
		coords, err := p.Geocode(context.Background(), "123 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)
		require.NotNil(t, coords)
		require.Equal(t, PrecisionAddress, coords.Precision)
		require.Equal(t, 37.80, coords.Latitude)
		require.Equal(t, -122.27, coords.Longitude)
	})

	t.Run("degrades to postcode precision when the address tier is empty", func(t *testing.T) {
		srv := mapboxStub(t, map[string][]mapboxFeature{
			"postcode": {{Center: []float64{-122.2, 37.8}}},
		})
		defer srv.Close()

		p := newTestProvider(t, srv)
		coords, err := p.Geocode(context.Background(), "unresolvable rural route", "Oakland", "CA", "94601")
		require.NoError(t, err)
		require.NotNil(t, coords)
		require.Equal(t, PrecisionPostcode, coords.Precision)
	})

	t.Run("degrades to place precision when address and postcode are both empty", func(t *testing.T) {
		srv := mapboxStub(t, map[string][]mapboxFeature{
			"place": {{Center: []float64{-122.2, 37.8}}},
		})
		defer srv.Close()

		p := newTestProvider(t, srv)
		coords, err := p.Geocode(context.Background(), "", "Oakland", "CA", "")
		require.NoError(t, err)
		require.NotNil(t, coords)
		require.Equal(t, PrecisionPlace, coords.Precision)
	})

	t.Run("returns nil, nil when every tier comes back empty", func(t *testing.T) {
		srv := mapboxStub(t, map[string][]mapboxFeature{})
		defer srv.Close()

		p := newTestProvider(t, srv)
		coords, err := p.Geocode(context.Background(), "123 Main St", "Nowhere", "CA", "00000")
		require.NoError(t, err)
		require.Nil(t, coords)
	})

	t.Run("treats a (0,0) answer the same as no result", func(t *testing.T) {
		srv := mapboxStub(t, map[string][]mapboxFeature{
			"address": {{Center: []float64{0, 0}}},
		})
		defer srv.Close()

		p := newTestProvider(t, srv)
		coords, err := p.Geocode(context.Background(), "123 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)
		require.Nil(t, coords, "a (0,0) center must never be handed back as a real fix")
	})

	t.Run("a transient error on the address tier does not cost the postcode centroid", func(t *testing.T) {
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.URL.Query().Get("types") == "address" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mapboxResponse{Features: []mapboxFeature{{Center: []float64{-122.2, 37.8}}}})
		}))
		defer srv.Close()

		p := newTestProvider(t, srv)
		coords, err := p.Geocode(context.Background(), "123 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)
		require.NotNil(t, coords)
		require.Equal(t, PrecisionPostcode, coords.Precision)
		require.GreaterOrEqual(t, calls, 2, "both the address and postcode tiers must have been tried")
	})

	t.Run("returns an error, not nil-nil, when every tier fails outright", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		p := newTestProvider(t, srv)
		coords, err := p.Geocode(context.Background(), "123 Main St", "Oakland", "CA", "94601")
		require.Nil(t, coords)
		require.Error(t, err, "a real failure on every tier must surface as an error, not be reported the same as a clean no-match")
	})

	t.Run("the access token never appears in a transport error", func(t *testing.T) {
		const token = "pk.SECRET-SHOULD-NEVER-LEAK-TOKEN"

		// A server that is immediately closed guarantees a real connection
		// failure (not just a non-200 status) from http.Client.Do, which is
		// the case whose error embeds the full request URL.
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		base := srv.URL + "/"
		srv.Close()

		p := NewMapboxProvider(token, MapboxOptions{BaseURL: base})
		_, err := p.Geocode(context.Background(), "123 Main St", "Oakland", "CA", "94601")
		require.Error(t, err)
		require.NotContains(t, err.Error(), token, "a transport failure must never put the access token into the error text")
	})

	t.Run("query is escaped for special characters", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mapboxResponse{Features: []mapboxFeature{{Center: []float64{-122.2, 37.8}}}})
		}))
		defer srv.Close()

		p := newTestProvider(t, srv)
		_, err := p.Geocode(context.Background(), "1 Foo & Bar Ave", "Oakland", "CA", "94601")
		require.NoError(t, err)
		unescaped, err := url.PathUnescape(gotPath)
		require.NoError(t, err)
		require.Contains(t, unescaped, "1 Foo & Bar Ave")
	})
}
