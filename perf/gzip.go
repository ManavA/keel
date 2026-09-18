package perf

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// skipContentTypePrefixes are response types Gzip leaves alone: already-
// compressed media that would not shrink further, and would sometimes grow,
// for the cost of a second compression pass.
var skipContentTypePrefixes = []string{"image/", "video/", "audio/"}

// MinCompressBytes is the smallest body Gzip will compress. Below this, the
// gzip header and trailer overhead (about 23 bytes) can make the compressed
// output larger than the original; compressing a handful of bytes is also
// pure CPU cost with no benefit to a client on any reasonable connection.
const MinCompressBytes = 1024

// Gzip compresses a response when the client sends Accept-Encoding: gzip,
// the response is at least MinCompressBytes, its content type is not one of
// skipContentTypePrefixes, and it has not already set its own
// Content-Encoding.
//
// The compression decision depends on the response's actual content type
// and total size, neither of which is known when the handler calls
// WriteHeader: a handler that never sets Content-Type relies on net/http's
// own sniffing of the first bytes written, and the total size is unknown
// until the handler stops writing or writes enough to cross
// MinCompressBytes. Gzip buffers up to MinCompressBytes bytes to make this
// decision from the response's real content, not a header that may not be
// set yet, then streams everything after that point through gzip.Writer
// without further buffering.
//
// It always adds Vary: Accept-Encoding, even to an uncompressed response —
// any cache in front of this handler needs that header on every response
// this middleware could have compressed differently, not just the ones it
// did, or it may serve a gzipped response to a client that never asked for
// one.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")

		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}

		gz := &gzipResponse{ResponseWriter: w, status: http.StatusOK}
		defer func() { _ = gz.Close() }()
		next.ServeHTTP(gz, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(enc) == "gzip" {
			return true
		}
	}
	return false
}

func skipCompressing(contentType string) bool {
	for _, prefix := range skipContentTypePrefixes {
		if strings.HasPrefix(contentType, prefix) {
			return true
		}
	}
	return false
}

// gzipResponse buffers up to MinCompressBytes to decide whether to compress,
// then streams the rest through gzip.Writer (or straight through, if not
// compressing) with no further buffering.
type gzipResponse struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	buf         []byte
	decided     bool
	skip        bool
	gz          *gzip.Writer
}

func (g *gzipResponse) WriteHeader(status int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.status = status
	// The header is not sent to the client yet: Content-Encoding and
	// Content-Length depend on the compression decision, which is not final
	// until decide runs.
}

func (g *gzipResponse) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.decided {
		return g.writeDecided(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) >= MinCompressBytes {
		g.decide()
		if err := g.flushBuffered(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// decide chooses whether to compress, based on the real content type (the
// handler's own Content-Type header, or a sniff of the buffered bytes if it
// never set one) and how much of the body has been seen so far. It always
// sends the response header exactly once, whichever way the decision goes.
func (g *gzipResponse) decide() {
	g.decided = true

	contentType := g.Header().Get("Content-Type")
	if contentType == "" {
		n := len(g.buf)
		if n > 512 {
			n = 512
		}
		contentType = http.DetectContentType(g.buf[:n])
	}

	switch {
	case g.status < 200 || g.status >= 300,
		len(g.buf) < MinCompressBytes,
		skipCompressing(contentType),
		g.Header().Get("Content-Encoding") != "":
		g.skip = true
	default:
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Del("Content-Length") // the compressed length differs and is not known yet
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}

	g.ResponseWriter.WriteHeader(g.status)
}

func (g *gzipResponse) flushBuffered() error {
	buf := g.buf
	g.buf = nil
	_, err := g.writeDecided(buf)
	return err
}

func (g *gzipResponse) writeDecided(p []byte) (int, error) {
	if g.skip {
		// Pass-through of the handler's own bytes; this writer neither builds
		// nor escapes them.
		return g.ResponseWriter.Write(p) //nolint:gosec // G705: pass-through of the handler's bytes
	}
	return g.gz.Write(p)
}

// Close flushes and closes the underlying gzip.Writer, if one was opened,
// and finalizes the compression decision for a response smaller than
// MinCompressBytes that never triggered decide from Write. Callers must
// defer this — a gzip stream that is never closed omits its trailer and
// produces a body some clients refuse to decode.
func (g *gzipResponse) Close() error {
	if !g.wroteHeader {
		return nil
	}
	if !g.decided {
		g.decide()
		if err := g.flushBuffered(); err != nil {
			return err
		}
	}
	if g.gz != nil {
		return g.gz.Close()
	}
	return nil
}

// Flush forces the compression decision now, on whatever has been buffered
// so far, rather than waiting for MinCompressBytes or Close — a streaming
// handler that calls Flush explicitly is asking for bytes on the wire now,
// and holding them back to see if more arrive would defeat the point of
// calling it.
func (g *gzipResponse) Flush() {
	if !g.decided {
		g.decide()
		_ = g.flushBuffered()
	}
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets other middleware (ETag's wrapper, http.ResponseController)
// reach the original writer.
func (g *gzipResponse) Unwrap() http.ResponseWriter { return g.ResponseWriter }
