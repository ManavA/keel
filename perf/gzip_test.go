package perf

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func jsonHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func TestGzip(t *testing.T) {
	t.Run("compresses when the client accepts gzip", func(t *testing.T) {
		wrapped := Gzip(jsonHandler(strings.Repeat("hello world ", 100)))
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		require.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		r, err := gzip.NewReader(w.Body)
		require.NoError(t, err)
		decoded, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, strings.Repeat("hello world ", 100), string(decoded))
	})

	t.Run("passes the body through untouched when the client sends no Accept-Encoding", func(t *testing.T) {
		wrapped := Gzip(jsonHandler("hello"))
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

		require.Empty(t, w.Header().Get("Content-Encoding"))
		require.Equal(t, "hello", w.Body.String())
	})

	t.Run("always sets Vary: Accept-Encoding, even when not compressing", func(t *testing.T) {
		wrapped := Gzip(jsonHandler("hello"))
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		require.Contains(t, w.Header().Values("Vary"), "Accept-Encoding")
	})

	t.Run("does not double-compress an image response", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("fake jpeg bytes"))
		})
		wrapped := Gzip(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		require.Empty(t, w.Header().Get("Content-Encoding"))
		require.Equal(t, "fake jpeg bytes", w.Body.String())
	})

	t.Run("does not compress a response that already set its own Content-Encoding", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "br")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("brotli-encoded-elsewhere"))
		})
		wrapped := Gzip(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		require.Equal(t, "br", w.Header().Get("Content-Encoding"))
		require.Equal(t, "brotli-encoded-elsewhere", w.Body.String())
	})
}
