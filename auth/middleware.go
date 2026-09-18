package auth

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const userIDKey contextKey = "auth.userID"

// UserIDFromContext returns the authenticated user ID, or "" if none is
// present. A comma-ok assertion, not a bare one: the documented answer for
// "absent" is "", and a bare assertion would turn "present but not a string"
// into a panic on a request whose only mistake was reaching this middleware
// through an unexpected path.
func UserIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(userIDKey).(string)
	return id
}

// RequireAuth is middleware that requires a valid bearer session token. It
// answers a generic 401 for anything wrong with the header or the token —
// missing, malformed, expired, wrong signature all look the same to a caller.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeGenericError(w, http.StatusUnauthorized)
			return
		}
		userID, err := s.session.Validate(r.Context(), token)
		if err != nil {
			writeGenericError(w, http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	prefix, token, found := strings.Cut(header, " ")
	if !found || prefix != "Bearer" || token == "" {
		return "", false
	}
	return token, true
}
