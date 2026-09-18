package httpx

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/ManavA/keel/httpx/middleware"
)

// RouterOptions configures NewRouter. The zero value gives you request ids,
// request logging, panic recovery, a 30 second timeout and HEAD routed to GET —
// no CORS, no rate limit, and forwarding headers ignored.
type RouterOptions struct {
	// Logger defaults to slog.Default, and is passed to the middleware that
	// logs.
	Logger *slog.Logger

	// RequestID configures the request-id middleware.
	RequestID middleware.RequestIDOptions

	// RealIP configures client-address recovery. Its zero value ignores
	// forwarding headers, so a service reachable directly cannot be told what
	// its clients' addresses are.
	RealIP middleware.RealIPOptions

	// RequestLog configures the request log. Set SkipRequestLog to leave it out
	// entirely — for a service that logs somewhere else.
	RequestLog     middleware.RequestLogOptions
	SkipRequestLog bool

	// Recoverer configures panic recovery.
	Recoverer middleware.RecovererOptions

	// CORS, when set, answers browser preflights.
	CORS *middleware.CORSOptions

	// RateLimit, when set, applies a limit to every route on this router. A
	// limit for only some routes belongs on a sub-router; the tight limits
	// (login, signup, password reset) are not ones you would apply API-wide.
	RateLimit *middleware.RateLimitOptions

	// Timeout bounds a request. Default 30 seconds; a negative value disables
	// it. Disable it on a router serving streaming responses, server-sent
	// events or uploads, which the timeout would cancel.
	Timeout time.Duration

	// CompressLevel enables gzip at that level, 1 to 9. Zero leaves compression
	// off, which is right when a proxy in front already does it.
	CompressLevel int
}

// DefaultTimeout bounds a request unless RouterOptions says otherwise.
const DefaultTimeout = 30 * time.Second

// NewRouter returns a chi router with the middleware stack in the order each
// piece needs.
//
//  1. The router's logger onto the request context, then RequestID, so
//     everything after them — including panic recovery and the response
//     helpers — can log through the right logger and name the request.
//  2. GetHead. chi answers HEAD with 405 on a route registered for GET, while
//     RFC 9110 requires HEAD wherever GET is served; uptime checks and CDNs use
//     it.
//  3. RealIP, before anything that cares who the client is. After the rate
//     limiter it would have no effect, the limiter having already bucketed
//     every client behind the proxy together.
//  4. RequestLog, above Recoverer, so a panic is still logged as a request with
//     a status.
//  5. Recoverer, above the timeout and the handler, so it catches panics from
//     both.
//  6. Timeout, then compression, then CORS, then the rate limit.
//
// Build your own chain from httpx/middleware if this does not suit; this is the
// order to copy.
func NewRouter(opts RouterOptions) *chi.Mux {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	r := chi.NewRouter()

	// First, so that everything below — including the response helpers reached
	// from a handler — logs through this router's logger.
	r.Use(withLogger(logger))
	r.Use(middleware.RequestID(opts.RequestID))
	r.Use(chimw.GetHead)
	r.Use(middleware.RealIP(opts.RealIP))

	if !opts.SkipRequestLog {
		logOpts := opts.RequestLog
		if logOpts.Logger == nil {
			logOpts.Logger = logger
		}
		r.Use(middleware.RequestLog(logOpts))
	}

	recOpts := opts.Recoverer
	if recOpts.Logger == nil {
		recOpts.Logger = logger
	}
	r.Use(middleware.Recoverer(recOpts))

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout > 0 {
		r.Use(chimw.Timeout(timeout))
	}

	if opts.CompressLevel > 0 {
		r.Use(chimw.Compress(opts.CompressLevel))
	}
	if opts.CORS != nil {
		r.Use(middleware.CORS(*opts.CORS))
	}
	if opts.RateLimit != nil {
		r.Use(middleware.RateLimit(*opts.RateLimit))
	}

	// chi's defaults write plain text with no request id.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		Error(w, req, http.StatusNotFound, nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		Error(w, req, http.StatusMethodNotAllowed, nil)
	})

	return r
}
