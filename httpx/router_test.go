package httpx_test

import (
	"bytes"
	"encoding/json"
	"errors"
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
	r, err := httpx.NewRouter(opts)
	require.NoError(t, err)
	return r
}

func TestRouterServes(t *testing.T) {
	r := newTestRouter(t, httpx.RouterOptions{Logger: log.New(log.Options{Output: io.Discard})})
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
	r := newTestRouter(t, httpx.RouterOptions{Logger: log.New(log.Options{Output: io.Discard})})
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
	r := newTestRouter(t, httpx.RouterOptions{Logger: log.New(log.Options{Output: io.Discard})})
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("secret detail") })

	rec := httptest.NewRecorder()
	assert.NotPanics(t, func() {
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret detail")
}

func TestRouterRateLimit(t *testing.T) {
	r := newTestRouter(t, httpx.RouterOptions{
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
	// With these the wrong way round, every client behind the proxy shares one
	// bucket.
	var seenRemote string
	r := newTestRouter(t, httpx.RouterOptions{
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
	r := newTestRouter(t, httpx.RouterOptions{
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

func TestRouterSecurityHeadersByDefault(t *testing.T) {
	r := newTestRouter(t, httpx.RouterOptions{})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "SAMEORIGIN", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "strict-origin-when-cross-origin", rec.Header().Get("Referrer-Policy"))
	assert.Empty(t, rec.Header().Get("Strict-Transport-Security"))
}

func TestRouterSecurityHeadersOverrideAndSkip(t *testing.T) {
	r := newTestRouter(t, httpx.RouterOptions{
		SecurityHeaders: &middleware.SecurityHeadersOptions{FrameOptions: "DENY"},
	})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))

	r = newTestRouter(t, httpx.RouterOptions{SkipSecurityHeaders: true})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Empty(t, rec.Header().Get("X-Content-Type-Options"))
	assert.Empty(t, rec.Header().Get("X-Frame-Options"))
	assert.Empty(t, rec.Header().Get("Referrer-Policy"))
}

func TestRouterRefusesWildcardOriginWithCredentials(t *testing.T) {
	// Browsers reject Access-Control-Allow-Origin "*" paired with credentials,
	// so the behaviour differs by environment: curl works, every browser fails.
	// The constructor must refuse the pairing instead of deploying it.
	_, err := httpx.NewRouter(httpx.RouterOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
		CORS: &middleware.CORSOptions{
			AllowedOrigins:   []string{"*"},
			AllowCredentials: true,
		},
	})
	require.Error(t, err, "wildcard origin with credentials must be a configuration error")
	assert.Contains(t, err.Error(), "*")

	r, err := httpx.NewRouter(httpx.RouterOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
		CORS:   &middleware.CORSOptions{AllowedOrigins: []string{"*"}},
	})
	require.NoError(t, err)
	require.NotNil(t, r)

	r, err = httpx.NewRouter(httpx.RouterOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
		CORS: &middleware.CORSOptions{
			AllowedOrigins:   []string{"https://app.example.com"},
			AllowCredentials: true,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestRouterSkipRequestLog(t *testing.T) {
	var buf writeCounter
	r := newTestRouter(t, httpx.RouterOptions{
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

func TestRouterInstallsItsLoggerOnTheRequest(t *testing.T) {
	// The error helpers read the logger off the request context, and the router
	// is what puts it there. Building the context by hand in a test would leave
	// that wiring unchecked.
	var buf bytes.Buffer
	logger := log.New(log.Options{Output: &buf})

	r := newTestRouter(t, httpx.RouterOptions{Logger: logger, SkipRequestLog: true})
	r.Get("/thing", func(w http.ResponseWriter, req *http.Request) {
		assert.Same(t, logger, httpx.Logger(req.Context()))
		httpx.InternalError(w, req, errors.New("handler reason"))
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thing", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, buf.String(), "handler reason")
}

func TestRouterWarnsWhenRateLimitIsBlindBehindAProxy(t *testing.T) {
	// A default-key limit with RealIP unconfigured buckets every client
	// behind a proxy together; the startup log is where that misconfiguration
	// surfaces, because the 429s it produces read as abuse.
	var buf bytes.Buffer
	r := newTestRouter(t, httpx.RouterOptions{
		Logger:    log.New(log.Options{Output: &buf}),
		RateLimit: &middleware.RateLimitOptions{Requests: 100, Window: time.Minute},
	})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})
	assert.Contains(t, buf.String(), "shares one bucket")

	var quiet bytes.Buffer
	r = newTestRouter(t, httpx.RouterOptions{
		Logger:    log.New(log.Options{Output: &quiet}),
		RealIP:    middleware.RealIPOptions{TrustedProxies: []string{"10.0.0.0/8"}},
		RateLimit: &middleware.RateLimitOptions{Requests: 100, Window: time.Minute},
	})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})
	assert.NotContains(t, quiet.String(), "shares one bucket")

	var norate bytes.Buffer
	r = newTestRouter(t, httpx.RouterOptions{
		Logger: log.New(log.Options{Output: &norate}),
	})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})
	assert.NotContains(t, norate.String(), "shares one bucket")
}

func TestRouterNotFoundLogsThroughItsLogger(t *testing.T) {
	// The 404 and 405 the router answers itself go through the same logger.
	var buf bytes.Buffer
	r := newTestRouter(t, httpx.RouterOptions{
		Logger:         log.New(log.Options{Output: &buf}),
		SkipRequestLog: true,
	})
	// chi short-circuits to NotFound without running the middleware chain when
	// no route is registered at all, so register one.
	r.Get("/thing", func(http.ResponseWriter, *http.Request) {})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/absent", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, buf.String(), "/absent")
	assert.Contains(t, buf.String(), log.RequestIDKey)
}
