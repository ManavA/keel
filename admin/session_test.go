package admin

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
	s := newSessionIssuer("shh", time.Hour)
	token, err := s.IssueToken("admin_1")
	require.NoError(t, err)

	subject, err := s.ValidateToken(token)
	require.NoError(t, err)
	assert.Equal(t, "admin_1", subject)
}

// TestSessionIssuerTokenCarriesAbsoluteLifetime pins the admin session's one
// lifetime control: exp is exactly iat plus the configured ttl, so the token
// carries a fixed absolute lifetime that no activity extends. Admin tokens
// are stateless JWTs with no activity record, so there is no idle window to
// pin — TokenTTL is the whole lifetime model, and deployments that need idle
// control must keep it short and require re-login.
func TestSessionIssuerTokenCarriesAbsoluteLifetime(t *testing.T) {
	const ttl = 12 * time.Hour
	const secret = "lifetime-test-secret"
	s := newSessionIssuer(secret, ttl)

	token, err := s.IssueToken("admin_1")
	require.NoError(t, err)

	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	require.NoError(t, err)

	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok, "issuer must sign MapClaims")
	iat, err := claims.GetIssuedAt()
	require.NoError(t, err)
	exp, err := claims.GetExpirationTime()
	require.NoError(t, err)
	assert.Equal(t, ttl, exp.Sub(iat.Time), "exp must be exactly iat plus ttl: the absolute lifetime")
}

func TestSessionIssuerZeroTTLUsesDefault(t *testing.T) {
	s := newSessionIssuer("secret", 0)
	assert.Equal(t, DefaultTokenTTL, s.ttl)
}

func TestSessionIssuerRejectsExpiredToken(t *testing.T) {
	s := newSessionIssuer("secret", time.Hour)
	s.ttl = -time.Minute
	token, err := s.IssueToken("admin_1")
	require.NoError(t, err)
	s.ttl = time.Hour

	_, err = s.ValidateToken(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestSessionIssuerRejectsWrongSecret(t *testing.T) {
	issuer := newSessionIssuer("secret-a", time.Hour)
	token, err := issuer.IssueToken("admin_1")
	require.NoError(t, err)

	other := newSessionIssuer("secret-b", time.Hour)
	_, err = other.ValidateToken(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestSessionIssuerRejectsGarbage(t *testing.T) {
	s := newSessionIssuer("secret", time.Hour)
	_, err := s.ValidateToken("not-a-jwt")
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// TestSessionIssuerRejectsAuthAudience is the regression test for the
// privilege-escalation this package's audience check exists to prevent: a
// token signed with the SAME secret and shape the auth package uses (HS256,
// {sub,aud,iat,exp}, aud="keel:auth") must not pass RequireAdmin. Before the
// audience check existed, an end-user session token validated here whenever
// the two packages shared a signing secret.
func TestSessionIssuerRejectsAuthAudience(t *testing.T) {
	const sharedSecret = "shared-by-mistake"
	s := newSessionIssuer(sharedSecret, time.Hour)

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user_1",
		"aud": "keel:auth",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	signed, err := token.SignedString([]byte(sharedSecret))
	require.NoError(t, err)

	_, err = s.ValidateToken(signed)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestSessionIssuerRejectsMissingAudience(t *testing.T) {
	const secret = "secret"
	s := newSessionIssuer(secret, time.Hour)

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "admin_1",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	signed, err := token.SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = s.ValidateToken(signed)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// TestSessionIssuerRejectsAlgNone pins WithValidMethods([]string{"HS256"})
// on this package's own issuer, mirroring auth's identical test.
func TestSessionIssuerRejectsAlgNone(t *testing.T) {
	s := newSessionIssuer("secret", time.Hour)

	now := time.Now()
	claims := jwt.MapClaims{
		"sub": "admin_1",
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
// RS256/HS256 confusion attack would actually try.
func TestSessionIssuerRejectsRS256(t *testing.T) {
	s := newSessionIssuer("secret", time.Hour)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "admin_1",
		"aud": sessionAudience,
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	signed, err := token.SignedString(priv)
	require.NoError(t, err)

	_, err = s.ValidateToken(signed)
	assert.ErrorIs(t, err, ErrInvalidToken)
}
