package middleware_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ManavA/keel/httpx/middleware"
	"github.com/stretchr/testify/assert"
)

func serveWithSecurityHeaders(opts middleware.SecurityHeadersOptions, req *http.Request) *httptest.ResponseRecorder {
	h := middleware.SecurityHeaders(opts)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("sample"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSecurityHeadersDefaults(t *testing.T) {
	rec := serveWithSecurityHeaders(middleware.SecurityHeadersOptions{},
		httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "SAMEORIGIN", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "strict-origin-when-cross-origin", rec.Header().Get("Referrer-Policy"))
	assert.Empty(t, rec.Header().Get("Strict-Transport-Security"),
		"plain HTTP must not carry HSTS, or local development gets pinned to HTTPS")
	assert.Empty(t, rec.Header().Get("Content-Security-Policy"),
		"no single policy suits every page, so none is sent unless configured")
}

func TestSecurityHeadersOverrides(t *testing.T) {
	rec := serveWithSecurityHeaders(middleware.SecurityHeadersOptions{
		FrameOptions:          "DENY",
		ReferrerPolicy:        "same-origin",
		ContentSecurityPolicy: "default-src 'self'",
	}, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "same-origin", rec.Header().Get("Referrer-Policy"))
	assert.Equal(t, "default-src 'self'", rec.Header().Get("Content-Security-Policy"))
	// Untouched options keep their defaults.
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
}

func TestSecurityHeadersHSTSNeedsTLS(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	assert.Empty(t, serveWithSecurityHeaders(middleware.SecurityHeadersOptions{}, plain).
		Header().Get("Strict-Transport-Security"))

	tlsReq := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	rec := serveWithSecurityHeaders(middleware.SecurityHeadersOptions{}, tlsReq)
	assert.Equal(t, "max-age=31536000; includeSubDomains", rec.Header().Get("Strict-Transport-Security"))
}

func TestSecurityHeadersNoHSTSOnLocalhost(t *testing.T) {
	for _, host := range []string{"localhost", "localhost:8080", "app.localhost:3000", "127.0.0.1:8080", "[::1]:8080"} {
		req := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		req.TLS = &tls.ConnectionState{}
		assert.Empty(t, serveWithSecurityHeaders(middleware.SecurityHeadersOptions{}, req).
			Header().Get("Strict-Transport-Security"),
			"host %s must not get HSTS, or local HTTPS work pins the browser", host)
	}
}

func TestSecurityHeadersDashOmitsAHeader(t *testing.T) {
	rec := serveWithSecurityHeaders(middleware.SecurityHeadersOptions{FrameOptions: "-"},
		httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Empty(t, rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"),
		"omitting one header must not drop the others")
}

func TestSecurityHeadersHandlerCanStillOverride(t *testing.T) {
	h := middleware.SecurityHeaders(middleware.SecurityHeadersOptions{})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Frame-Options", "DENY")
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
}
