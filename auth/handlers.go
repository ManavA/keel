package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/ManavA/keel/textpolicy"
)

var emailPattern = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// normalizeEmail is the one place every email address is trimmed and
// lowercased before being used to look up or create a User. Applying it
// inconsistently is exactly how a UserStore that compares emails by exact
// string (MemoryUserStore does; a citext-backed pg store does not need it
// but still gets it) ends up with two accounts for "Person@example.com"
// and "person@example.com" — including an identity-token claim, which
// arrives however the provider capitalized it, not necessarily lowercase.
func normalizeEmail(email string) string {
	return strings.TrimSpace(strings.ToLower(email))
}

const minPasswordLength = 8

// maxPasswordLength matches bcrypt's own limit: it reads only the first 72
// bytes of its input and silently ignores the rest. Without this check, two
// passwords that differ only after byte 72 hash identically, which is a
// surprising way to lock an account or, worse, generate a weaker credential
// than the one someone thought they set.
const maxPasswordLength = 72

// validatePasswordLength rejects a password bcrypt would silently truncate,
// or one too short to be worth hashing at all.
func validatePasswordLength(password string) error {
	if len(password) < minPasswordLength {
		return fmt.Errorf("password must be at least %d characters", minPasswordLength)
	}
	if len(password) > maxPasswordLength {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordLength)
	}
	return nil
}

// SignupRequest is the body of POST /signup.
type SignupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginRequest is the body of POST /login.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// ExchangeIDTokenRequest is the body of POST /exchange.
type ExchangeIDTokenRequest struct {
	IDToken string `json:"id_token"`
}

// SessionResponse is returned by every endpoint that establishes or refreshes
// a session.
type SessionResponse struct {
	Token string `json:"token"`
	User  struct {
		ID            string `json:"id"`
		Email         string `json:"email"`
		Name          string `json:"name,omitempty"`
		EmailVerified bool   `json:"email_verified"`
	} `json:"user"`
	// VerificationSent reports whether a verification link actually went out
	// on signup. It belongs in the response, not only the log: the client is
	// what tells the person "check your inbox", and it must not say that when
	// the send failed.
	VerificationSent bool `json:"verification_sent,omitempty"`
}

func sessionResponse(token string, u *User) SessionResponse {
	resp := SessionResponse{Token: token}
	resp.User.ID = u.ID
	resp.User.Email = u.Email
	resp.User.Name = u.Name
	resp.User.EmailVerified = u.EmailVerified
	return resp
}

