package middleware

import (
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/cors"
)

// CORSOptions configures CORS. The zero value allows nothing.
type CORSOptions struct {
	// AllowedOrigins are exact origins, "https://app.example.com". A single "*"
	// allows any origin.
	//
	// An empty list allows nothing. go-chi/cors reads an empty list as ["*"],
	// so CORS does not pass one through: an unset CORS_ORIGINS variable loaded
	// into a []string would otherwise turn a deployment's policy from one
	// origin into every origin, with no signal.
	AllowedOrigins []string

	// AllowedMethods defaults to the safe set plus the four that change things.
	AllowedMethods []string

	// AllowedHeaders defaults to Accept, Authorization, Content-Type and the
	// request id header.
	AllowedHeaders []string

	// ExposedHeaders are the response headers JavaScript may read. Anything not
	// listed is invisible to the browser whatever the response contains, so a
	// client reading a pagination total from a header sees nothing.
	ExposedHeaders []string

	// AllowCredentials lets the browser send cookies and Authorization. It
	// cannot be combined with a "*" origin; CORS panics on that pairing rather
	// than letting it deploy.
	AllowCredentials bool

	// MaxAge is how long a browser may cache the preflight response.
	MaxAge time.Duration
}

// CORS answers cross-origin preflights and adds the response headers a browser
// needs to hand a response to JavaScript.
//
// With no allowed origins it adds no CORS headers at all. The browser then
// refuses the response, which is what an API not called from a browser wants
// and what a misconfigured one should get.
//
// It panics when AllowCredentials is combined with a "*" origin. Browsers
// reject that pairing, so the alternative is a configuration that deploys,
// works in curl, and fails in every browser.
func CORS(opts CORSOptions) func(http.Handler) http.Handler {
	if opts.AllowCredentials && slices.Contains(opts.AllowedOrigins, "*") {
		panic(`middleware.CORS: AllowCredentials cannot be combined with the "*" origin; ` +
			`browsers reject it. List the origins that may send credentials.`)
	}
	if len(opts.AllowedOrigins) == 0 {
		return denyCORS
	}

	methods := opts.AllowedMethods
	if len(methods) == 0 {
		methods = []string{
			http.MethodGet, http.MethodHead, http.MethodPost,
			http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
		}
	}
	headers := opts.AllowedHeaders
	if len(headers) == 0 {
		headers = []string{"Accept", "Authorization", "Content-Type", RequestIDHeader}
	}
	exposed := opts.ExposedHeaders
	if len(exposed) == 0 {
		exposed = []string{RequestIDHeader}
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}

	return cors.Handler(cors.Options{
		AllowedOrigins:   opts.AllowedOrigins,
		AllowedMethods:   methods,
		AllowedHeaders:   headers,
		ExposedHeaders:   exposed,
		AllowCredentials: opts.AllowCredentials,
		MaxAge:           int(maxAge.Seconds()),
	})
}

// denyCORS passes the request through untouched. With no
// Access-Control-Allow-Origin header the browser will not hand the response to
// the page.
func denyCORS(next http.Handler) http.Handler { return next }
