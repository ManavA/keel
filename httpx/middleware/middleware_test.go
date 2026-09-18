package middleware_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestIDGeneratesOne(t *testing.T) {
	var fromContext string
	h := middleware.RequestID(middleware.RequestIDOptions{})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			fromContext = log.RequestID(r.Context())
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.NotEmpty(t, fromContext)
	assert.Equal(t, fromContext, rec.Header().Get(middleware.RequestIDHeader),
		"the id in the context and the one on the response must be the same string")
}

func TestRequestIDIsDifferentEachTime(t *testing.T) {
	h := middleware.RequestID(middleware.RequestIDOptions{})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	seen := map[string]bool{}
	for range 100 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		id := rec.Header().Get(middleware.RequestIDHeader)
		require.False(t, seen[id], "id %q was issued twice", id)
		seen[id] = true
	}
}

func TestRequestIDInbound(t *testing.T) {
	tests := []struct {
		name         string
		trustInbound bool
		sent         string
		want         string
	}{
		{name: "ignored unless trusted", sent: "caller-supplied", want: ""},
		{name: "accepted when trusted", trustInbound: true, sent: "caller-supplied", want: "caller-supplied"},
		{
			name:         "sanitised when trusted",
			trustInbound: true,
			sent:         "a b<script>/../\x00c",
			want:         "abscript..c",
		},
		{
			name:         "length capped",
			trustInbound: true,
			sent:         strings.Repeat("x", 200),
			want:         strings.Repeat("x", 64),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := middleware.RequestID(middleware.RequestIDOptions{TrustInbound: tt.trustInbound})(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(middleware.RequestIDHeader, tt.sent)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(middleware.RequestIDHeader)
			if tt.want == "" {
				assert.NotEqual(t, tt.sent, got)
				assert.NotEmpty(t, got)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRecovererTurnsAPanicIntoA500(t *testing.T) {
	var buf bytes.Buffer
	h := middleware.Recoverer(middleware.RecovererOptions{
		Logger: log.New(log.Options{Output: &buf, Level: slog.LevelError}),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("the database is on fire and the password is hunter2")
	}))

	rec := httptest.NewRecorder()
	assert.NotPanics(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	})

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "hunter2", "a panic message must not reach the caller")
	assert.NotContains(t, rec.Body.String(), "on fire")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "internal server error", body["error"])

	assert.Contains(t, buf.String(), "panic in handler")
	assert.Contains(t, buf.String(), "on fire", "the log is where the detail belongs")
}

func TestRecovererCallsOnPanic(t *testing.T) {
	var got any
	h := middleware.Recoverer(middleware.RecovererOptions{
		Logger:  log.New(log.Options{Output: io.Discard}),
		OnPanic: func(_ *http.Request, recovered any, _ []byte) { got = recovered },
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("x") }))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "x", got)
}

func TestRecovererRepanicsAbortHandler(t *testing.T) {
	h := middleware.Recoverer(middleware.RecovererOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	assert.PanicsWithError(t, http.ErrAbortHandler.Error(), func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
}

func TestRecovererLeavesAPartialResponseAlone(t *testing.T) {
	h := middleware.Recoverer(middleware.RecovererOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("partial"))
		panic("too late")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, "partial", rec.Body.String())
}

func TestRecovererPassesThroughWhenNothingPanics(t *testing.T) {
	h := middleware.Recoverer(middleware.RecovererOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("fine"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "fine", rec.Body.String())
}

func TestRequestLog(t *testing.T) {
	var buf bytes.Buffer
	h := middleware.RequestLog(middleware.RequestLogOptions{
		Logger: log.New(log.Options{Output: &buf}),
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/things?page=2&token=abc123", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "POST", rec["method"])
	assert.Equal(t, "/things", rec["path"])
	assert.Equal(t, float64(201), rec["status"])
	assert.Equal(t, float64(5), rec["bytes"])
	assert.Contains(t, rec["query"], "page=2")
	assert.NotContains(t, rec["query"], "abc123",
		"a token in the query string is a single-use credential and must not be logged")
}

func TestRequestLogDefaultsTo200(t *testing.T) {
	var buf bytes.Buffer
	h := middleware.RequestLog(middleware.RequestLogOptions{
		Logger: log.New(log.Options{Output: &buf}),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, float64(200), rec["status"])
}

func TestRequestLogLevels(t *testing.T) {
	tests := []struct {
		name   string
		status int
		slow   time.Duration
		want   string
	}{
		{name: "ok", status: http.StatusOK, want: "INFO"},
		{name: "client error stays info", status: http.StatusNotFound, want: "INFO"},
		{name: "server error is an error", status: http.StatusInternalServerError, want: "ERROR"},
		{name: "slow is a warning", status: http.StatusOK, slow: time.Nanosecond, want: "WARN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := middleware.RequestLog(middleware.RequestLogOptions{
				Logger:      log.New(log.Options{Output: &buf}),
				SlowRequest: tt.slow,
			})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))

			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

			var rec map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
			assert.Equal(t, tt.want, rec["level"])
		})
	}
}