// Signup handles POST /signup: create a password account and an initial
// session, and best-effort send a verification email. Mounted only when
// Options.Sources includes SourceLocal.
func (s *Service) Signup(w http.ResponseWriter, r *http.Request) {
	var req SignupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	email := normalizeEmail(req.Email)
	if email == "" || !emailPattern.MatchString(email) {
		http.Error(w, "a valid email address is required", http.StatusBadRequest)
		return
	}
	if err := validatePasswordLength(req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.rejectBreachedPassword(w, r, req.Password) {
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		writeGenericError(w, http.StatusInternalServerError)
		return
	}

	user := &User{Email: email, PasswordHash: hash}
	if err := s.users.Create(r.Context(), user); err != nil {
		if errors.Is(err, ErrDuplicateEmail) {
			// A 409 here does confirm the email is registered — a deliberate
			// trade-off, not an oversight: a signup form needs to tell a
			// returning user "sign in instead" rather than accept a second
			// account for the same address, and there is no way to do that
			// without the response depending on whether the account already
			// existed. ForgotPassword makes the opposite trade-off, because
			// it has no comparable need to.
			writeGenericError(w, http.StatusConflict)
			return
		}
		writeGenericError(w, http.StatusInternalServerError)
		return
	}

	token, err := s.session.Issue(r.Context(), user.ID)
	if err != nil {
		writeGenericError(w, http.StatusInternalServerError)
		return
	}

	resp := sessionResponse(token, user)
	resp.VerificationSent = s.issueVerificationEmail(r.Context(), user) == nil

	writeJSON(w, http.StatusCreated, resp)
}

// Login handles POST /login. Mounted only when Options.Sources includes
// SourceLocal.
//
// Beyond the per-IP limiter in front of it, Login throttles failed attempts
// per account email (see Options.AccountRateLimit): a spray that spreads a
// few attempts per IP across many IPs still shares one per-account bucket.
// Every outcome is recorded to Options.Attempts — success and failure, never
// password material — so the attempt history is auditable. The throttle
// answers an unknown email exactly like a known one, keeping the fixed-string
// contract that stops login from enumerating registered addresses.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	email := normalizeEmail(req.Email)
	if email != "" && s.accountLoginBlocked(r.Context(), email) {
		// Recorded as a failure like any other: the audit trail should show
		// the attempts that arrived during a lockout, not go quiet for it.
		s.recordLoginAttempt(r.Context(), r, LoginAttempt{Email: email, IP: clientIPFromRequest(r)})
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	user, err := s.users.GetByEmail(r.Context(), email)
	switch {
	case errors.Is(err, ErrUserNotFound):
		// Burn a comparison anyway so timing does not reveal that the email
		// was the reason this failed rather than the password.
		burnTimingEqualizer(req.Password)
		s.recordLoginAttempt(r.Context(), r, LoginAttempt{Email: email, IP: clientIPFromRequest(r)})
		writeGenericError(w, http.StatusUnauthorized)
		return
	case err != nil:
		// A real store failure, not "no such user" — reporting it as
		// invalid credentials would tell an operator debugging an outage
		// that every login attempt was simply wrong, and tell a caller
		// retrying makes no more sense than it would for any other
		// endpoint's 401.
		s.logger(r.Context()).Error("auth: login failed to look up user", "error", err)
		writeGenericError(w, http.StatusServiceUnavailable)
		return
	case user.PasswordHash == "":
		// An account that only ever signed in via an identity token: burn a
		// comparison anyway so timing does not reveal that either.
		burnTimingEqualizer(req.Password)
		s.recordLoginAttempt(r.Context(), r, LoginAttempt{Email: email, IP: clientIPFromRequest(r)})
		writeGenericError(w, http.StatusUnauthorized)
		return
	}
	if !ComparePassword(user.PasswordHash, req.Password) {
		s.recordLoginAttempt(r.Context(), r, LoginAttempt{Email: email, IP: clientIPFromRequest(r)})
		writeGenericError(w, http.StatusUnauthorized)
		return
	}

	token, err := s.session.Issue(r.Context(), user.ID)
	if err != nil {
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	s.recordLoginAttempt(r.Context(), r, LoginAttempt{Email: email, Success: true, IP: clientIPFromRequest(r)})
	writeJSON(w, http.StatusOK, sessionResponse(token, user))
}

// Refresh handles POST /refresh (requires RequireAuth). It issues a fresh
// token for the caller's own session, after confirming the account still
// exists.
func (s *Service) Refresh(w http.ResponseWriter, r *http.Request) {
	userID := UserIDFromContext(r.Context())
	if userID == "" {
		writeGenericError(w, http.StatusUnauthorized)
		return
	}
	user, err := s.users.GetByID(r.Context(), userID)
	if err != nil {
		writeGenericError(w, http.StatusUnauthorized)
		return
	}
	token, err := s.session.Issue(r.Context(), user.ID)
	if err != nil {
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse(token, user))
}

// ExchangeIDToken handles POST /exchange: verify an identity token from the
// configured IDTokenVerifier, resolve it to a user (linking or registering as
// needed — see resolveIdentityUser), and issue this package's own session
// token. Answers 503 if no verifier is configured (see SetIDTokenVerifier)
// rather than panicking.
func (s *Service) ExchangeIDToken(w http.ResponseWriter, r *http.Request) {
	verifier := s.idTokenVerifier()
	if verifier == nil {
		writeGenericError(w, http.StatusServiceUnavailable)
		return
	}

	var req ExchangeIDTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.IDToken) == "" {
		http.Error(w, "id_token is required", http.StatusBadRequest)
		return
	}

	// This is the session-establishing exchange, so pay for the
	// revocation/disabled-account check here; a per-request path would stay
	// offline-only.
	identity, err := verifier.VerifyIDTokenCheckRevoked(r.Context(), req.IDToken)
	if err != nil {
		s.logger(r.Context()).Warn("auth: identity token verification failed", "error", err)
		writeGenericError(w, http.StatusUnauthorized)
		return
	}

	user, err := s.resolveIdentityUser(r.Context(), identity)
	if err != nil {
		if errors.Is(err, ErrDuplicateEmail) {
			writeGenericError(w, http.StatusConflict)
			return
		}
		s.logger(r.Context()).Error("auth: failed to resolve identity to a user", "error", err)
		writeGenericError(w, http.StatusInternalServerError)
		return
	}

	token, err := s.session.Issue(r.Context(), user.ID)
	if err != nil {
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse(token, user))
}

// resolveIdentityUser turns a verified Identity into a User: the existing
// account if this external UID has signed in before, an existing SourceLocal
// account with the same verified email if one exists, or a newly registered
// account.
//
// Linking to an existing account requires identity.EmailVerified. An
// unverified email claim does not establish that the signer controls the
// address, so it is never used to link accounts; doing so would let anyone
// take over an existing account by entering its owner's email at a provider
// that does not verify addresses.
func (s *Service) resolveIdentityUser(ctx context.Context, identity Identity) (*User, error) {
	// Applied once, here, rather than trusting every call site to remember:
	// a provider's email claim arrives capitalized however that provider
	// chose, and MemoryUserStore compares emails as exact strings — an
	// un-normalized claim would fail to find (and so would fork a second
	// account for) an existing user whose email differs only in case.
	identity.Email = normalizeEmail(identity.Email)

	user, err := s.users.GetByExternalUID(ctx, identity.UID)
	switch {
	case err == nil:
		if identity.EmailVerified && !user.EmailVerified {
			if err := s.users.SetEmailVerified(ctx, user.ID); err != nil {
				s.logger(ctx).Error("auth: failed to record email verification", "error", err, "user_id", user.ID)
			}
			user.EmailVerified = true
		}
		return user, nil
	case !errors.Is(err, ErrUserNotFound):
		return nil, err
	}

	if identity.Email != "" && identity.EmailVerified {
		if existing, linkErr := s.users.GetByEmail(ctx, identity.Email); linkErr == nil {
			if err := s.users.LinkExternalUID(ctx, existing.ID, identity.UID); err != nil {
				return nil, err
			}
			existing.ExternalUID = identity.UID
			if !existing.EmailVerified {
				if err := s.users.SetEmailVerified(ctx, existing.ID); err != nil {
					s.logger(ctx).Error("auth: failed to record email verification", "error", err, "user_id", existing.ID)
				}
				existing.EmailVerified = true
			}
			return existing, nil
		}
	}

	name, dropped := sanitizeDisplayName(identity.Name)
	if dropped {
		// Logged rather than silently discarded: a non-empty claim that
		// sanitizes to nothing is a sign the input was never a name (control
		// characters, an encoding trick), and that is worth an operator's
		// attention on a NEW account.
		s.logger(ctx).Warn("auth: dropped an unusable display name from an identity token", "external_uid", identity.UID)
	}

	newUser := &User{Email: identity.Email, Name: name, ExternalUID: identity.UID, EmailVerified: identity.EmailVerified}
	if err := s.users.Create(ctx, newUser); err != nil {
		return nil, err
	}
	return newUser, nil
}

// maxDisplayNameLength is a count of runes, not bytes: clamping by byte
// count could cut a multi-byte UTF-8 character in half and store an invalid
// string.
const maxDisplayNameLength = 200

// sanitizeDisplayName cleans a display-name claim before it is stored: line
// and tab breaks become spaces, remaining control characters and every
// Default_Ignorable_Code_Point character are stripped (via
// textpolicy.Normalize — the same normalization this toolkit's text-policy
// guard applies, since a display name is exactly the kind of externally
// supplied text a zero-width or bidi-control character could otherwise slip
// through unnoticed), internal whitespace is collapsed, and the result is
// clamped to maxDisplayNameLength runes. It reports dropped=true when raw
// was non-empty but nothing usable survived, so the caller can log that
// rather than silently storing an empty name.
//
// A display-name claim comes from the identity provider account, not from
// this system, and nothing upstream validates it.
func sanitizeDisplayName(raw string) (name string, dropped bool) {
	trimmed := strings.TrimSpace(raw)

	withoutControl := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, trimmed)

	cleaned := textpolicy.Normalize(withoutControl)

	if runes := []rune(cleaned); len(runes) > maxDisplayNameLength {
		cleaned = strings.TrimSpace(string(runes[:maxDisplayNameLength]))
	}
	return cleaned, trimmed != "" && cleaned == ""
}
