package perf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryStore(t *testing.T) {
	t.Run("a miss reports not found", func(t *testing.T) {
		s := NewMemoryStore(MemoryStoreOptions{})
		_, ok := s.Get("missing")
		require.False(t, ok)
	})

	t.Run("set then get round-trips within ttl", func(t *testing.T) {
		s := NewMemoryStore(MemoryStoreOptions{})
		s.Set("k", Entry{Body: []byte("v")}, time.Minute, nil)
		entry, ok := s.Get("k")
		require.True(t, ok)
		require.Equal(t, []byte("v"), entry.Body)
	})

	t.Run("an entry past its ttl is a miss", func(t *testing.T) {
		s := NewMemoryStore(MemoryStoreOptions{})
		s.Set("k", Entry{Body: []byte("v")}, -time.Second, nil) // already expired
		_, ok := s.Get("k")
		require.False(t, ok)
	})

	t.Run("invalidating a tag removes every entry tagged with it", func(t *testing.T) {
		s := NewMemoryStore(MemoryStoreOptions{})
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
		s := NewMemoryStore(MemoryStoreOptions{})
		s.Set("a", Entry{Body: []byte("a")}, time.Minute, []string{"item:1"})
		s.Invalidate("item:does-not-exist")
		_, ok := s.Get("a")
		require.True(t, ok)
	})

	t.Run("exceeding MaxBytes evicts the least recently used entry", func(t *testing.T) {
		// Each entry here is exactly 1 byte (a single-byte body, no headers),
		// so MaxBytes: 2 fits exactly two of them.
		s := NewMemoryStore(MemoryStoreOptions{MaxBytes: 2})
		s.Set("a", Entry{Body: []byte("a")}, time.Minute, nil)
		s.Set("b", Entry{Body: []byte("b")}, time.Minute, nil)
		_, _ = s.Get("a") // touch a, so b becomes the least recently used

		s.Set("c", Entry{Body: []byte("c")}, time.Minute, nil) // pushes total size over MaxBytes

		_, ok := s.Get("b")
		require.False(t, ok, "b was least recently used and must have been evicted")
		_, ok = s.Get("a")
		require.True(t, ok, "a was touched more recently and must survive")
		_, ok = s.Get("c")
		require.True(t, ok)
	})

	t.Run("an entry larger than MaxEntryBytes is not stored", func(t *testing.T) {
		s := NewMemoryStore(MemoryStoreOptions{MaxBytes: 1000, MaxEntryBytes: 4})
		s.Set("too-big", Entry{Body: []byte("way too large for the per-entry cap")}, time.Minute, nil)
		_, ok := s.Get("too-big")
		require.False(t, ok, "an entry over MaxEntryBytes must not be cached at all")
	})
}