func TestRequestLogSkip(t *testing.T) {
	var buf bytes.Buffer
	h := middleware.RequestLog(middleware.RequestLogOptions{
		Logger: log.New(log.Options{Output: &buf}),
		Skip:   func(r *http.Request) bool { return r.URL.Path == "/healthz" },
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Empty(t, buf.String())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/other", nil))
	assert.NotEmpty(t, buf.String())
}

func TestRedactQuery(t *testing.T) {
	got := middleware.RedactQuery(url.Values{
		"page":     {"2"},
		"token":    {"abc"},
		"password": {"hunter2"},
		"sort":     {"created_at"},
	})
	assert.Contains(t, got, "page=2")
	assert.Contains(t, got, "sort=created_at")
	assert.NotContains(t, got, "abc")
	assert.NotContains(t, got, "hunter2")
	assert.Empty(t, middleware.RedactQuery(nil))
}

func TestRateLimit(t *testing.T) {
	h := middleware.RateLimit(middleware.RateLimitOptions{
		Requests: 2,
		Window:   time.Minute,
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	send := func(remote string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusOK, send("203.0.113.1:1"))
	assert.Equal(t, http.StatusOK, send("203.0.113.1:2"))
	assert.Equal(t, http.StatusTooManyRequests, send("203.0.113.1:3"))

	// A different client has its own budget, or the limiter is a global switch
	// rather than a rate limit.
	assert.Equal(t, http.StatusOK, send("203.0.113.2:1"))
}

func TestRateLimitIgnoresForwardingHeaders(t *testing.T) {
	// RealIP has already decided what to believe. A limiter that re-reads the
	// raw header hands every client a fresh bucket per request.
	h := middleware.RateLimit(middleware.RateLimitOptions{
		Requests: 1,
		Window:   time.Minute,
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	send := func(forwarded string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.1:1"
		req.Header.Set("X-Forwarded-For", forwarded)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusOK, send("1.1.1.1"))
	assert.Equal(t, http.StatusTooManyRequests, send("2.2.2.2"))
}

func TestKeyByHeader(t *testing.T) {
	key := middleware.KeyByHeader("X-Api-Key")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.1:1"
	req.Header.Set("X-Api-Key", "abc")
	withHeader, err := key(req)
	require.NoError(t, err)
	assert.Contains(t, withHeader, "abc")

	req.Header.Del("X-Api-Key")
	withoutHeader, err := key(req)
	require.NoError(t, err)
	assert.Contains(t, withoutHeader, "203.0.113.1",
		"a request with no key falls back to its address rather than joining a shared bucket")
}

func TestCORS(t *testing.T) {
	h := middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{"https://app.example.com"},
		ExposedHeaders: []string{"X-Total-Count"},
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), "X-Total-Count")

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORSPreflight(t *testing.T) {
	h := middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{"https://app.example.com"},
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a preflight must never reach the handler")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), http.MethodPost)
}

// errReader lets ReadFrom be exercised through the recorder.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestRecorderReadFromReportsTheError(t *testing.T) {
	want := errors.New("boom")
	h := middleware.RequestLog(middleware.RequestLogOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.Copy(w, errReader{err: want})
		assert.ErrorIs(t, err, want)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestCORSZeroValueAllowsNothing(t *testing.T) {
	// go-chi/cors reads an empty AllowedOrigins as ["*"], so an unset origin
	// list would otherwise turn a deployment's policy into every origin.
	h := middleware.CORS(middleware.CORSOptions{})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "the request still reaches the handler")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
		"no origin is allowed, so the browser must get no allow header")
}

func TestCORSEmptyConfiguredListAllowsNothing(t *testing.T) {
	h := middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{},
		ExposedHeaders: []string{"X-Total-Count"},
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, rec.Header().Get("Access-Control-Expose-Headers"))
}

func TestCORSRefusesWildcardWithCredentials(t *testing.T) {
	assert.Panics(t, func() {
		middleware.CORS(middleware.CORSOptions{
			AllowedOrigins:   []string{"*"},
			AllowCredentials: true,
		})
	}, "browsers reject this pairing, so it must not be allowed to deploy")

	assert.NotPanics(t, func() {
		middleware.CORS(middleware.CORSOptions{AllowedOrigins: []string{"*"}})
	})
	assert.NotPanics(t, func() {
		middleware.CORS(middleware.CORSOptions{
			AllowedOrigins:   []string{"https://app.example.com"},
			AllowCredentials: true,
		})
	})
}

func TestRateLimitRefusesAConfigurationThatWouldDoNothing(t *testing.T) {
	// A forgotten Window makes httprate admit everything, which is a limiter
	// that never says it is not working.
	tests := []struct {
		name string
		opts middleware.RateLimitOptions
	}{
		{"nothing set", middleware.RateLimitOptions{}},
		{"window forgotten", middleware.RateLimitOptions{Requests: 1}},
		{"requests forgotten", middleware.RateLimitOptions{Window: time.Minute}},
		{"negative window", middleware.RateLimitOptions{Requests: 1, Window: -time.Second}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Panics(t, func() { middleware.RateLimit(tt.opts) })
		})
	}

	assert.NotPanics(t, func() {
		middleware.RateLimit(middleware.RateLimitOptions{Requests: 1, Window: time.Minute})
	})
}

func TestRedactQueryCoversSingleUseCredentials(t *testing.T) {
	// The spellings the request-log doc claims to cover: an OAuth code, a
	// pre-signed URL's signature, a reset link, a one-time password.
	got := middleware.RedactQuery(url.Values{
		"code":      {"oauth-code"},
		"signature": {"sig-value"},
		"sig":       {"short-sig"},
		"otp":       {"123456"},
		"reset":     {"reset-token"},
		"token":     {"bearer-value"},
		"page":      {"2"},
		"status":    {"open"},
	})

	for _, leaked := range []string{"oauth-code", "sig-value", "short-sig", "123456", "reset-token", "bearer-value"} {
		assert.NotContains(t, got, leaked)
	}
	assert.Contains(t, got, "page=2")
	assert.Contains(t, got, "status=open")
}

func TestRecorderKeepsTheResponseWriterUsable(t *testing.T) {
	// A wrapper implementing only ResponseWriter removes streaming, breaks
	// websocket upgrades and turns io.Copy into a byte-by-byte loop.
	var (
		sawFlusher  bool
		sawHijacker bool
		deadlineErr error
	)

	srv := httptest.NewServer(middleware.RequestLog(middleware.RequestLogOptions{
		Logger: log.New(log.Options{Output: io.Discard}),
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		sawFlusher = ok
		if ok {
			_, _ = w.Write([]byte("chunk"))
			f.Flush()
		}
		_, sawHijacker = w.(http.Hijacker)
		// SetReadDeadline is not on our wrapper, so it can only be reached
		// through Unwrap.
		deadlineErr = http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Minute))
	})))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, "chunk", string(body))
	assert.True(t, sawFlusher, "Flush delegation is gone, which breaks streaming responses")
	assert.True(t, sawHijacker, "Hijack delegation is gone, which breaks websocket upgrades")
	assert.NoError(t, deadlineErr, "Unwrap is gone, so http.ResponseController cannot reach the real writer")
}
