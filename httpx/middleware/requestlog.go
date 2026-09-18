package middleware

import (
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/ManavA/keel/log"
)

// RequestLogOptions configures RequestLog.
type RequestLogOptions struct {
	// Logger defaults to slog.Default.
	Logger *slog.Logger

	// Skip returns true for requests that should not be logged. A health check
	// polled every few seconds from several regions otherwise buries
	// everything else.
	Skip func(*http.Request) bool

	// SlowRequest, if set, logs a request taking longer than this at warn level.
	// Nothing else about the line changes.
	SlowRequest time.Duration
}

// RequestLog logs one line per request, after the response.
//
// The query string has credential-shaped parameters removed. Password reset
// links, email verification links and signed download URLs all carry a
// single-use credential in the query string.
func RequestLog(opts RequestLogOptions) func(http.Handler) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opts.Skip != nil && opts.Skip(r) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			elapsed := time.Since(start)

			status := rec.statusOrOK()
			level := slog.LevelInfo
			switch {
			case status >= 500:
				level = slog.LevelError
			case opts.SlowRequest > 0 && elapsed >= opts.SlowRequest:
				level = slog.LevelWarn
			}

			logger.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("query", RedactQuery(r.URL.Query())),
				slog.Int("status", status),
				slog.Int64("bytes", rec.bytes),
				slog.Duration("duration", elapsed),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// RedactQuery renders query parameters with credential-shaped values replaced.
// Exported for handlers that log a URL of their own.
func RedactQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	safe := make(url.Values, len(values))
	for name, vs := range values {
		if log.IsSecretKey(name) {
			safe[name] = []string{log.Placeholder}
			continue
		}
		safe[name] = vs
	}
	return safe.Encode()
}
