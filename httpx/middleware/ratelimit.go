package middleware

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/httprate"
)

// RateLimitOptions configures RateLimit.
type RateLimitOptions struct {
	// Requests allowed per Window. Both are required and RateLimit panics
	// without them: a zero Window makes httprate admit everything, so a
	// forgotten field on a login route is a limiter that does nothing and never
	// says so, and a zero Requests refuses everything.
	Requests int
	Window   time.Duration

	// Key chooses the bucket. Nil means per client address.
	//
	// That default is only as good as r.RemoteAddr, so RealIP must run before
	// this middleware. The wrong way round, every request carries the proxy's
	// address and all clients share one bucket.
	//
	// The same sharing happens with the order right but RealIP unconfigured:
	// behind Cloud Run or any proxy, with no TrustedProxies or TrustAnyPeer,
	// every request still carries the proxy's address, so a few active users
	// spend the whole budget and everyone else gets 429s. Those 429s read as
	// abuse rather than misconfiguration, which is why this failure mode is
	// worth stating twice. RateLimitSharedBucketWarning names it at startup.
	Key func(*http.Request) (string, error)

	// Message is the response body, generic by default. Keep it generic: a
	// limiter that names the bucket confirms a guessed identifier.
	Message string
}

// RateLimit rejects requests over the configured rate with 429 and a
// Retry-After header.
//
// It panics when Requests or Window is missing. Returning a limiter that admits
// everything would be indistinguishable from a working one until somebody
// counted the requests that got through.
func RateLimit(opts RateLimitOptions) func(http.Handler) http.Handler {
	if opts.Requests <= 0 || opts.Window <= 0 {
		panic(fmt.Sprintf(
			"middleware.RateLimit: both Requests and Window are required, got Requests=%d Window=%v",
			opts.Requests, opts.Window))
	}

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

// RateLimitSharedBucketWarning names the misconfiguration in which a rate
// limit buckets every client behind a proxy together: a limit is enabled with
// the default per-address key while realIP trusts no forwarding header, so
// behind any proxy each request still carries the proxy's address. A few
// active users then spend the whole budget and everyone else gets 429s that
// read as abuse.
//
// It returns "" when there is nothing to warn about: no limit, a custom Key
// that does not bucket by the client address, or a RealIP that trusts a
// header. Call it at startup — httpx.NewRouter already does — or in a test
// over the service's own options.
func RateLimitSharedBucketWarning(realIP RealIPOptions, rateLimit *RateLimitOptions) string {
	if rateLimit == nil || rateLimit.Key != nil || realIP.TrustsHeaders() {
		return ""
	}
	return "middleware.RateLimit: the default key buckets by client address but " +
		"RealIP trusts no forwarding header, so behind a proxy every client " +
		"shares one bucket; set TrustedProxies (or TrustAnyPeer where only the " +
		"platform can reach the process), or key the limit off something else"
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
//
// Only use it for a header something upstream has already authenticated. A
// caller free to invent the value is free to invent a fresh bucket per request,
// which is no limit at all. Behind an authenticating gateway, or keyed off a
// verified session, it is a per-tenant limit; in front of one it is decoration.
func KeyByHeader(name string) func(*http.Request) (string, error) {
	return func(r *http.Request) (string, error) {
		if v := r.Header.Get(name); v != "" {
			return name + ":" + v, nil
		}
		return KeyByIP(r)
	}
}
