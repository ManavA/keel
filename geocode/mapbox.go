package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultMapboxBaseURL is overridden in tests via MapboxOptions.BaseURL.
const defaultMapboxBaseURL = "https://api.mapbox.com/geocoding/v5/mapbox.places/"

// MapboxOptions configures NewMapboxProvider. The zero value is a working
// default: a 5-second HTTP timeout against the real Mapbox API.
type MapboxOptions struct {
	HTTPClient *http.Client
	BaseURL    string
}

func (o MapboxOptions) withDefaults() MapboxOptions {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if o.BaseURL == "" {
		o.BaseURL = defaultMapboxBaseURL
	}
	return o
}

// MapboxProvider geocodes through the Mapbox Geocoding API.
type MapboxProvider struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

// NewMapboxProvider creates a Provider backed by Mapbox.
func NewMapboxProvider(apiKey string, opts MapboxOptions) *MapboxProvider {
	opts = opts.withDefaults()
	return &MapboxProvider{apiKey: apiKey, httpClient: opts.HTTPClient, baseURL: opts.BaseURL}
}

// geocodeTier is one attempt at resolving an address, from most to least
// precise.
type geocodeTier struct {
	precision string
	types     string
	query     func(address, city, state, postalCode string) string
}

// tiers degrades from an exact address match to a postal-code or city
// centroid instead of returning no result.
//
// A request restricted to types=address returns no result for an address it
// cannot match exactly: a rural route, new construction, a bare lot, or
// malformed input. A postal-code or city centroid is not the exact
// location, but it is close enough for a map, a distance calculation, or a
// commute-time lookup to work. Coordinates.Precision records which tier
// produced the result, so a caller can distinguish a centroid from an exact
// match.
var tiers = []geocodeTier{
	{
		precision: PrecisionAddress,
		types:     "address",
		query: func(address, city, state, postalCode string) string {
			if address == "" {
				return ""
			}
			return joinNonEmpty(address, city, strings.TrimSpace(state+" "+postalCode))
		},
	},
	{
		precision: PrecisionPostcode,
		types:     "postcode",
		query: func(_, city, state, postalCode string) string {
			if postalCode == "" {
				return ""
			}
			return joinNonEmpty(city, strings.TrimSpace(state+" "+postalCode))
		},
	},
	{
		precision: PrecisionPlace,
		types:     "place",
		query: func(_, city, state, _ string) string {
			if city == "" {
				return ""
			}
			return joinNonEmpty(city, state)
		},
	},
}

func joinNonEmpty(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

// Geocode implements Provider, degrading from an exact address to a postal
// centroid to a place centroid rather than returning nothing. A nil result
// with a nil error means every tier came back empty.
func (p *MapboxProvider) Geocode(ctx context.Context, address, city, state, postalCode string) (*Coordinates, error) {
	var firstErr error

	for _, tier := range tiers {
		q := tier.query(address, city, state, postalCode)
		if q == "" {
			continue
		}

		coords, err := p.lookup(ctx, q, tier.types)
		if err != nil {
			// Remember the first failure but keep degrading — a transient
			// error on the address tier should not cost the postal centroid.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if coords == nil {
			continue
		}

		coords.Precision = tier.precision
		return coords, nil
	}

	return nil, firstErr
}

func (p *MapboxProvider) lookup(ctx context.Context, query, types string) (*Coordinates, error) {
	reqURL := fmt.Sprintf("%s%s.json?access_token=%s&country=US&types=%s&limit=1",
		p.baseURL, url.PathEscape(query), url.QueryEscape(p.apiKey), types)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("geocode: build request: %w", sanitizeTransportErr(err))
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geocode: request failed: %w", sanitizeTransportErr(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("geocode: status %d", resp.StatusCode)
	}

	var result mapboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("geocode: decode response: %w", err)
	}

	if len(result.Features) == 0 {
		return nil, nil
	}

	// Mapbox returns [longitude, latitude].
	center := result.Features[0].Center
	if len(center) < 2 {
		return nil, nil
	}

	// (0,0) is the sentinel this whole package treats as "not geocoded". If
	// Mapbox ever answers with it, that is not a usable fix — treat it the
	// same as no result.
	if center[0] == 0 && center[1] == 0 {
		return nil, nil
	}

	return &Coordinates{Longitude: center[0], Latitude: center[1]}, nil
}

// sanitizeTransportErr removes the request URL from a transport-level
// error. The access token is sent as a query parameter (Mapbox's API does
// not accept it as a header), and both http.NewRequestWithContext (via
// url.Parse) and http.Client.Do return a *url.Error whose Error() method
// renders the full request URL, token included. Every transport failure
// would otherwise put the token into whatever log records the error.
func sanitizeTransportErr(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

type mapboxResponse struct {
	Features []mapboxFeature `json:"features"`
}

type mapboxFeature struct {
	Center []float64 `json:"center"`
}
