package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/metrics"
)

// TestObserveCountsRequestsByRoutePattern pins what Observe promises:
// the route pattern labels the series, the concrete path does not, and
// a nil Metrics leaves the response untouched.
func TestObserveCountsRequestsByRoutePattern(t *testing.T) {
	mem := metrics.NewInMemory()
	m := mem.Metrics()

	r := chi.NewRouter()
	r.Use(middleware.Observe(m))
	r.Get("/jobs/{name}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	for _, path := range []string{"/jobs/nightly", "/jobs/hourly"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusTeapot, rec.Code)
	}

	attrs := []metrics.Attr{
		metrics.String(metrics.AttrHTTPRoute, "/jobs/{name}"),
		metrics.String(metrics.AttrHTTPStatus, "418"),
	}
	assert.Equal(t, int64(2), mem.CounterTotal(metrics.NameHTTPRequests, attrs...))
	assert.Len(t, mem.HistogramValues(metrics.NameHTTPRequestDur, attrs...), 2)
	assert.Equal(t, int64(0), mem.CounterTotal(metrics.NameHTTPRequests,
		metrics.String(metrics.AttrHTTPRoute, "/jobs/nightly")),
		"concrete paths must never label the series")
}

// TestObserveNilPassesThrough asserts the unset default changes
// nothing: no recording, and the same status and body.
func TestObserveNilPassesThrough(t *testing.T) {
	h := middleware.Observe(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}
