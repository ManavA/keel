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
	// limit that should only cover some routes belongs on a sub-router instead:
	// the interesting limits — login, signup, password reset — are much tighter
	// than anything you would put on a whole API.
	RateLimit *middleware.RateLimitOptions

	// Timeout bounds a request. Default 30 seconds; a negative value disables
	// it. Disable it for a router serving long-lived responses — streaming,
	// server-sent events, an upload — since the timeout cancels those too.
	Timeout time.Duration

	// CompressLevel enables gzip at that level, 1 to 9. Zero leaves compression
	// off, which is right when a proxy in front already does it.
	CompressLevel int
}

// DefaultTimeout bounds a request unless RouterOptions says otherwise.
const DefaultTimeout = 30 * time.Second

// NewRouter returns a chi router with the middleware stack assembled in the
// order that makes each piece work.
//
// The order is the value here. Read it downwards:
//
//  1. RequestID, so everything after it — including the panic recovery — can
//     name the request it is talking about.
//  2. GetHead, because chi answers HEAD with 405 on a route registered for GET,
//     and RFC 9110 says HEAD is available wherever GET is. Uptime checks, CDNs
//     and link checkers all reach for it; `curl -sI` against an endpoint
//     returning 405 reads as "this endpoint is broken".
//  3. RealIP, before anything that cares who the client is. After the rate
//     limiter it would be useless, because the limiter would already have
//     bucketed every client behind the proxy together.
//  4. RequestLog, above Recoverer so that a panic is still logged as a request
//     with a status, rather than vanishing from the access log entirely.
//  5. Recoverer, above the timeout and the handler, so it catches panics from
//     both.
//  6. Timeout, then compression, then CORS, then the rate limit.
//
// Nothing stops you building your own chain out of httpx/middleware. This is
// the order to copy when you do.
func NewRouter(opts RouterOptions) *chi.Mux {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	r := chi.NewRouter()

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

	// chi's defaults write plain text and no request id. These match every
	// other error the service produces.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		Error(w, req, http.StatusNotFound, nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		Error(w, req, http.StatusMethodNotAllowed, nil)
	})

	return r
}
