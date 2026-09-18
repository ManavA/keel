package perf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryStore(t *testing.T) {
	t.Run("a miss reports not found", func(t *testing.T) {
		s := NewMemoryStore(10)
		_, ok := s.Get("missing")
		require.False(t, ok)
	})

	t.Run("set then get round-trips within ttl", func(t *testing.T) {
		s := NewMemoryStore(10)
		s.Set("k", Entry{Body: []byte("v")}, time.Minute, nil)
		entry, ok := s.Get("k")
		require.True(t, ok)
		require.Equal(t, []byte("v"), entry.Body)
	})

	t.Run("an entry past its ttl is a miss", func(t *testing.T) {
		s := NewMemoryStore(10)
		s.Set("k", Entry{Body: []byte("v")}, -time.Second, nil) // already expired
		_, ok := s.Get("k")
		require.False(t, ok)
	})

	t.Run("invalidating a tag removes every entry tagged with it", func(t *testing.T) {
		s := NewMemoryStore(10)
		s.Set("a", Entry{Body: []byte("a")}, time.Minute, []string{"item:1"})
		s.Set("b", Entry{Body: []byte("b")}, time.Minute, []string{"item:1", "city:example"})
		s.Set("c", Entry{Body: []byte("c")}, time.Minute, []string{"city:example"})

		s.Invalidate("item:1")

		_, ok := s.Get("a")
		require.False(t, ok, "a was tagged item:1")
		_, ok = s.Get("b")
		require.False(t, ok, "b was tagged item:1")
		_, ok = s.Get("c")
		require.True(t, ok, "c was only tagged city:example and must survive")
	})

	t.Run("invalidating an unused tag is a no-op", func(t *testing.T) {
		s := NewMemoryStore(10)
		s.Set("a", Entry{Body: []byte("a")}, time.Minute, []string{"item:1"})
		s.Invalidate("item:does-not-exist")
		_, ok := s.Get("a")
		require.True(t, ok)
	})

	t.Run("exceeding maxEntries evicts the least recently used entry", func(t *testing.T) {
		s := NewMemoryStore(2)
		s.Set("a", Entry{Body: []byte("a")}, time.Minute, nil)
		s.Set("b", Entry{Body: []byte("b")}, time.Minute, nil)
		_, _ = s.Get("a") // touch a, so b becomes the least recently used

		s.Set("c", Entry{Body: []byte("c")}, time.Minute, nil) // pushes the store over 2 entries

		_, ok := s.Get("b")
		require.False(t, ok, "b was least recently used and must have been evicted")
		_, ok = s.Get("a")
		require.True(t, ok, "a was touched more recently and must survive")
		_, ok = s.Get("c")
		require.True(t, ok)
	})
}

func TestResponseCache(t *testing.T) {
	t.Run("a GET response is served from cache on the second call without hitting the handler again", func(t *testing.T) {
		store := NewMemoryStore(10)
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"n":1}`))
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items/1", nil)
		w1 := httptest.NewRecorder()
		wrapped.ServeHTTP(w1, req)
		require.Equal(t, "MISS", w1.Header().Get("X-Cache"))

		w2 := httptest.NewRecorder()
		wrapped.ServeHTTP(w2, req)
		require.Equal(t, "HIT", w2.Header().Get("X-Cache"))
		require.Equal(t, w1.Body.String(), w2.Body.String())
		require.Equal(t, 1, calls, "the handler must not run again on a cache hit")
	})

	t.Run("a non-2xx response is never cached", func(t *testing.T) {
		store := NewMemoryStore(10)
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/broken", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 2, calls, "a failing response must be retried, not cached")
	})

	t.Run("a POST is never cached even to an otherwise cacheable path", func(t *testing.T) {
		store := NewMemoryStore(10)
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/search", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 2, calls)
	})

	t.Run("a handler's own cache tag is honored for invalidation", func(t *testing.T) {
		store := NewMemoryStore(10)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			AddCacheTag(r.Context(), "item:1")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("body"))
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items/1", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)

		store.Invalidate("item:1")

		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		require.Equal(t, "MISS", w.Header().Get("X-Cache"), "invalidation must force the handler to run again")
	})

	t.Run("a custom KeyFunc distinguishes requests DefaultKey would conflate", func(t *testing.T) {
		store := NewMemoryStore(10)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(r.Header.Get("X-Tenant")))
		})
		byTenant := func(r *http.Request) string { return r.Header.Get("X-Tenant") + " " + DefaultKey(r) }
		wrapped := ResponseCache(store, CacheOptions{KeyFunc: byTenant})(handler)

		reqA := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/data", nil)
		reqA.Header.Set("X-Tenant", "a")
		wA := httptest.NewRecorder()
		wrapped.ServeHTTP(wA, reqA)

		reqB := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/data", nil)
		reqB.Header.Set("X-Tenant", "b")
		wB := httptest.NewRecorder()
		wrapped.ServeHTTP(wB, reqB)

		require.Equal(t, "a", wA.Body.String())
		require.Equal(t, "b", wB.Body.String())
	})
}
