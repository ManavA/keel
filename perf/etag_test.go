package perf

import (
	"context"
	"net/http"
	"net/http/httptest"
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
}
