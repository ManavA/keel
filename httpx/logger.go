package httpx

import (
	"context"
	"log/slog"
	"net/http"
)

type loggerContextKey struct{}

// WithLogger returns a context carrying logger. NewRouter puts its own logger
// there, so the response helpers in this package log through the same one the
// service configured rather than through slog.Default.
//
// The logger travels on the context because the helpers take only (w, r): a
// handler calls httpx.NotFound(w, r) without holding a logger, and threading
// one through every signature would put it in the way of every call site to
// serve the two that care.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

// Logger returns the logger carried by ctx, or slog.Default.
func Logger(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if logger, ok := ctx.Value(loggerContextKey{}).(*slog.Logger); ok {
			return logger
		}
	}
	return slog.Default()
}

// withLogger is the middleware NewRouter uses to put its logger on every
// request context.
func withLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(WithLogger(r.Context(), logger)))
		})
	}
}
