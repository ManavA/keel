package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOIDCIssuer is a minimal OIDC issuer for tests: it serves a discovery
// document and a JSON Web Key Set, and can mint tokens signed by whichever
// key the test currently holds.
type fakeOIDCIssuer struct {
	server *httptest.Server
	keys   []fakeOIDCKey // current key set, newest last

	// discoveryIssuer and discoveryJWKSURI override what the discovery
	// document reports, for tests of a hostile or misconfigured document.
	// Empty means "report the real values".
	discoveryIssuer   string
	discoveryJWKSURI  string
	discoveryRequests int
	jwksRequests      int
	extraJWKSKeys     []map[string]string
	mu                sync.Mutex
}

type fakeOIDCKey struct {
	kid     string
	private *rsa.PrivateKey
}

func newFakeOIDCIssuer(t *testing.T) *fakeOIDCIssuer {
	t.Helper()
	f := &fakeOIDCIssuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		f.discoveryRequests++
		issuer := f.discoveryIssuer
		if issuer == "" {
			issuer = f.server.URL
		}
		jwksURI := f.discoveryJWKSURI
		if jwksURI == "" {
			jwksURI = f.server.URL + "/jwks.json"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   issuer,
			"jwks_uri": jwksURI,
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.jwksRequests++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": f.jwks()})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	f.addKey(t, "key-1")
	return f
}

func (f *fakeOIDCIssuer) addKey(t *testing.T, kid string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f.keys = append(f.keys, fakeOIDCKey{kid: kid, private: priv})
}

// rotate replaces the served key set with a single new key, simulating a
// provider rotating its signing key.
func (f *fakeOIDCIssuer) rotate(t *testing.T, newKid string) {
	t.Helper()
	f.keys = nil
	f.addKey(t, newKid)
}

func (f *fakeOIDCIssuer) jwks() []map[string]string {
	out := make([]map[string]string, 0, len(f.keys)+len(f.extraJWKSKeys))
	for _, k := range f.keys {
		pub := k.private.PublicKey
		out = append(out, map[string]string{
			"kty": "RSA",
			"kid": k.kid,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(bigEndianExponent(pub.E)),
		})
	}
	out = append(out, f.extraJWKSKeys...)
	return out
}

func bigEndianExponent(e int) []byte {
	// Standard RSA public exponents (e.g. 65537) fit in 3 bytes; that is all
	// this fake issuer needs to produce.
	b := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
	i := 0
	for i < len(b)-1 && b[i] == 0 {
		i++
	}
	return b[i:]
}

// mint signs a token with the named key. claims lets a test override or add
// fields (e.g. a wrong audience or issuer) on top of sane defaults.
func (f *fakeOIDCIssuer) mint(t *testing.T, kid string, override func(jwt.MapClaims)) string {
	t.Helper()
	var key *rsa.PrivateKey
	for _, k := range f.keys {
		if k.kid == kid {
			key = k.private
		}
	}
	require.NotNil(t, key, "no such key %q on the fake issuer", kid)

	claims := jwt.MapClaims{
		"iss":            f.server.URL,
		"aud":            "test-audience",
		"sub":            "user-abc",
		"email":          "person@example.com",
		"email_verified": true,
		"name":           "Person Example",
		"iat":            time.Now().Unix(),
		"exp":            time.Now().Add(time.Hour).Unix(),
	}
	if override != nil {
		override(claims)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

func newTestOIDCVerifier(t *testing.T, issuer *fakeOIDCIssuer) *OIDCVerifier {
	t.Helper()
	v, err := NewOIDCVerifier(context.Background(), OIDCOptions{
		IssuerURL:           issuer.server.URL,
		Audience:            "test-audience",
		AllowInsecureIssuer: true, // httptest.NewServer is plain HTTP
	})
	require.NoError(t, err)
	return v
}

func TestOIDCVerifierAcceptsAValidToken(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	token := issuer.mint(t, "key-1", nil)
	identity, err := v.VerifyIDToken(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "user-abc", identity.UID)
	assert.Equal(t, "person@example.com", identity.Email)
	assert.True(t, identity.EmailVerified)
	assert.Equal(t, "Person Example", identity.Name)
}

func TestOIDCVerifierRejectsExpiredToken(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	token := issuer.mint(t, "key-1", func(c jwt.MapClaims) {
		c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
		c["exp"] = time.Now().Add(-time.Hour).Unix()
	})
	_, err := v.VerifyIDToken(context.Background(), token)
	assert.Error(t, err)
}

func TestOIDCVerifierRejectsWrongAudience(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	token := issuer.mint(t, "key-1", func(c jwt.MapClaims) {
		c["aud"] = "someone-elses-audience"
	})
	_, err := v.VerifyIDToken(context.Background(), token)
	assert.Error(t, err)
}

func TestOIDCVerifierRejectsWrongIssuer(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	token := issuer.mint(t, "key-1", func(c jwt.MapClaims) {
		c["iss"] = "https://not-the-configured-issuer.example.com"
	})
	_, err := v.VerifyIDToken(context.Background(), token)
	assert.Error(t, err)
}

func TestOIDCVerifierRejectsAlgNone(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	claims := jwt.MapClaims{
		"iss": issuer.server.URL,
		"aud": "test-audience",
		"sub": "user-abc",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	token, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, err = v.VerifyIDToken(context.Background(), token)
	assert.Error(t, err, "alg=none must never be accepted regardless of what the issuer would have signed")
}

func TestOIDCVerifierRejectsWrongKeyType(t *testing.T) {
	// A token signed with an HMAC secret derived from the RSA public key's
	// modulus (the classic RS256->HS256 confusion attack) must be rejected:
	// WithValidMethods([]string{"RS256"}) must refuse HS256 outright.
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	claims := jwt.MapClaims{
		"iss": issuer.server.URL,
		"aud": "test-audience",
		"sub": "user-abc",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	hsToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	hsToken.Header["kid"] = "key-1"
	signed, err := hsToken.SignedString([]byte("attacker-controlled-secret"))
	require.NoError(t, err)

	_, err = v.VerifyIDToken(context.Background(), signed)
	assert.Error(t, err)
}

func TestOIDCVerifierHandlesKeyRotation(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	// Token signed with the original key verifies fine.
	original := issuer.mint(t, "key-1", nil)
	_, err := v.VerifyIDToken(context.Background(), original)
	require.NoError(t, err)

	// The issuer rotates to a new key the verifier has never seen. A token
	// signed with it names a kid missing from the verifier's cache, which
	// must trigger a refetch rather than a rejection.
	issuer.rotate(t, "key-2")
	rotated := issuer.mint(t, "key-2", nil)

	identity, err := v.VerifyIDToken(context.Background(), rotated)
	require.NoError(t, err, "a token from a rotated-in key must verify after an on-miss refetch")
	assert.Equal(t, "user-abc", identity.UID)
}

func TestOIDCVerifierRejectsTokenFromRemovedKeyAfterRotation(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	original := issuer.mint(t, "key-1", nil)
	_, err := v.VerifyIDToken(context.Background(), original)
	require.NoError(t, err)

	// Force the verifier's cache to be considered stale, then rotate the key
	// out entirely. The next verification of the OLD token must refetch,
	// find the old key gone, and fail — not serve a stale local cache entry
	// forever.
	v.mu.Lock()
	v.fetchedAt = time.Time{}
	v.mu.Unlock()
	issuer.rotate(t, "key-2")

	_, err = v.VerifyIDToken(context.Background(), original)
	assert.Error(t, err)
}

func TestOIDCVerifierRejectsMissingKid(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	claims := jwt.MapClaims{
		"iss": issuer.server.URL,
		"aud": "test-audience",
		"sub": "user-abc",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	// No kid header set.
	signed, err := token.SignedString(issuer.keys[0].private)
	require.NoError(t, err)

	_, err = v.VerifyIDToken(context.Background(), signed)
	assert.Error(t, err)
}

func TestNewOIDCVerifierRequiresIssuerAndAudience(t *testing.T) {
	_, err := NewOIDCVerifier(context.Background(), OIDCOptions{Audience: "aud"})
	assert.Error(t, err)

	_, err = NewOIDCVerifier(context.Background(), OIDCOptions{IssuerURL: "https://example.com"})
	assert.Error(t, err)
}

func TestOIDCVerifierVerifyIDTokenCheckRevokedMatchesVerifyIDToken(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)
	token := issuer.mint(t, "key-1", nil)

	a, errA := v.VerifyIDToken(context.Background(), token)
	b, errB := v.VerifyIDTokenCheckRevoked(context.Background(), token)
	require.NoError(t, errA)
	require.NoError(t, errB)
	assert.Equal(t, a, b)
}

// TestOIDCVerifierRequiresExpiry is the regression test for a token that
// carries no exp claim at all: it must be rejected rather than treated as
// never expiring.
func TestOIDCVerifierRequiresExpiry(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	token := issuer.mint(t, "key-1", func(c jwt.MapClaims) {
		delete(c, "exp")
	})
	_, err := v.VerifyIDToken(context.Background(), token)
	assert.Error(t, err)
}

// TestOIDCVerifierAcceptsATrailingSlashIssuer is the regression test for the
// Auth0 preset: Auth0's issuer strings end with "/", and that trailing slash
// must survive into the exact string compared against a token's iss claim —
// trimming it during construction would make every real Auth0 token fail
// issuer validation.
func TestOIDCVerifierAcceptsATrailingSlashIssuer(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuerURLWithSlash := issuer.server.URL + "/"
	issuer.discoveryIssuer = issuerURLWithSlash // the fake issuer reports itself the way Auth0 does

	v, err := NewOIDCVerifier(context.Background(), OIDCOptions{
		IssuerURL:           issuerURLWithSlash,
		Audience:            "test-audience",
		AllowInsecureIssuer: true,
	})
	require.NoError(t, err)

	token := issuer.mint(t, "key-1", func(c jwt.MapClaims) {
		c["iss"] = issuerURLWithSlash
	})
	identity, err := v.VerifyIDToken(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "user-abc", identity.UID)
}

// TestNewOIDCVerifierRejectsDiscoveryIssuerMismatch is the regression test
// for RFC 8414 §3.3: the discovery document's own "issuer" field must equal
// the configured issuer exactly. A hostile or misconfigured document naming
// a different issuer must not be trusted.
func TestNewOIDCVerifierRejectsDiscoveryIssuerMismatch(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.discoveryIssuer = "https://not-the-real-issuer.example.com"

	_, err := NewOIDCVerifier(context.Background(), OIDCOptions{
		IssuerURL:           issuer.server.URL,
		Audience:            "test-audience",
		AllowInsecureIssuer: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}

// TestNewOIDCVerifierRejectsCrossOriginJWKSURI is the regression test for a
// discovery document pointing jwks_uri at a different host than the issuer.
// Nothing in the discovery protocol otherwise stops that, and this package
// would fetch and trust whatever keys that host serves.
func TestNewOIDCVerifierRejectsCrossOriginJWKSURI(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer evil.Close()

	issuer := newFakeOIDCIssuer(t)
	issuer.discoveryJWKSURI = evil.URL + "/jwks.json"

	_, err := NewOIDCVerifier(context.Background(), OIDCOptions{
		IssuerURL:           issuer.server.URL,
		Audience:            "test-audience",
		AllowInsecureIssuer: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "origin")
}

// TestOIDCVerifierRateLimitsOnMissKidRefetch is the regression test for an
// unbounded refetch storm: many tokens naming unknown kids, arriving faster
// than minRefetchInterval, must not each trigger their own JWKS fetch.
func TestOIDCVerifierRateLimitsOnMissKidRefetch(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	requestsBefore := issuer.jwksRequests
	for i := 0; i < 10; i++ {
		_, _ = v.keyForKID(context.Background(), fmt.Sprintf("bogus-kid-%d", i))
	}
	requestsAfter := issuer.jwksRequests

	assert.LessOrEqual(t, requestsAfter-requestsBefore, 1,
		"ten unknown kids within the rate-limit window must cause at most one refetch")
}

func TestOIDCVerifierConcurrentUnknownKidsCollapseToOneFetch(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	requestsBefore := issuer.jwksRequests
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = v.keyForKID(context.Background(), fmt.Sprintf("concurrent-bogus-%d", i))
		}(i)
	}
	wg.Wait()

	assert.LessOrEqual(t, issuer.jwksRequests-requestsBefore, 1,
		"concurrent unknown kids must collapse into a single fetch via singleflight")
}

// TestOIDCVerifierRejectsMultiAudienceToken is the regression test for a
// token whose aud claim names our audience AND a second one: jwt.WithAudience
// alone would accept it (our audience is contained in the claim), but this
// verifier's threat model refuses a token issued for more than one audience.
func TestOIDCVerifierRejectsMultiAudienceToken(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	token := issuer.mint(t, "key-1", func(c jwt.MapClaims) {
		c["aud"] = []string{"test-audience", "some-other-audience"}
	})
	_, err := v.VerifyIDToken(context.Background(), token)
	assert.Error(t, err)
}

func TestOIDCVerifierAcceptsSingleElementAudienceArray(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	v := newTestOIDCVerifier(t, issuer)

	token := issuer.mint(t, "key-1", func(c jwt.MapClaims) {
		c["aud"] = []string{"test-audience"}
	})
	_, err := v.VerifyIDToken(context.Background(), token)
	assert.NoError(t, err)
}

// TestNewOIDCVerifierRejectsPlainHTTPIssuer is the regression test for a
// plain-http issuer: both discovery and every JWKS fetch would travel
// unencrypted, so this is refused unless AllowInsecureIssuer opts in.
func TestNewOIDCVerifierRejectsPlainHTTPIssuer(t *testing.T) {
	issuer := newFakeOIDCIssuer(t) // httptest.NewServer, plain http

	_, err := NewOIDCVerifier(context.Background(), OIDCOptions{
		IssuerURL: issuer.server.URL,
		Audience:  "test-audience",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https")
}

// TestOIDCVerifierNeverLoadsANonRSAJWKSEntry pins a type invariant this
// verifier depends on structurally: keyForKID's key map is
// map[string]*rsa.PublicKey, and refreshKeys populates it only from JWKS
// entries whose "kty" is "RSA". A non-RSA entry — an EC key, say, from a
// compromised or MITM'd JWKS response — must never become a usable key
// under any kid, including one that collides with a real RSA key's kid on a
// key set that has not been refreshed since the collision was introduced.
func TestOIDCVerifierNeverLoadsANonRSAJWKSEntry(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.extraJWKSKeys = []map[string]string{
		{
			"kty": "EC",
			"kid": "ec-key-1",
			"use": "sig",
			"alg": "ES256",
			"crv": "P-256",
			"x":   base64.RawURLEncoding.EncodeToString([]byte("not-a-real-x-coordinate-00000")),
			"y":   base64.RawURLEncoding.EncodeToString([]byte("not-a-real-y-coordinate-00000")),
		},
	}
	v := newTestOIDCVerifier(t, issuer)

	_, err := v.keyForKID(context.Background(), "ec-key-1")
	require.Error(t, err, "a non-RSA JWKS entry must never be returned as a usable key")
}

func TestNewOIDCVerifierAllowsPlainHTTPIssuerWhenOptedIn(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)

	_, err := NewOIDCVerifier(context.Background(), OIDCOptions{
		IssuerURL:           issuer.server.URL,
		Audience:            "test-audience",
		AllowInsecureIssuer: true,
	})
	require.NoError(t, err)
}
