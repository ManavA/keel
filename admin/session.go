package admin

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultTokenTTL is how long an issued admin session token is valid. Short
// by default: an admin token is privileged, and this package has no
// server-side revocation, so the console re-authenticates often rather than
// carrying a long-lived credential.
const DefaultTokenTTL = 12 * time.Hour

// sessionAudience is the required "aud" claim on every token this package
// issues and validates. The sibling auth package signs its own tokens with a
// different audience ("keel:auth"); without this check, a token from either
// package validates against the other whenever both happen to share a
// signing secret. Deployments MUST use separate secrets for auth and admin —
// an admin token is privileged in a way an end-user token is not — but a
// shared secret is a configuration mistake this package can and does defend
// against on its own.
const sessionAudience = "keel:admin"

// ErrInvalidToken covers every way a bearer token can fail to validate: bad
// signature, wrong algorithm, wrong audience, missing or malformed claims,
// expiry. A caller does not need to know which check failed, and the HTTP
// response for each is the same 401 regardless.
var ErrInvalidToken = errors.New("admin: invalid token")

// sessionIssuer issues and validates this package's JWT sessions. It is a
// separate implementation from auth's (see that package's session.go): admin
// and auth sit at the same layer and neither imports the other, so each
// carries its own small JWT wrapper rather than sharing one.
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
		return "", fmt.Errorf("admin: sign token: %w", err)
	}
	return signed, nil
}

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
