package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/jobs"
	"github.com/ManavA/keel/metrics"
)

// TestAcceptance_HTTPAndJobCounters is issue #25's acceptance check: a
// request through the httpx middleware and a fake job outcome must move the
// documented counters with the documented attribute names.
func TestAcceptance_HTTPAndJobCounters(t *testing.T) {
	mem := metrics.NewInMemory()
	m := mem.Metrics()

	r := chi.NewRouter()
	r.Use(middleware.Observe(m))
	r.Get("/jobs/{name}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jobs/nightly", nil))
	require.Equal(t, http.StatusTeapot, rec.Code)

	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameHTTPRequests,
		metrics.String(metrics.AttrHTTPRoute, "/jobs/{name}"),
		metrics.String(metrics.AttrHTTPStatus, "418")),
		"one request counted under the route pattern, not the concrete path")
	assert.Len(t, mem.HistogramValues(metrics.NameHTTPRequestDur,
		metrics.String(metrics.AttrHTTPRoute, "/jobs/{name}"),
		metrics.String(metrics.AttrHTTPStatus, "418")), 1,
		"the same request recorded once in the duration histogram")

	runner := jobs.NewRunner(jobs.RunnerOptions{Metrics: m})
	code := runner.Run(context.Background(), "nightly", func(context.Context) (jobs.Outcome, error) {
		return jobs.Outcome{Attempted: 2, Succeeded: 2}, nil
	})
	require.Equal(t, 0, code)

	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameJobRuns,
		metrics.String(metrics.AttrJobName, "nightly"),
		metrics.String(metrics.AttrJobStatus, jobs.StatusSuccess)),
		"the fake job's outcome counted once under its status word")
}
