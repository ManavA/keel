package httpx

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/metrics"
)

// RouterOptions configures NewRouter. The zero value gives you request ids,
// security headers, request logging, panic recovery, a 30 second timeout and
// HEAD routed to GET — no CORS, no rate limit, and forwarding headers ignored.
type RouterOptions struct {
	// Logger defaults to slog.Default, and is passed to the middleware that
	// logs.
	Logger *slog.Logger

	RequestID middleware.RequestIDOptions

	// RealIP configures client-address recovery. Its zero value ignores
	// forwarding headers, so a service reachable directly cannot be told what
	// its clients' addresses are.
	RealIP middleware.RealIPOptions

	// SecurityHeaders, when set, overrides the default security headers. Nil
	// applies the safe defaults; set SkipSecurityHeaders to leave them out
	// entirely, for a service whose proxy in front already sets them.
	SecurityHeaders     *middleware.SecurityHeadersOptions
	SkipSecurityHeaders bool

	// RequestLog configures the request log. Set SkipRequestLog to leave it out
	// entirely — for a service that logs somewhere else.
	RequestLog     middleware.RequestLogOptions
	SkipRequestLog bool

	// Metrics receives one count and one duration per request, labeled
	// with the route pattern and status. Nil records nothing.
	Metrics *metrics.Metrics

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

// NewRouter returns a chi router with the middleware stack assembled in the
// order below. Build your own chain from httpx/middleware if it does not suit;
// this is the order to copy, and each step is placed where it is for a reason:
//
//	logger, RequestID   everything after them, including panic recovery and the
//	                    response helpers, can name the request
//	GetHead             chi answers HEAD with 405 on a GET route; RFC 9110
//	                    requires HEAD wherever GET is served
//	RealIP              after the rate limiter it would have no effect, the
//	                    limiter having already bucketed every client behind the
//	                    proxy together
//	SecurityHeaders     above Recoverer, so the 500 it writes carries the same
//	                    headers as every other response
//	RequestLog          above Recoverer, so a panic is still logged as a request
//	Observe             with the request log when Metrics is set, above
//	                    Recoverer, so a recovered panic is recorded as the
//	                    500 the client receives
//	Recoverer           above the timeout and the handler, to catch both
//	Timeout, compression, CORS, rate limit
//
// One chi behaviour to know about: on a router with no routes registered at
// all, ServeHTTP goes straight to the NotFound handler without running the
// middleware chain, so that 404 carries no request id and logs through
// slog.Default. Register one route and both 404 and 405 behave normally.
func NewRouter(opts RouterOptions) *chi.Mux {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	r := chi.NewRouter()

	// A default-key limit with RealIP unconfigured shares one bucket across
	// every client behind a proxy, and the 429s that follow read as abuse.
	// Name it here, where the stack is assembled, rather than at the first
	// 429.
	if warning := middleware.RateLimitSharedBucketWarning(opts.RealIP, opts.RateLimit); warning != "" {
		logger.Warn(warning)
	}

	// First, so that everything below — including the response helpers reached
	// from a handler — logs through this router's logger.
	r.Use(withLogger(logger))
	r.Use(middleware.RequestID(opts.RequestID))
	r.Use(chimw.GetHead)
	r.Use(middleware.RealIP(opts.RealIP))

	if !opts.SkipSecurityHeaders {
		secOpts := middleware.SecurityHeadersOptions{}
		if opts.SecurityHeaders != nil {
			secOpts = *opts.SecurityHeaders
		}
		r.Use(middleware.SecurityHeaders(secOpts))
	}

	if !opts.SkipRequestLog {
		logOpts := opts.RequestLog
		if logOpts.Logger == nil {
			logOpts.Logger = logger
		}
		r.Use(middleware.RequestLog(logOpts))
	}
	if opts.Metrics != nil {
		r.Use(middleware.Observe(opts.Metrics))
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
