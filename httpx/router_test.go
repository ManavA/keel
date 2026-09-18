package httpx_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRouter(t *testing.T, opts httpx.RouterOptions) *chi.Mux {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = log.New(log.Options{Output: io.Discard})
	}
	return httpx.NewRouter(opts)
}

func TestRouterServes(t *testing.T) {
	r := httpx.NewRouter(httpx.RouterOptions{Logger: log.New(log.Options{Output: io.Discard})})
	r.Get("/things", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/things", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, rec.Header().Get(middleware.RequestIDHeader))
}

func TestRouterAnswersHEADWhereverItAnswersGET(t *testing.T) {
	// chi returns 405 for HEAD on a GET route. RFC 9110 says HEAD is available
	// wherever GET is, and uptime checks and `curl -sI` both use it — a 405
	// there reads as a broken endpoint.
	r := httpx.NewRouter(httpx.RouterOptions{Logger: log.New(log.Options{Output: io.Discard})})
	r.Get("/things", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body"))
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/things", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRouterNotFoundAndMethodNotAllowedAreJSON(t *testing.T) {
	r := newTestRouter(t, httpx.RouterOptions{})
	r.Get("/things", func(http.ResponseWriter, *http.Request) {})

	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"unknown path", http.MethodGet, "/nope", http.StatusNotFound},
		{"wrong method", http.MethodPost, "/things", http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			assert.Equal(t, tt.status, rec.Code)
			var body httpx.ErrorBody
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body),
				"chi's own default is plain text with no request id; body was %q", rec.Body.String())
			assert.NotEmpty(t, body.Error)
			assert.NotEmpty(t, body.RequestID)
		})
	}
}

func TestRouterRecoversFromAPanic(t *testing.T) {
	r := httpx.NewRouter(httpx.RouterOptions{Logger: log.New(log.Options{Output: io.Discard})})
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("secret detail") })

	rec := httptest.NewRecorder()
	assert.NotPanics(t, func() {
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret detail")
}

func TestRouterRateLimit(t *testing.T) {
	r := httpx.NewRouter(httpx.RouterOptions{
		Logger:    log.New(log.Options{Output: io.Discard}),
		RateLimit: &middleware.RateLimitOptions{Requests: 1, Window: time.Minute},
	})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})

	send := func() int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.77:1"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	assert.Equal(t, http.StatusOK, send())
	assert.Equal(t, http.StatusTooManyRequests, send())
}

func TestRouterRealIPRunsBeforeTheRateLimiter(t *testing.T) {
	// The ordering that decides whether a rate limit means anything behind a
	// proxy: with them the wrong way round every client shares one bucket.
	var seenRemote string
	r := httpx.NewRouter(httpx.RouterOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
		RealIP: middleware.RealIPOptions{TrustedProxies: []string{"10.0.0.0/8"}},
		RateLimit: &middleware.RateLimitOptions{
			Requests: 100,
			Window:   time.Minute,
			Key: func(req *http.Request) (string, error) {
				seenRemote = req.RemoteAddr
				return req.RemoteAddr, nil
			},
		},
	})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:1111"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.5")
	r.ServeHTTP(httptest.NewRecorder(), req)

	assert.Contains(t, seenRemote, "203.0.113.9")
}

func TestRouterCORS(t *testing.T) {
	r := httpx.NewRouter(httpx.RouterOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
		CORS:   &middleware.CORSOptions{AllowedOrigins: []string{"https://app.example.com"}},
	})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestRouterSkipRequestLog(t *testing.T) {
	var buf writeCounter
	r := httpx.NewRouter(httpx.RouterOptions{
		Logger:         log.New(log.Options{Output: &buf}),
		SkipRequestLog: true,
	})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Zero(t, buf.n)
}

type writeCounter struct{ n int }

func (w *writeCounter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}
