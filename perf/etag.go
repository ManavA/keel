package perf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// MaxETagBufferBytes bounds how much of a response ETag will buffer before
// giving up and streaming the rest directly, with no ETag header. Without a
// cap, a large streamed response (a file download, an export) would be held
// in memory in full before the first byte reaches the client.
const MaxETagBufferBytes = 8 << 20 // 8 MiB

// ETag buffers a GET/HEAD response, computes a weak ETag from its body
// (SHA-256, truncated to 16 hex characters — collisions are irrelevant at
// cache-validation scale, and a short header is cheap to send on every
// response), and answers 304 Not Modified when the request's If-None-Match
// already names it.
//
// The tag is weak (W/"...") rather than strong because ETag has no way to
// know, by itself, whether something else in the handler chain will alter
// the bytes after it computes the hash — Gzip is the case that matters:
// composed as ETag(Gzip(h)), ETag hashes the already-compressed bytes and a
// gzip and a non-gzip request correctly get different tags; composed the
// other way, Gzip(ETag(h)), ETag hashes the handler's uncompressed output
// before Gzip has run, so a gzip and a non-gzip request get the identical
// tag despite different bytes on the wire. RFC 9110 requires a strong
// validator to differ whenever the representation (including its
// content-coding) differs; a weak validator is defined to be compared for
// semantic equivalence instead, so it stays correct under either
// composition. ETag(Gzip(h)) is still the recommended order — it is the one
// where the tag also happens to differ per encoding.
//
// It fully buffers the body before deciding, because answering 304
// correctly means not sending the body at all — there is no way to compute
// a hash of bytes not yet produced and still have the option of
// withholding them. Buffering stops at MaxETagBufferBytes: a response
// larger than that is streamed straight through, uncompressed by an ETag,
// rather than held in memory in full.
func ETag(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}

		buf := &bufferedResponse{real: w, header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(buf, r)

		if buf.tooLarge {
			// Already streamed directly to w by bufferedResponse.Write once
			// it exceeded MaxETagBufferBytes; nothing left to do here.
			return
		}

		for k, v := range buf.header {
			w.Header()[k] = v
		}

		if buf.status < 200 || buf.status >= 300 || buf.body.Len() == 0 {
			w.WriteHeader(buf.status)
			_, _ = w.Write(buf.body.Bytes())
			return
		}

		sum := sha256.Sum256(buf.body.Bytes())
		tag := `W/"` + hex.EncodeToString(sum[:])[:16] + `"`
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
// list of ETags. Since ETag always issues the same weak tag for the same
// content, a client echoing back exactly what it was given compares equal
// under plain string equality — a full weak-comparison algorithm (ignoring
// the W/ prefix, comparing multiple representations as equivalent) is not
// needed for round-tripping this middleware's own tags.
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
// forwarding any of it to real, up to MaxETagBufferBytes. Past that point it
// commits whatever was buffered to real and forwards every subsequent Write
// directly, uncompressed by an ETag.
type bufferedResponse struct {
	real        http.ResponseWriter
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
	tooLarge    bool
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
	if b.tooLarge {
		return b.real.Write(p)
	}
	if b.body.Len()+len(p) <= MaxETagBufferBytes {
		return b.body.Write(p)
	}

	b.tooLarge = true
	for k, v := range b.header {
		b.real.Header()[k] = v
	}
	b.real.WriteHeader(b.status)
	// The bytes are the handler's own response, replayed after buffering; this
	// writer neither builds nor escapes them.
	if _, err := b.real.Write(b.body.Bytes()); err != nil { //nolint:gosec // G705: pass-through of the handler's bytes
		return 0, err
	}
	b.body.Reset()
	return b.real.Write(p)
}

// Flush is a no-op while still buffering — nothing has been sent to real
// yet, so there is nothing to flush — and forwards to real's Flusher once
// tooLarge has switched this response to streaming directly.
func (b *bufferedResponse) Flush() {
	if b.tooLarge {
		if f, ok := b.real.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// Unwrap lets http.ResponseController and other middleware reach the
// original writer.
func (b *bufferedResponse) Unwrap() http.ResponseWriter { return b.real }
