package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionIssuerRoundTrip(t *testing.T) {
	s := newSessionIssuer("shh-its-a-secret", time.Hour)

	token, err := s.IssueToken("user_1")
	require.NoError(t, err)
	require.NotEmpty(t, token)

	subject, err := s.ValidateToken(token)
	require.NoError(t, err)
	assert.Equal(t, "user_1", subject)
}

func TestSessionIssuerZeroTTLUsesDefault(t *testing.T) {
	s := newSessionIssuer("secret", 0)
	assert.Equal(t, DefaultTokenTTL, s.ttl)
}

func TestSessionIssuerRejectsExpiredToken(t *testing.T) {
	s := newSessionIssuer("secret", time.Hour)
	// Set ttl directly to bypass the constructor's <= 0 default, so this
	// actually issues an already-expired token rather than falling back to
	// DefaultTokenTTL.
	s.ttl = -time.Minute
	token, err := s.IssueToken("user_1")
	require.NoError(t, err)
	s.ttl = time.Hour

	_, err = s.ValidateToken(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestSessionIssuerRejectsWrongSecret(t *testing.T) {
	issuer := newSessionIssuer("secret-a", time.Hour)
	token, err := issuer.IssueToken("user_1")
	require.NoError(t, err)

	other := newSessionIssuer("secret-b", time.Hour)
	_, err = other.ValidateToken(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestSessionIssuerRejectsGarbage(t *testing.T) {
	s := newSessionIssuer("secret", time.Hour)
	_, err := s.ValidateToken("not-a-jwt-at-all")
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// signHS256ForTest builds a raw HS256 token from arbitrary claims, standing
// in for a token minted by something other than this package's own issuer
// (the admin package's issuer, or a hand-crafted token with a missing claim).
func signHS256ForTest(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	require.NoError(t, err)
	return signed
}

// TestSessionIssuerRejectsAdminAudience is the regression test for a
// privilege-escalation bug: a token signed with the SAME secret and shape
// the admin package uses (HS256, {sub,aud,iat,exp}, aud="keel:admin") must
// not validate here. Before the audience check existed, nothing distinguished
// an auth token from an admin token beyond the secret, so a shared secret let
// an end-user token pass admin.RequireAdmin.
func TestSessionIssuerRejectsAdminAudience(t *testing.T) {
	const sharedSecret = "shared-by-mistake"
	s := newSessionIssuer(sharedSecret, time.Hour)

	now := time.Now()
	token := signHS256ForTest(t, sharedSecret, jwt.MapClaims{
		"sub": "admin_1",
		"aud": "keel:admin",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})

	_, err := s.ValidateToken(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestSessionIssuerRejectsMissingAudience(t *testing.T) {
	const secret = "secret"
	s := newSessionIssuer(secret, time.Hour)

	now := time.Now()
	token := signHS256ForTest(t, secret, jwt.MapClaims{
		"sub": "user_1",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})

	_, err := s.ValidateToken(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// TestSessionIssuerRejectsAlgNone pins WithValidMethods([]string{"HS256"}):
// without it, a token whose header claims "alg":"none" and carries no
// signature at all would need to be rejected some other way, and there is
// no other check in ValidateToken that would catch it.
func TestSessionIssuerRejectsAlgNone(t *testing.T) {
	s := newSessionIssuer("secret", time.Hour)

	now := time.Now()
	claims := jwt.MapClaims{
		"sub": "user_1",
		"aud": sessionAudience,
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	token, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, err = s.ValidateToken(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// TestSessionIssuerRejectsRS256 pins the same guard against the direction an
// RS256/HS256 confusion attack would actually try: a token claiming RS256 (or
// any algorithm other than HS256) must be rejected regardless of what key
// material it was "signed" with.
func TestSessionIssuerRejectsRS256(t *testing.T) {
	s := newSessionIssuer("secret", time.Hour)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "user_1",
		"aud": sessionAudience,
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	signed, err := token.SignedString(priv)
	require.NoError(t, err)

	_, err = s.ValidateToken(signed)
	assert.ErrorIs(t, err, ErrInvalidToken)
}