func TestResponseCache(t *testing.T) {
	t.Run("a GET response is served from cache on the second call without hitting the handler again", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
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
		store := NewMemoryStore(MemoryStoreOptions{})
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
		store := NewMemoryStore(MemoryStoreOptions{})
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
		store := NewMemoryStore(MemoryStoreOptions{})
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
		store := NewMemoryStore(MemoryStoreOptions{})
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

	t.Run("a request carrying Authorization is never served from or written to the cache", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("Bearer " + r.Header.Get("Authorization")))
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)

		alice := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/me", nil)
		alice.Header.Set("Authorization", "Bearer alice-token")
		wAlice := httptest.NewRecorder()
		wrapped.ServeHTTP(wAlice, alice)
		require.Empty(t, wAlice.Header().Get("X-Cache"), "an authenticated request must never touch the cache path")

		bob := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/me", nil)
		bob.Header.Set("Authorization", "Bearer bob-token")
		wBob := httptest.NewRecorder()
		wrapped.ServeHTTP(wBob, bob)

		require.Equal(t, 2, calls, "each authenticated request must reach the handler")
		require.NotEqual(t, wAlice.Body.String(), wBob.Body.String(), "bob must never receive alice's response")
	})

	t.Run("a request carrying Cookie is never served from or written to the cache", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(r.Header.Get("Cookie")))
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)

		alice := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/me", nil)
		alice.Header.Set("Cookie", "session=alice")
		wrapped.ServeHTTP(httptest.NewRecorder(), alice)

		bob := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/me", nil)
		bob.Header.Set("Cookie", "session=bob")
		wBob := httptest.NewRecorder()
		wrapped.ServeHTTP(wBob, bob)

		require.Equal(t, 2, calls, "each cookie-bearing request must reach the handler")
		require.Equal(t, "session=bob", wBob.Body.String(), "bob must get his own response, not alice's cached one")
	})

	t.Run("a response with Set-Cookie is never cached", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Set-Cookie", "session=abc")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/set-cookie", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 2, calls, "a Set-Cookie response must never be replayed to a second caller")
	})

	t.Run("a response with Cache-Control: private is never cached", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Cache-Control", "private, max-age=60")
			w.WriteHeader(http.StatusOK)
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/private", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 2, calls)
	})

	t.Run("a response with Cache-Control: no-store is never cached", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/no-store", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 2, calls)
	})

	t.Run("a response with Vary: * is never cached", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Vary", "*")
			w.WriteHeader(http.StatusOK)
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/vary-star", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 2, calls)
	})

	t.Run("an ordinary Cache-Control value is still cached", func(t *testing.T) {
		// Control for the private/no-store refusal above: a ordinary
		// directive must not be mistaken for one of the refused ones.
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Cache-Control", "public, max-age=60")
			w.WriteHeader(http.StatusOK)
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/public", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 1, calls, "a public, cacheable response must be served from cache on the second call")
	})

	t.Run("a response with Cache-Control: no-cache is never cached", func(t *testing.T) {
		// no-cache means "store it, but revalidate before every use" — this
		// cache has no revalidation path, so it must refuse to store rather
		// than serve a copy it never revalidates.
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/no-cache", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 2, calls)
	})

	t.Run("a response with Cache-Control: max-age=0 is never cached", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Cache-Control", "public, max-age=0")
			w.WriteHeader(http.StatusOK)
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/max-age-zero", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 2, calls)
	})

	t.Run("a response with a positive max-age is still cached", func(t *testing.T) {
		// Control for the max-age=0 refusal above: an ordinary positive
		// max-age must not be mistaken for it.
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Cache-Control", "public, max-age=60")
			w.WriteHeader(http.StatusOK)
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/max-age-positive", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 1, calls)
	})

	t.Run("a response Vary-ing on a header the cache key does not fold in is never cached", func(t *testing.T) {
		// Without this refusal, a response cached under a key that ignores
		// X-Tenant would be replayed across every tenant.
		store := NewMemoryStore(MemoryStoreOptions{})
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Vary", "X-Tenant")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(r.Header.Get("X-Tenant")))
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)

		alice := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/tenant-data", nil)
		alice.Header.Set("X-Tenant", "alice")
		wAlice := httptest.NewRecorder()
		wrapped.ServeHTTP(wAlice, alice)

		bob := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/tenant-data", nil)
		bob.Header.Set("X-Tenant", "bob")
		wBob := httptest.NewRecorder()
		wrapped.ServeHTTP(wBob, bob)

		require.Equal(t, "alice", wAlice.Body.String())
		require.Equal(t, "bob", wBob.Body.String(), "bob must never receive alice's response because Vary named a header the key ignores")
	})

	t.Run("a response Vary-ing only on Accept-Encoding is still cached", func(t *testing.T) {
		// Control for the Vary refusal above: Accept-Encoding is already
		// folded into DefaultKey, so a Vary naming only it must not be
		// mistaken for one naming something the key ignores.
		store := NewMemoryStore(MemoryStoreOptions{})
		calls := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Vary", "Accept-Encoding")
			w.WriteHeader(http.StatusOK)
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/vary-encoding-only", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, 1, calls)
	})

	t.Run("gzip and identity responses to the same URL are cached and replayed separately", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strings.Repeat("hello ", 200)))
		}))
		wrapped := ResponseCache(store, CacheOptions{})(handler)

		gzReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/text", nil)
		gzReq.Header.Set("Accept-Encoding", "gzip")
		gz1 := httptest.NewRecorder()
		wrapped.ServeHTTP(gz1, gzReq)
		require.Equal(t, "gzip", gz1.Header().Get("Content-Encoding"))

		gz2 := httptest.NewRecorder()
		wrapped.ServeHTTP(gz2, gzReq)
		require.Equal(t, "HIT", gz2.Header().Get("X-Cache"))
		require.Equal(t, "gzip", gz2.Header().Get("Content-Encoding"), "a replayed gzip entry must still be labelled gzip")
		require.Equal(t, gz1.Body.Bytes(), gz2.Body.Bytes())

		plainReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/text", nil)
		// No Accept-Encoding: a client that never asked for gzip.
		plain := httptest.NewRecorder()
		wrapped.ServeHTTP(plain, plainReq)
		require.Empty(t, plain.Header().Get("Content-Encoding"), "a client with no Accept-Encoding must never receive raw gzip bytes")
		require.NotEqual(t, gz1.Body.Bytes(), plain.Body.Bytes())
		require.Equal(t, strings.Repeat("hello ", 200), plain.Body.String())
	})

	t.Run("a cache hit replays the original status code, not always 200", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("created"))
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/create", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)

		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		require.Equal(t, "HIT", w.Header().Get("X-Cache"))
		require.Equal(t, http.StatusCreated, w.Code, "a cached 201 must replay as 201, not 200")
	})

	t.Run("a cache hit replays headers beyond Content-Type", func(t *testing.T) {
		store := NewMemoryStore(MemoryStoreOptions{})
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Custom-Header", "custom-value")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("body"))
		})
		wrapped := ResponseCache(store, CacheOptions{})(handler)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/headers", nil)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)

		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		require.Equal(t, "HIT", w.Header().Get("X-Cache"))
		require.Equal(t, "custom-value", w.Header().Get("X-Custom-Header"))
		require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	})
}
