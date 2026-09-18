package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ForgotPasswordRequest is the body of POST /forgot-password.
type ForgotPasswordRequest struct {
	Email string `json:"email"`
}

// ForgotPassword handles POST /forgot-password. It always answers 200 with
// the same body regardless of what happened.
//
// Answering differently is the one way this endpoint could leak which
// emails are registered, so the real work (does the account exist? does it
// have a password to reset? did the send succeed?) happens best-effort
// behind an identical response. That is a deliberate trade-off: an attacker
// who already suspects an address is registered learns nothing new from
// this endpoint either way, and the alternative — refusing an unknown email
// outright — hands them a working account-enumeration oracle instead.
func (s *Service) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req ForgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	email := normalizeEmail(req.Email)

	if email != "" && s.passwordResets != nil && s.emailer != nil {
		if user, err := s.users.GetByEmail(r.Context(), email); err == nil && user.PasswordHash != "" {
			if err := s.sendPasswordReset(r.Context(), user); err != nil {
				s.logger(r.Context()).Error("auth: failed to send password reset email", "user_id", user.ID, "error", err)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message": "If an account exists for that email, a reset link is on its way.",
	})
}

func (s *Service) sendPasswordReset(ctx context.Context, user *User) error {
	raw, err := GenerateVerificationToken() // same shape of token as email verification; different store, different purpose
	if err != nil {
		return fmt.Errorf("auth: generate password reset token: %w", err)
	}
	if err := s.passwordResets.InvalidateForUser(ctx, user.ID); err != nil {
		s.logger(ctx).Error("auth: failed to invalidate previous password reset tokens", "error", err)
	}
	if err := s.passwordResets.Create(ctx, user.ID, HashVerificationToken(raw), time.Now().Add(PasswordResetTokenTTL)); err != nil {
		return fmt.Errorf("auth: store password reset token: %w", err)
	}
	resetURL := fmt.Sprintf("%s/reset-password?token=%s", s.siteURL, raw)
	return s.emailer.Send(ctx, user.Email, "password-reset", resetURL)
}

// ResetPasswordRequest is the body of POST /reset-password.
type ResetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// ResetPassword handles POST /reset-password. Unauthenticated on purpose: the
// link is opened from an inbox, and the token itself is the credential.
//
// On success it revokes every session belonging to the account
// (s.session.RevokeAllForUser), not just this request's own. Without that, a
// session token stolen before the reset — the exact scenario a reset is
// meant to recover from — would stay valid after the account holder had
// just proven they, not the attacker, control the account.
func (s *Service) ResetPassword(w http.ResponseWriter, r *http.Request) {
	if s.passwordResets == nil {
		writeGenericError(w, http.StatusServiceUnavailable)
		return
	}
	var req ResetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := validatePasswordLength(req.NewPassword); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	userID, err := s.passwordResets.ConsumeValid(r.Context(), HashVerificationToken(req.Token))
	if err != nil {
		http.Error(w, "this reset link is invalid or has expired", http.StatusBadRequest)
		return
	}

	hash, err := HashPassword(req.NewPassword)
	if err != nil {
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	if err := s.users.SetPasswordHash(r.Context(), userID, hash); err != nil {
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	if err := s.session.RevokeAllForUser(r.Context(), userID); err != nil {
		s.logger(r.Context()).Error("auth: failed to revoke sessions after password reset", "user_id", userID, "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "Your password has been reset. You can sign in now.",
		// See Logout's doc comment: under SessionJWT this is false, because
		// a token issued before the reset keeps validating until it
		// expires regardless of this call.
		sessionsRevokedKey: s.session.Revocable(),
	})
}
