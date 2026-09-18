package perf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func handlerWithBody(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func TestETag(t *testing.T) {
	t.Run("a plain GET receives an ETag header", func(t *testing.T) {
		wrapped := ETag(handlerWithBody("hello"))
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.NotEmpty(t, w.Header().Get("ETag"))
		require.Equal(t, "hello", w.Body.String())
	})

	t.Run("a matching If-None-Match gets 304 with no body", func(t *testing.T) {
		wrapped := ETag(handlerWithBody("hello"))

		first := httptest.NewRecorder()
		wrapped.ServeHTTP(first, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		tag := first.Header().Get("ETag")
		require.NotEmpty(t, tag)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.Header.Set("If-None-Match", tag)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotModified, w.Code)
		require.Empty(t, w.Body.String())
	})

	t.Run("a stale If-None-Match gets the full body again", func(t *testing.T) {
		wrapped := ETag(handlerWithBody("hello"))
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.Header.Set("If-None-Match", `"0000000000000000"`)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "hello", w.Body.String())
	})

	t.Run("If-None-Match: * always matches", func(t *testing.T) {
		wrapped := ETag(handlerWithBody("hello"))
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.Header.Set("If-None-Match", "*")
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotModified, w.Code)
	})

	t.Run("a non-2xx response is passed through with no ETag", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
		})
		wrapped := ETag(handler)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

		require.Equal(t, http.StatusNotFound, w.Code)
		require.Empty(t, w.Header().Get("ETag"))
		require.Equal(t, "not found", w.Body.String())
	})

	t.Run("a POST is passed through untouched", func(t *testing.T) {
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("posted"))
		})
		wrapped := ETag(handler)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil))

		require.Equal(t, 1, calls)
		require.Empty(t, w.Header().Get("ETag"))
	})

	t.Run("different bodies produce different ETags", func(t *testing.T) {
		w1 := httptest.NewRecorder()
		ETag(handlerWithBody("a")).ServeHTTP(w1, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

		w2 := httptest.NewRecorder()
		ETag(handlerWithBody("b")).ServeHTTP(w2, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

		require.NotEqual(t, w1.Header().Get("ETag"), w2.Header().Get("ETag"))
	})

	t.Run("the tag is weak", func(t *testing.T) {
		w := httptest.NewRecorder()
		ETag(handlerWithBody("hello")).ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		require.True(t, strings.HasPrefix(w.Header().Get("ETag"), `W/"`))
	})

	t.Run("a handler's own headers survive on a 200, not just Content-Type", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Custom-Header", "custom-value")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("hello"))
		})
		w := httptest.NewRecorder()
		ETag(handler).ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		require.Equal(t, "custom-value", w.Header().Get("X-Custom-Header"))
	})

	t.Run("a handler's own headers survive on a non-2xx response too", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Custom-Header", "custom-value")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
		})
		w := httptest.NewRecorder()
		ETag(handler).ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		require.Equal(t, "custom-value", w.Header().Get("X-Custom-Header"))
	})

	t.Run("composed as ETag(Gzip(h)), a gzip and a non-gzip request get different tags", func(t *testing.T) {
		body := strings.Repeat("x", 2000) // over Gzip's MinCompressBytes
		handler := ETag(Gzip(handlerWithBody(body)))

		plain := httptest.NewRecorder()
		handler.ServeHTTP(plain, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

		gz := httptest.NewRecorder()
		gzReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		gzReq.Header.Set("Accept-Encoding", "gzip")
		handler.ServeHTTP(gz, gzReq)

		require.NotEmpty(t, plain.Header().Get("ETag"))
		require.NotEmpty(t, gz.Header().Get("ETag"))
		require.NotEqual(t, plain.Header().Get("ETag"), gz.Header().Get("ETag"),
			"identity and gzip representations of the same content must not share a validator")
	})

	t.Run("a response over MaxETagBufferBytes streams directly with no ETag", func(t *testing.T) {
		body := strings.Repeat("x", MaxETagBufferBytes+1)
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Custom-Header", "custom-value")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		})
		w := httptest.NewRecorder()
		ETag(handler).ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

		require.Empty(t, w.Header().Get("ETag"), "a response over the buffer cap must not be held in memory to compute a tag")
		require.Equal(t, "custom-value", w.Header().Get("X-Custom-Header"), "headers set before the cap was hit must still reach the client")
		require.Equal(t, body, w.Body.String(), "the full body must still arrive even without an ETag")
	})

	t.Run("Flush after the buffer cap forwards to the real ResponseWriter", func(t *testing.T) {
		body := strings.Repeat("x", MaxETagBufferBytes+1)
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			w.(http.Flusher).Flush() // must not panic: bufferedResponse must implement Flusher
		})
		w := httptest.NewRecorder()
		require.NotPanics(t, func() {
			ETag(handler).ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		})
	})
}
