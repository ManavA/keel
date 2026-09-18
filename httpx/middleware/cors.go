package middleware

import (
	"net/http"
	"time"

	"github.com/go-chi/cors"
)

// CORSOptions configures CORS. The zero value allows nothing, which is the
// correct default for an API that is not called from a browser at all.
type CORSOptions struct {
	// AllowedOrigins are exact origins, "https://app.example.com". A single "*"
	// allows any origin and cannot be combined with AllowCredentials; browsers
	// reject that pairing, so an API with both works in curl and fails in the
	// browser.
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

	// AllowCredentials lets the browser send cookies and Authorization.
	AllowCredentials bool

	// MaxAge is how long a browser may cache the preflight response.
	MaxAge time.Duration
}

// CORS answers cross-origin preflights and adds the response headers a browser
// needs to hand a response to JavaScript.
func CORS(opts CORSOptions) func(http.Handler) http.Handler {
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
