package perf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BenchmarkETag_NoMatch measures the full cost of the ETag path when the
// request carries no If-None-Match: buffer the body, hash it, write it out.
func BenchmarkETag_NoMatch(b *testing.B) {
	body := strings.Repeat("x", 4096)
	wrapped := ETag(handlerWithBody(body))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkETag_Match measures the cheaper 304 path: the handler still runs
// and its body is still hashed, but nothing is written to the wire.
func BenchmarkETag_Match(b *testing.B) {
	body := strings.Repeat("x", 4096)
	wrapped := ETag(handlerWithBody(body))

	first := httptest.NewRecorder()
	wrapped.ServeHTTP(first, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	tag := first.Header().Get("ETag")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", tag)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
	}
}
