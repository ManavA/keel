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

// Gzip compresses a response when the client sends Accept-Encoding: gzip and
// the handler's own Content-Type is not one of skipContentTypePrefixes and it
// has not already set its own Content-Encoding.
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

		gz := &gzipResponse{ResponseWriter: w}
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

// gzipResponse decides, at the first WriteHeader or Write, whether to
// compress — once decided, every subsequent Write goes the same way.
type gzipResponse struct {
	http.ResponseWriter
	gz          *gzip.Writer
	skip        bool
	wroteHeader bool
}

func (g *gzipResponse) WriteHeader(status int) {
	if !g.wroteHeader {
		g.wroteHeader = true
		contentType := g.Header().Get("Content-Type")
		if status < 200 || status >= 300 || skipCompressing(contentType) || g.Header().Get("Content-Encoding") != "" {
			g.skip = true
		} else {
			g.Header().Set("Content-Encoding", "gzip")
			g.Header().Del("Content-Length") // the compressed length is different and not known yet
			g.gz = gzip.NewWriter(g.ResponseWriter)
		}
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponse) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.skip {
		return g.ResponseWriter.Write(p)
	}
	return g.gz.Write(p)
}

// Close flushes and closes the underlying gzip.Writer, if one was opened.
// Callers must defer this — a gzip stream that is never closed omits its
// trailer and produces a body some clients refuse to decode.
func (g *gzipResponse) Close() error {
	if g.gz != nil {
		return g.gz.Close()
	}
	return nil
}

// Unwrap lets other middleware (ETag's wrapper, http.ResponseController)
// reach the original writer.
func (g *gzipResponse) Unwrap() http.ResponseWriter { return g.ResponseWriter }
