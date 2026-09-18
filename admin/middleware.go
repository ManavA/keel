package admin

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const adminIDKey contextKey = "admin.adminID"

// AdminIDFromContext returns the authenticated admin ID, or "" if none is
// present.
func AdminIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(adminIDKey).(string)
	return id
}

// RequireAdmin is middleware that requires a valid admin bearer session
// token. It answers a generic 401 for anything wrong with the header or the
// token.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeGenericError(w, http.StatusUnauthorized)
			return
		}
		adminID, err := s.session.ValidateToken(token)
		if err != nil {
			writeGenericError(w, http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), adminIDKey, adminID)
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

func writeGenericError(w http.ResponseWriter, status int) {
	msg := "invalid request"
	switch status {
	case http.StatusUnauthorized:
		msg = "invalid credentials"
	case http.StatusInternalServerError:
		msg = "internal error"
	case http.StatusServiceUnavailable:
		msg = "unavailable"
	}
	http.Error(w, msg, status)
}
