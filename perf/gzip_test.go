package perf

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// largeBody is comfortably over MinCompressBytes, so a test using it isolates
// the behavior under test from the size threshold.
func largeBody() string {
	return strings.Repeat("hello world ", 200) // 2400 bytes
}

func gzipRequest() *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	return req
}

func TestGzip(t *testing.T) {
	t.Run("compresses a response over MinCompressBytes when the client accepts gzip", func(t *testing.T) {
		body := largeBody()
		wrapped := Gzip(jsonHandler(body))
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, gzipRequest())

		require.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		r, err := gzip.NewReader(w.Body)
		require.NoError(t, err)
		decoded, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, body, string(decoded))
	})

	t.Run("passes the body through untouched when the client sends no Accept-Encoding", func(t *testing.T) {
		wrapped := Gzip(jsonHandler(largeBody()))
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

		require.Empty(t, w.Header().Get("Content-Encoding"))
		require.Equal(t, largeBody(), w.Body.String())
	})

	t.Run("always sets Vary: Accept-Encoding, even when not compressing", func(t *testing.T) {
		wrapped := Gzip(jsonHandler("hello"))
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		require.Contains(t, w.Header().Values("Vary"), "Accept-Encoding")
	})

	t.Run("does not compress a response under MinCompressBytes", func(t *testing.T) {
		wrapped := Gzip(jsonHandler("short"))
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, gzipRequest())

		require.Empty(t, w.Header().Get("Content-Encoding"), "a response this small must not be compressed")
		require.Equal(t, "short", w.Body.String())
	})

	t.Run("does not double-compress a large image response with an explicit Content-Type", func(t *testing.T) {
		body := strings.Repeat("x", 2000)
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		})
		wrapped := Gzip(handler)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, gzipRequest())

		require.Empty(t, w.Header().Get("Content-Encoding"))
		require.Equal(t, body, w.Body.String())
	})

	t.Run("does not compress a large image response whose Content-Type is only sniffed, not set", func(t *testing.T) {
		// A real PNG signature followed by padding, so http.DetectContentType
		// (which Gzip must run itself, since the handler never sets
		// Content-Type) reports image/png without net/http ever seeing it.
		body := append([]byte("\x89PNG\r\n\x1a\n"), []byte(strings.Repeat("x", 2000))...)
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		})
		wrapped := Gzip(handler)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, gzipRequest())

		require.Empty(t, w.Header().Get("Content-Encoding"), "a sniffed image content type must be skipped exactly like an explicit one")
		require.Equal(t, body, w.Body.Bytes())
	})

	t.Run("does not compress a response that already set its own Content-Encoding", func(t *testing.T) {
		body := strings.Repeat("brotli-encoded-elsewhere ", 100)
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "br")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		})
		wrapped := Gzip(handler)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, gzipRequest())

		require.Equal(t, "br", w.Header().Get("Content-Encoding"))
		require.Equal(t, body, w.Body.String())
	})

	t.Run("removes a stale Content-Length when it compresses", func(t *testing.T) {
		body := largeBody()
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		})
		wrapped := Gzip(handler)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, gzipRequest())

		require.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		require.Empty(t, w.Header().Get("Content-Length"), "the uncompressed Content-Length must not survive onto the compressed body")
	})

	t.Run("keeps an accurate Content-Length when it does not compress", func(t *testing.T) {
		body := "short"
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		})
		wrapped := Gzip(handler)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, gzipRequest())

		require.Equal(t, strconv.Itoa(len(body)), w.Header().Get("Content-Length"), "an untouched body's Content-Length must be left alone")
	})

	t.Run("Flush forces the compression decision on a still-small buffered body", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("small"))
			w.(http.Flusher).Flush()
		})
		wrapped := Gzip(handler)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, gzipRequest())

		require.Equal(t, "small", w.Body.String(), "an explicit Flush on a body under MinCompressBytes must still deliver it uncompressed")
	})
}
