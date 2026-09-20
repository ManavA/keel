package middleware

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/metrics"
)

// Observe records one count and one duration per request into m, labeled
// with the matched route pattern and the status code. A nil m skips the
// recording entirely, so leaving it unset costs nothing.
//
// The route is the chi route pattern ("/jobs/{name}"), not the concrete
// path: concrete paths have unbounded cardinality and make one time
// series per id. Outside a chi router there is no pattern, and the path
// itself is recorded instead.
//
// Mount it above Recoverer (as NewRouter does), so a recovered panic is
// recorded as the 500 the client actually receives rather than not at
// all.
func Observe(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m == nil {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			route := r.URL.Path
			if rc := chi.RouteContext(r.Context()); rc != nil {
				if pattern := rc.RoutePattern(); pattern != "" {
					route = pattern
				}
			}
			m.ObserveHTTP(r.Context(), route, rec.statusOrOK(), time.Since(start))
		})
	}
}
