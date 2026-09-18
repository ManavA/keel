package perf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// BenchmarkResponseCache_Hit measures the cost of a cache hit: no handler
// call, just the Store lookup and copying the buffered body onto the wire.
func BenchmarkResponseCache_Hit(b *testing.B) {
	store := NewMemoryStore(MemoryStoreOptions{})
	body := strings.Repeat("x", 2048)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	wrapped := ResponseCache(store, CacheOptions{})(handler)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items/1", nil)
	wrapped.ServeHTTP(httptest.NewRecorder(), req) // warm the cache

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkResponseCache_Miss measures the cost this middleware adds on top
// of a handler that always misses — the buffering and Store.Set overhead.
func BenchmarkResponseCache_Miss(b *testing.B) {
	store := NewMemoryStore(MemoryStoreOptions{})
	body := strings.Repeat("x", 2048)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	// A KeyFunc unique per call defeats caching entirely, so every call is a
	// miss and exercises the Set path rather than warming into hits.
	i := 0
	wrapped := ResponseCache(store, CacheOptions{
		TTL: time.Minute,
		KeyFunc: func(*http.Request) string {
			i++
			return string(rune(i))
		},
	})(handler)

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items/1", nil))
	}
}
