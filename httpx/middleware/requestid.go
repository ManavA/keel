package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/ManavA/keel/log"
)

// RequestIDHeader is the header a request id is read from and echoed back in.
const RequestIDHeader = "X-Request-Id"

// RequestIDOptions configures RequestID.
type RequestIDOptions struct {
	// TrustInbound accepts a client-supplied X-Request-Id instead of generating
	// one, so a trace spans the whole system rather than one service. It also
	// lets a caller choose a string that will appear in your logs; inbound ids
	// are length capped and stripped to a safe character set first.
	TrustInbound bool
}

// RequestID puts an id on every request: into the context, where log picks it
// up, and into the response header, so a user reporting an error can quote
// something searchable.
func RequestID(opts RequestIDOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := ""
			if opts.TrustInbound {
				id = sanitizeRequestID(r.Header.Get(RequestIDHeader))
			}
			if id == "" {
				id = newRequestID()
			}

			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(log.WithRequestID(r.Context(), id)))
		})
	}
}

const maxRequestIDLen = 64

func sanitizeRequestID(raw string) string {
	if len(raw) > maxRequestIDLen {
		raw = raw[:maxRequestIDLen]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		default:
			return -1
		}
	}, raw)
}

func newRequestID() string {
	var b [12]byte
	// crypto/rand.Read does not fail on any supported platform; since Go 1.24
	// it panics rather than returning an error, so there is nothing to handle.
	rand.Read(b[:]) //nolint:errcheck // documented never to fail
	return hex.EncodeToString(b[:])
}
