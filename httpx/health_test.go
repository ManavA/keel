package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body was %q", rec.Body.String())
	return rec, body
}

func TestLivenessIgnoresChecks(t *testing.T) {
	// Liveness must not consult a dependency, or a database blip restarts every
	// instance at once.
	var ran atomic.Int32
	h := httpx.Health(httpx.HealthOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
		Checks: map[string]httpx.Check{
			"database": func(context.Context) error {
				ran.Add(1)
				return errors.New("down")
			},
		},
	})

	rec, body := get(t, h, "/healthz")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", body["status"])
	assert.Zero(t, ran.Load(), "liveness ran a dependency check")
}

func TestReadinessRunsChecks(t *testing.T) {
	h := httpx.Health(httpx.HealthOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
		Checks: map[string]httpx.Check{
			"database": func(context.Context) error { return nil },
			"search":   func(context.Context) error { return nil },
		},
	})

	rec, body := get(t, h, "/readyz")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", body["status"])

	checks, _ := body["checks"].(map[string]any)
	assert.Equal(t, "ok", checks["database"])
	assert.Equal(t, "ok", checks["search"])
}

func TestReadinessFails(t *testing.T) {
	h := httpx.Health(httpx.HealthOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
		Checks: map[string]httpx.Check{
			"database": func(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: refused") },
			"search":   func(context.Context) error { return nil },
		},
	})

	rec, body := get(t, h, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "degraded", body["status"])

	checks, _ := body["checks"].(map[string]any)
	assert.Equal(t, "down", checks["database"])
	assert.Equal(t, "ok", checks["search"])
	assert.NotContains(t, rec.Body.String(), "10.0.0.5",
		"a driver error names hosts and must not be published by default")
}

func TestReadinessCanExposeCheckErrors(t *testing.T) {
	h := httpx.Health(httpx.HealthOptions{
		Logger:            log.New(log.Options{Output: io.Discard}),
		ExposeCheckErrors: true,
		Checks: map[string]httpx.Check{
			"database": func(context.Context) error { return errors.New("connection refused") },
		},
	})

	_, body := get(t, h, "/readyz")
	checks, _ := body["checks"].(map[string]any)
	assert.Equal(t, "down: connection refused", checks["database"])
}

func TestReadinessLogsTheFailure(t *testing.T) {
	var buf bytes.Buffer
	h := httpx.Health(httpx.HealthOptions{
		Logger: log.New(log.Options{Output: &buf}),
		Checks: map[string]httpx.Check{
			"database": func(context.Context) error { return errors.New("connection refused") },
		},
	})

	get(t, h, "/readyz")
	assert.Contains(t, buf.String(), "readiness check failed")
	assert.Contains(t, buf.String(), "connection refused")
}

func TestReadinessCaches(t *testing.T) {
	var ran atomic.Int32
	h := httpx.Health(httpx.HealthOptions{
		Logger:   log.New(log.Options{Output: io.Discard}),
		CacheTTL: time.Hour,
		Checks: map[string]httpx.Check{
			"database": func(context.Context) error {
				ran.Add(1)
				return nil
			},
		},
	})

	for range 5 {
		get(t, h, "/readyz")
	}
	assert.Equal(t, int32(1), ran.Load(),
		"every prober you own polls this, and without a cache that is a query load of its own")
}

func TestReadinessCacheExpires(t *testing.T) {
	var ran atomic.Int32
	h := httpx.Health(httpx.HealthOptions{
		Logger:   log.New(log.Options{Output: io.Discard}),
		CacheTTL: time.Millisecond,
		Checks: map[string]httpx.Check{
			"database": func(context.Context) error {
				ran.Add(1)
				return nil
			},
		},
	})

	get(t, h, "/readyz")
	time.Sleep(5 * time.Millisecond)
	get(t, h, "/readyz")
	assert.Equal(t, int32(2), ran.Load())
}

