package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// RecovererOptions configures Recoverer.
type RecovererOptions struct {
	// Logger defaults to slog.Default.
	Logger *slog.Logger

	// OnPanic, if set, is called with the recovered value and the stack after
	// the panic is logged, for reporting to an error tracker. It runs inside
	// the recovery, so a panic in it takes the process down.
	OnPanic func(r *http.Request, recovered any, stack []byte)
}

// Recoverer turns a panic in a handler into a 500, logs it with its stack, and
// lets the process carry on serving.
//
// The body is the same generic 500 as everywhere else. A panic value names
// types, fields and sometimes the data that broke, none of which belongs in a
// response.
//
// http.ErrAbortHandler is re-panicked rather than caught: net/http uses it to
// mean the response is being abandoned deliberately.
func Recoverer(opts RecovererOptions) func(http.Handler) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Wrapped so the recovery below can tell whether the handler had
			// already started writing, and not call WriteHeader twice.
			rw := &recorder{ResponseWriter: w}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}

				stack := debug.Stack()
				logger.ErrorContext(r.Context(), "panic in handler",
					"panic", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(stack),
				)
				if opts.OnPanic != nil {
					opts.OnPanic(r, rec, stack)
				}

				// If the handler already wrote a status, the client has a
				// partial response and there is nothing left to say.
				if rw.wroteHeader {
					return
				}
				rw.Header().Set("Content-Type", "application/json; charset=utf-8")
				rw.WriteHeader(http.StatusInternalServerError)
				_, _ = rw.Write([]byte(`{"error":"internal server error"}`))
			}()

			next.ServeHTTP(rw, r)
		})
	}
}
