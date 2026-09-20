package admin

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultTokenTTL is how long an issued admin session token is valid. Short
// by default: an admin token is privileged, and expiry is its main lifetime
// control — revocation ends sessions explicitly (see RevokeSessions), but the
// console still re-authenticates often rather than carrying a long-lived
// credential.
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

// sessionEpochClaim carries the admin's SessionEpoch at the moment the token
// was issued. RevokeSessions moves the stored epoch forward, so a token
// carrying an older epoch stops validating even though its signature and
// expiry are still good.
const sessionEpochClaim = "epoch"

// sessionIssuer issues and validates this package's JWT sessions. It is a
// separate implementation from auth's (see that package's session.go): admin
// and auth sit at the same layer and neither imports the other, so each
// carries its own small JWT wrapper rather than sharing one.
//
// The ttl is the token's absolute lifetime: exp is always iat plus ttl, and
// no activity extends it. There is deliberately no idle timeout here. These
// tokens are stateless — validation is a signature and expiry check with no
// storage lookup — so there is no record of last activity to measure idleness
// against. A deployment that needs idle control must keep ttl short and have
// the console re-authenticate; see Options.TokenTTL.
//
// Revocation is the one check that does reach the store, and it happens
// outside this type: ValidateToken hands back the token's epoch, and the
// caller (RequireAdmin) compares it against the admin's current SessionEpoch.
// See AdminStore.RevokeSessions.
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

func (s *sessionIssuer) IssueToken(subject string, epoch int64) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":             subject,
		"aud":             sessionAudience,
		sessionEpochClaim: epoch,
		"iat":             now.Unix(),
		"exp":             now.Add(s.ttl).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(s.secret))
	if err != nil {
		return "", fmt.Errorf("admin: sign token: %w", err)
	}
	return signed, nil
}

func (s *sessionIssuer) ValidateToken(tokenString string) (subject string, epoch int64, err error) {
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
		return []byte(s.secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(sessionAudience))
	if err != nil || !token.Valid {
		return "", 0, ErrInvalidToken
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", 0, ErrInvalidToken
	}
	subject, ok = claims["sub"].(string)
	if !ok || subject == "" {
		return "", 0, ErrInvalidToken
	}
	epoch, ok = epochFromClaims(claims)
	if !ok {
		return "", 0, ErrInvalidToken
	}
	return subject, epoch, nil
}

// epochFromClaims reads the session epoch back out of validated claims. JSON
// numbers decode as float64; anything else — a string, a bool, a missing
// claim on a token minted before epochs existed — is not a token this package
// issued and does not validate.
func epochFromClaims(claims jwt.MapClaims) (int64, bool) {
	raw, ok := claims[sessionEpochClaim]
	if !ok {
		return 0, false
	}
	f, ok := raw.(float64)
	if !ok || f != float64(int64(f)) || f < 0 {
		return 0, false
	}
	return int64(f), true
}
