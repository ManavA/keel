package auth

import "net/http"

// DeleteAccount handles POST /delete-account (requires RequireAuth). The id
// comes from the verified session and NOWHERE ELSE: there is deliberately no
// path parameter and no body, so a delete-any-account endpoint one typo away
// from delete-my-account is not something this package can produce.
//
// It revokes every session belonging to the account before returning, the
// same as ResetPassword, and reports whether that revocation actually ends
// a token before it expires (see Service.session's Revocable) rather than
// answering a bare success that reads the same either way — under
// SessionJWT a token issued before the deletion keeps authenticating
// against a UserStore call that will fail for a since-deleted id, but it
// keeps validating as a TOKEN until it expires, and a caller integrating
// this deserves to know that rather than assume "deleted" means "revoked
// everywhere immediately".
//
// Deleting the local User row does not reach any external identity (a
// Firebase or OIDC account, if this one was linked to one) — that is a
// stated limit, not an oversight: this package owns the local account, not
// the external provider's. A caller whose privacy commitments cover the
// external identity too must delete it there as well.
func (s *Service) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	userID := UserIDFromContext(r.Context())
	if userID == "" {
		writeGenericError(w, http.StatusUnauthorized)
		return
	}
	if err := s.users.Delete(r.Context(), userID); err != nil {
		s.logger(r.Context()).Error("auth: account deletion failed", "user_id", userID, "error", err)
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	if err := s.session.RevokeAllForUser(r.Context(), userID); err != nil {
		s.logger(r.Context()).Error("auth: failed to revoke sessions after account deletion", "user_id", userID, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{sessionsRevokedKey: s.session.Revocable()})
}

// Logout handles POST /logout (requires RequireAuth). It revokes the
// caller's own session token and reports whether that revocation actually
// ends it before it expires: true under SessionOpaque (the default);
// false under SessionJWT, where a self-contained token keeps validating on
// any server holding the secret regardless (see jwtSessionBackend.Revoke) —
// the CLIENT-side half of logout (discarding the token) still happened, but
// a caller that reports "you have been logged out" on the strength of this
// response alone would be telling its user something only half true under
// SessionJWT, and sessions_revoked is what lets it say the accurate thing.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		writeGenericError(w, http.StatusUnauthorized)
		return
	}
	if err := s.session.Revoke(r.Context(), token); err != nil {
		s.logger(r.Context()).Error("auth: failed to revoke session on logout", "error", err)
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{sessionsRevokedKey: s.session.Revocable()})
}
