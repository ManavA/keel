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
// token. It answers a generic 401 for anything wrong with the header, the
// token, or the admin it names: an unknown admin, a token revoked since it
// was issued, and a store failure all read the same from outside, so a
// revoked credential is indistinguishable from a forged one.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeGenericError(w, http.StatusUnauthorized)
			return
		}
		adminID, epoch, err := s.session.ValidateToken(token)
		if err != nil {
			writeGenericError(w, http.StatusUnauthorized)
			return
		}
		admin, err := s.users.GetByID(r.Context(), adminID)
		if err != nil {
			// Fail closed either way: a deleted admin's tokens stop working,
			// and an outage reads as unauthenticated rather than as success.
			// The distinction is logged, not answered, so the status stays
			// generic.
			s.logger(r.Context()).Warn("admin: session admin lookup failed", "admin_id", adminID, "error", err)
			writeGenericError(w, http.StatusUnauthorized)
			return
		}
		if epoch != admin.SessionEpoch {
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
