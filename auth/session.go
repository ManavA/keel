package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultTokenTTL is the session lifetime used when Options.TokenTTL is zero.
const DefaultTokenTTL = 7 * 24 * time.Hour

// sessionAudience is the required "aud" claim on every token this package
// issues and validates. The sibling admin package signs its own tokens with
// a different audience ("keel:admin"); the two are the same algorithm and
// claim shape otherwise, so without this check a token from one package
// validates against the other whenever both happen to share a signing
// secret. Deployments MUST use separate secrets for auth and admin anyway,
// but a shared secret is a configuration mistake this package can and does
// defend against on its own.
const sessionAudience = "keel:auth"

// ErrInvalidToken covers every way a bearer token can fail to validate: bad
// signature, wrong algorithm, wrong audience, missing or malformed claims,
// expiry. Callers that need to distinguish those cases should not — the
// response to a client is the same 401 regardless, and telling an attacker
// which check failed narrows their search.
var ErrInvalidToken = errors.New("auth: invalid token")

// sessionIssuer issues and validates the app's own JWT sessions. It holds the
// signing secret and default expiry; every method is safe for concurrent use
// since a jwt.Token carries no shared mutable state.
type sessionIssuer struct {
	secret string
	ttl    time.Duration
}

func newSessionIssuer(secret string, ttl time.Duration) *sessionIssuer {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	return &sessionIssuer{secret: secret, ttl: ttl}
}

// IssueToken signs a new session token for the given subject (typically a
// user ID) using the default expiry.
func (s *sessionIssuer) IssueToken(subject string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": subject,
		"aud": sessionAudience,
		"iat": now.Unix(),
		"exp": now.Add(s.ttl).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(s.secret))
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return signed, nil
}

// ValidateToken parses and verifies a session token and returns its subject.
// It accepts only HS256 — an attacker-supplied "alg" of "none" or an
// asymmetric algorithm the server never signed with must not be honored —
// and only the "keel:auth" audience, so a token minted by the admin package
// (or anything else sharing this secret) is rejected here.
func (s *sessionIssuer) ValidateToken(tokenString string) (string, error) {
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
		return []byte(s.secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(sessionAudience))
	if err != nil || !token.Valid {
		return "", ErrInvalidToken
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", ErrInvalidToken
	}
	subject, ok := claims["sub"].(string)
	if !ok || subject == "" {
		return "", ErrInvalidToken
	}
	return subject, nil
}
