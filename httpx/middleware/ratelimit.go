package middleware

import (
	"net"
	"net/http"
	"time"

	"github.com/go-chi/httprate"
)

// RateLimitOptions configures RateLimit.
type RateLimitOptions struct {
	// Requests allowed per Window. Both are required; a zero Requests would be
	// a limiter that refuses everything, which is never what anyone meant.
	Requests int
	Window   time.Duration

	// Key chooses the bucket. Nil means per client address.
	//
	// That default is only as good as r.RemoteAddr, so RealIP must run before
	// this middleware. The wrong way round, every request carries the proxy's
	// address and all clients share one bucket.
	Key func(*http.Request) (string, error)

	// Message is the response body, generic by default. Keep it generic: a
	// limiter that names the bucket confirms a guessed identifier.
	Message string
}

// RateLimit rejects requests over the configured rate with 429 and a
// Retry-After header.
func RateLimit(opts RateLimitOptions) func(http.Handler) http.Handler {
	message := opts.Message
	if message == "" {
		message = `{"error":"too many requests"}`
	}
	key := opts.Key
	if key == nil {
		key = KeyByIP
	}

	return httprate.LimitBy(opts.Requests, opts.Window, key,
		httprate.WithResponseHeaders(httprate.ResponseHeaders{
			Limit:      "X-RateLimit-Limit",
			Remaining:  "X-RateLimit-Remaining",
			Reset:      "X-RateLimit-Reset",
			RetryAfter: "Retry-After",
		}),
		httprate.WithLimitHandler(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(message))
		}),
	)
}

// KeyByIP buckets by the client address in r.RemoteAddr, ignoring forwarding
// headers: RealIP has already decided what to believe about those, and
// re-reading the raw header undoes that decision.
//
// IPv6 addresses are reduced to their /64, since a client usually controls a
// whole /64 and could otherwise take a fresh bucket per request.
func KeyByIP(r *http.Request) (string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return httprate.CanonicalizeIP(host), nil
}

// KeyByHeader buckets by the value of a header, such as an API key or tenant
// id. A request without the header falls back to the client address rather than
// sharing one bucket with every other anonymous request.
func KeyByHeader(name string) func(*http.Request) (string, error) {
	return func(r *http.Request) (string, error) {
		if v := r.Header.Get(name); v != "" {
			return name + ":" + v, nil
		}
		return KeyByIP(r)
	}
}
