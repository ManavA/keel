package perf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// ETag buffers a GET/HEAD response, computes a strong ETag from its body
// (SHA-256, truncated to 16 hex characters — collisions are irrelevant at
// cache-validation scale, and a short header is cheap to send on every
// response), and answers 304 Not Modified when the request's If-None-Match
// already names it.
//
// It fully buffers the body before deciding, because answering 304 correctly
// means not sending the body at all — there is no way to compute a hash of
// bytes not yet produced and still have the option of withholding them. That
// makes ETag proportional to response size in memory, which is fine for API
// JSON responses; put it in front of a large file stream and it will hold
// the whole thing before the client sees a byte.
func ETag(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}

		buf := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(buf, r)

		for k, v := range buf.header {
			w.Header()[k] = v
		}

		if buf.status < 200 || buf.status >= 300 || buf.body.Len() == 0 {
			w.WriteHeader(buf.status)
			_, _ = w.Write(buf.body.Bytes())
			return
		}

		sum := sha256.Sum256(buf.body.Bytes())
		tag := `"` + hex.EncodeToString(sum[:])[:16] + `"`
		w.Header().Set("ETag", tag)

		if ifNoneMatchHits(r.Header.Get("If-None-Match"), tag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.WriteHeader(buf.status)
		_, _ = w.Write(buf.body.Bytes())
	})
}

// ifNoneMatchHits implements the subset of If-None-Match comparison this
// middleware needs: "*", or an exact match against one of a comma-separated
// list of ETags. It deliberately does not implement weak comparison (W/"...")
// — this package only ever issues strong tags, so a weak one in the request
// can never legitimately match here.
func ifNoneMatchHits(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}

// bufferedResponse captures a handler's headers, status and body without
// forwarding any of it, so a wrapper can inspect the whole response before
// deciding what — if anything — to send downstream.
type bufferedResponse struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if !b.wroteHeader {
		b.wroteHeader = true
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	return b.body.Write(p)
}
