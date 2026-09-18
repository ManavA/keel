package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// issueVerificationEmail mints a single-use link and sends it. It is the one
// definition used by both Signup and ResendVerification, so the two flows
// cannot drift into "resend was wired up and signup was not" — the defect
// this package exists to avoid repeating.
//
// Returns nil only when the provider accepted the message. A nil
// Verifications store or Emailer means the flow is not configured, which is
// reported as an error here rather than silently skipped — the caller (see
// Signup) uses that to answer VerificationSent honestly.
func (s *Service) issueVerificationEmail(ctx context.Context, user *User) error {
	if s.verifications == nil || s.emailer == nil {
		return errVerificationUnavailable
	}

	raw, err := GenerateVerificationToken()
	if err != nil {
		return fmt.Errorf("auth: generate verification token: %w", err)
	}
	// Retire any earlier link first, so a resend does not leave a trail of
	// live tokens with the oldest email in the inbox still working.
	if err := s.verifications.InvalidateForUser(ctx, user.ID); err != nil {
		s.logger(ctx).Error("auth: failed to invalidate previous verification tokens", "error", err)
	}
	if err := s.verifications.Create(ctx, user.ID, HashVerificationToken(raw), time.Now().Add(VerificationTokenTTL)); err != nil {
		return fmt.Errorf("auth: store verification token: %w", err)
	}

	verifyURL := fmt.Sprintf("%s/verify-email?token=%s", s.siteURL, raw)
	if err := s.emailer.Send(ctx, user.Email, "verify-email", verifyURL); err != nil {
		return fmt.Errorf("auth: send verification email: %w", err)
	}
	return nil
}

var errVerificationUnavailable = errors.New("auth: email verification is not configured")

// emailVerifiedKey is the response field name used by every endpoint in
// this file that reports verification status.
const emailVerifiedKey = "email_verified"

// VerifyEmailRequest is the body of POST /verify-email.
type VerifyEmailRequest struct {
	Token string `json:"token"`
}

// VerifyEmail handles POST /verify-email. Unauthenticated on purpose: the
// link is opened from an inbox, on whichever device is to hand, and the
// token itself is the credential.
func (s *Service) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	if s.verifications == nil {
		writeGenericError(w, http.StatusServiceUnavailable)
		return
	}
	var req VerifyEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Token) == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	userID, err := s.verifications.ConsumeValid(r.Context(), HashVerificationToken(req.Token))
	if err != nil {
		http.Error(w, "this verification link is invalid or has expired", http.StatusBadRequest)
		return
	}
	if err := s.users.SetEmailVerified(r.Context(), userID); err != nil {
		s.logger(r.Context()).Error("auth: failed to mark email verified", "user_id", userID, "error", err)
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{emailVerifiedKey: true})
}

// ResendVerification handles POST /resend-verification (requires
// RequireAuth). A failure here answers 502 rather than a cheerful 200:
// claiming an email was sent when it was not defeats the point of the
// endpoint.
func (s *Service) ResendVerification(w http.ResponseWriter, r *http.Request) {
	userID := UserIDFromContext(r.Context())
	if userID == "" {
		writeGenericError(w, http.StatusUnauthorized)
		return
	}
	if s.verifications == nil || s.emailer == nil {
		writeGenericError(w, http.StatusServiceUnavailable)
		return
	}

	user, err := s.users.GetByID(r.Context(), userID)
	if err != nil {
		writeGenericError(w, http.StatusNotFound)
		return
	}
	if user.EmailVerified {
		writeJSON(w, http.StatusOK, map[string]bool{emailVerifiedKey: true, "sent": false})
		return
	}

	if err := s.issueVerificationEmail(r.Context(), user); err != nil {
		s.logger(r.Context()).Error("auth: failed to send verification email", "user_id", userID, "error", err)
		http.Error(w, "could not send the verification email", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{emailVerifiedKey: false, "sent": true})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