func TestReadinessTimesOut(t *testing.T) {
	h := httpx.Health(httpx.HealthOptions{
		Logger:  log.New(log.Options{Output: io.Discard}),
		Timeout: 10 * time.Millisecond,
		Checks: map[string]httpx.Check{
			"slow": func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		},
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		done <- rec
	}()

	select {
	case rec := <-done:
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	case <-time.After(2 * time.Second):
		t.Fatal("a readiness check that can hang leaves the prober's own timeout in charge")
	}
}

func TestHealthReportsTheBuild(t *testing.T) {
	h := httpx.Health(httpx.HealthOptions{Logger: log.New(log.Options{Output: io.Discard})})

	for _, path := range []string{"/healthz", "/readyz"} {
		rec, body := get(t, h, path)
		build, ok := body["build"].(map[string]any)
		require.True(t, ok, "%s must always carry a build, even when it is unknown", path)
		assert.NotEmpty(t, build["revision"])
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	}
}

func TestHealthCustomPaths(t *testing.T) {
	h := httpx.Health(httpx.HealthOptions{
		Logger:        log.New(log.Options{Output: io.Discard}),
		LivenessPath:  "/live",
		ReadinessPath: "/ready",
	})

	rec, _ := get(t, h, "/live")
	assert.Equal(t, http.StatusOK, rec.Code)

	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusNotFound, missing.Code)
}

func TestHealthRejectsNonGET(t *testing.T) {
	h := httpx.Health(httpx.HealthOptions{Logger: log.New(log.Options{Output: io.Discard})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func init() { slog.SetDefault(log.New(log.Options{Output: io.Discard})) }

func TestReadinessDoesNotCacheADisconnectedProber(t *testing.T) {
	// A prober that hangs up mid-check makes every check return
	// context.Canceled. Deriving the check context from the request, and then
	// caching the result, takes the instance out of rotation for CacheTTL with
	// a perfectly healthy database.
	var ran atomic.Int32
	h := httpx.Health(httpx.HealthOptions{
		Logger:   log.New(log.Options{Output: io.Discard}),
		CacheTTL: time.Hour,
		Checks: map[string]httpx.Check{
			"database": func(ctx context.Context) error {
				ran.Add(1)
				// Fails only if the request's cancellation reached it.
				return ctx.Err()
			},
		},
	})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(cancelled))
	assert.Equal(t, http.StatusOK, first.Code,
		"the checks must run on a context detached from the request")

	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, int32(1), ran.Load(), "a healthy result is still cached")
}

func TestReadinessDoesNotCacheACancelledCheck(t *testing.T) {
	// The second guard: a check that reports context.Canceled says nothing
	// about the dependency, so the answer must not be remembered.
	var calls atomic.Int32
	h := httpx.Health(httpx.HealthOptions{
		Logger:   log.New(log.Options{Output: io.Discard}),
		CacheTTL: time.Hour,
		Checks: map[string]httpx.Check{
			"database": func(context.Context) error {
				if calls.Add(1) == 1 {
					return context.Canceled
				}
				return nil
			},
		},
	})

	first, _ := get(t, h, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, first.Code)

	second, _ := get(t, h, "/readyz")
	assert.Equal(t, http.StatusOK, second.Code, "the cancelled result must not have been cached")
	assert.Equal(t, int32(2), calls.Load())
}

func TestReadinessCachesARealTimeout(t *testing.T) {
	// A deadline is evidence about the dependency, unlike a cancellation, so it
	// is cached like any other failure.
	var calls atomic.Int32
	h := httpx.Health(httpx.HealthOptions{
		Logger:   log.New(log.Options{Output: io.Discard}),
		CacheTTL: time.Hour,
		Timeout:  10 * time.Millisecond,
		Checks: map[string]httpx.Check{
			"slow": func(ctx context.Context) error {
				calls.Add(1)
				<-ctx.Done()
				return ctx.Err()
			},
		},
	})

	first, _ := get(t, h, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, first.Code)
	second, _ := get(t, h, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, second.Code)
	assert.Equal(t, int32(1), calls.Load())
}
