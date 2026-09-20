package middleware

import (
	"net"
	"net/http"
	"strings"
)

// Defaults for SecurityHeaders: what each header carries when its option is
// unset.
const (
	DefaultContentTypeOptions = "nosniff"
	DefaultFrameOptions       = "SAMEORIGIN"
	DefaultReferrerPolicy     = "strict-origin-when-cross-origin"
	DefaultHSTS               = "max-age=31536000; includeSubDomains"
)

// SecurityHeadersOptions configures SecurityHeaders. The zero value is the
// safe default.
type SecurityHeadersOptions struct {
	// ContentTypeOptions defaults to "nosniff", so a mislabeled response is
	// never sniffed into something executable.
	ContentTypeOptions string

	// FrameOptions defaults to "SAMEORIGIN": the site's own pages may frame it,
	// another origin may not, which stops clickjacking without breaking
	// same-origin embeds.
	FrameOptions string

	// ReferrerPolicy defaults to "strict-origin-when-cross-origin": same-origin
	// requests carry the full URL, cross-origin ones only the origin, and
	// downgrades to HTTP carry nothing.
	ReferrerPolicy string

	// HSTS defaults to a one-year includeSubDomains policy, and is only sent
	// when the request arrived over TLS to a non-local host. Plain HTTP never
	// carries it, so local development over http://localhost is never pinned
	// to HTTPS by a stray header; likewise a local HTTPS setup never pins
	// localhost. No "preload": that commits every subdomain to HTTPS in the
	// browser vendors' lists, which a library must not opt a service into.
	HSTS string

	// ContentSecurityPolicy is off unless set. No single policy suits every
	// page — a default of "default-src 'self'" would break inline scripts on
	// day one — so a service sets its own once it knows what it serves.
	ContentSecurityPolicy string
}

// SecurityHeaders sets the response headers that stop whole classes of
// browser-side attacks: MIME sniffing, clickjacking, referrer leakage, and
// protocol downgrades.
//
// An option left empty takes its default; "-" omits that header, for the page
// that must be framed cross-origin or carries its own policy elsewhere.
// ContentSecurityPolicy is the exception: empty means off, since there is no
// safe universal default.
//
// The headers go on before the handler runs, so a handler can still override
// one per route with Header().Set. They sit above Recoverer in NewRouter for
// the same reason: the generic 500 it writes carries them too.
func SecurityHeaders(opts SecurityHeadersOptions) func(http.Handler) http.Handler {
	contentType := orDefault(opts.ContentTypeOptions, DefaultContentTypeOptions)
	frame := orDefault(opts.FrameOptions, DefaultFrameOptions)
	referrer := orDefault(opts.ReferrerPolicy, DefaultReferrerPolicy)
	hsts := orDefault(opts.HSTS, DefaultHSTS)
	csp := strings.TrimSpace(opts.ContentSecurityPolicy)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := w.Header()
			setUnlessOmitted(header, "X-Content-Type-Options", contentType)
			setUnlessOmitted(header, "X-Frame-Options", frame)
			setUnlessOmitted(header, "Referrer-Policy", referrer)
			if hsts != "-" && r.TLS != nil && !isLocalHost(r.Host) {
				header.Set("Strict-Transport-Security", hsts)
			}
			if csp != "" {
				header.Set("Content-Security-Policy", csp)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// orDefault resolves an option: blank means the default, "-" stays "-" so the
// caller can tell an omission apart from a value further down.
func orDefault(value, def string) string {
	if v := strings.TrimSpace(value); v != "" {
		return v
	}
	return def
}

func setUnlessOmitted(header http.Header, name, value string) {
	if value == "-" {
		return
	}
	header.Set(name, value)
}

// isLocalHost reports whether the request host is this machine: "localhost",
// a subdomain of it, or a loopback address. HSTS on such a host would pin
// local development to HTTPS.
func isLocalHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	// Bracketed IPv6 without a port ("[::1]") reaches SplitHostPort's error
	// branch, so strip the brackets before parsing.
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
