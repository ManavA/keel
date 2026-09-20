package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
)

// DefaultJWKSCacheTTL is how long OIDCVerifier keeps a fetched key set before
// refetching it on a schedule, independent of the on-miss refetch described
// on OIDCVerifier.
const DefaultJWKSCacheTTL = 10 * time.Minute

// minRefetchInterval bounds how often an unrecognized key id can trigger a
// network fetch, regardless of how many distinct unknown kids arrive. A
// caller sending tokens with random or bogus kid values would otherwise force
// one HTTP request per token; this turns that into at most one fetch per
// interval.
const minRefetchInterval = 5 * time.Second

// defaultHTTPTimeout bounds a discovery or JWKS request when the caller does
// not supply an HTTPClient. http.DefaultClient has no timeout at all, which
// would let an unresponsive issuer hang a verification indefinitely.
const defaultHTTPTimeout = 10 * time.Second

// maxDiscoveryBodyBytes and maxJWKSBodyBytes bound how much of a response
// this package reads, so a compromised or misconfigured issuer cannot exhaust
// memory with an oversized response.
const (
	maxDiscoveryBodyBytes = 1 << 20 // 1 MiB
	maxJWKSBodyBytes      = 1 << 20 // 1 MiB
)

// OIDCOptions configures an OIDCVerifier.
type OIDCOptions struct {
	// IssuerURL identifies the token issuer and is checked against the
	// token's iss claim, byte for byte, exactly as given here — including a
	// trailing slash if the issuer has one (Auth0's issuer strings do; a
	// verifier that normalizes the slash away would then never match a real
	// Auth0 token). Required.
	IssuerURL string
	// Audience identifies this application to the issuer and is checked
	// against the token's aud claim. Required.
	Audience string
	// JWKSURL overrides the key-set URL. If empty, NewOIDCVerifier fetches
	// IssuerURL + "/.well-known/openid-configuration" and reads jwks_uri from
	// it (standard OIDC discovery). The discovered jwks_uri must share the
	// issuer's origin (scheme and host); NewOIDCVerifier refuses one that
	// does not, since nothing about OIDC discovery otherwise stops a
	// document from pointing an implementation at an arbitrary host.
	JWKSURL string
	// HTTPClient makes the discovery and key-set requests. Defaults to a
	// client with a DefaultHTTPTimeout timeout — never http.DefaultClient,
	// which has none and would let an unresponsive issuer hang a
	// verification indefinitely.
	HTTPClient *http.Client
	// CacheTTL is how long a fetched key set is used before being refetched
	// on a schedule. Defaults to DefaultJWKSCacheTTL. A key set is also
	// refetched, at most once per minRefetchInterval, when a token names a
	// key id (kid) not in the current set, which is what makes key rotation
	// work without waiting out the TTL.
	CacheTTL time.Duration
	// AllowInsecureIssuer permits a plain-http IssuerURL. Left false (the
	// default), NewOIDCVerifier refuses one: an http issuer means both
	// discovery and every subsequent JWKS fetch travel unencrypted, so
	// whoever can see or alter that traffic can hand this verifier
	// attacker-controlled keys. Set it only for a local development issuer
	// that never runs over the network.
	AllowInsecureIssuer bool
}

// OIDCVerifier is an IDTokenVerifier for a generic OpenID Connect issuer:
// Auth0, Google, Apple, Cognito, Clerk, or any other standard OIDC provider.
// It performs discovery, fetches and caches the issuer's JSON Web Key Set,
// and verifies a token's signature, issuer, audience and expiry. Only RS256
// is accepted, and expiry is required — a token missing "exp" is rejected
// rather than treated as never expiring.
//
// # Presets
//
// IssuerURL and Audience for common providers:
//
//	Provider   IssuerURL                              Audience
//	Auth0      https://<tenant>.auth0.com/            the application Client ID (ID tokens; the API identifier is for access tokens)
//	Google     https://accounts.google.com             your OAuth client ID
//	Apple      https://appleid.apple.com               your Services ID (client ID)
//	Cognito    https://cognito-idp.<region>.amazonaws.com/<pool id>   your app client ID
//	Clerk      https://<your-subdomain>.clerk.accounts.dev  your Clerk instance's audience, as configured there
//
// Each of these publishes /.well-known/openid-configuration at its
// IssuerURL, so no vendor SDK is needed. Auth0's issuer string includes the
// trailing slash shown above; pass it exactly like that, since it is what
// Auth0 puts in a token's iss claim.
//
// # Revocation
//
// OIDC has no standard equivalent of Firebase's revocation check.
// VerifyIDTokenCheckRevoked performs the same check as VerifyIDToken; a
// caller that needs revocation must use the issuer's own introspection
// endpoint, if it has one, which is outside this type's scope.
type OIDCVerifier struct {
	issuer     string
	audience   string
	jwksURL    string
	httpClient *http.Client
	cacheTTL   time.Duration

	group singleflight.Group

	mu                 sync.Mutex
	keys               map[string]*rsa.PublicKey
	fetchedAt          time.Time
	nextAllowedRefetch time.Time
}

// NewOIDCVerifier runs discovery (unless opts.JWKSURL is set) and fetches the
// initial key set.
func NewOIDCVerifier(ctx context.Context, opts OIDCOptions) (*OIDCVerifier, error) {
	if opts.IssuerURL == "" {
		return nil, errors.New("auth: OIDCOptions.IssuerURL is required")
	}
	if opts.Audience == "" {
		return nil, errors.New("auth: OIDCOptions.Audience is required")
	}
	if !opts.AllowInsecureIssuer && !strings.HasPrefix(opts.IssuerURL, "https://") {
		return nil, errors.New("auth: OIDCOptions.IssuerURL must use https (set AllowInsecureIssuer for local development only)")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	cacheTTL := opts.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = DefaultJWKSCacheTTL
	}

	v := &OIDCVerifier{
		// issuer is kept EXACTLY as given, trailing slash included: it is
		// compared byte for byte against a token's iss claim, and Auth0's
		// issuer strings (and therefore its tokens' iss claims) include one.
		issuer:     opts.IssuerURL,
		audience:   opts.Audience,
		jwksURL:    opts.JWKSURL,
		httpClient: client,
		cacheTTL:   cacheTTL,
	}

	if v.jwksURL == "" {
		jwksURL, err := discoverJWKSURL(ctx, client, opts.IssuerURL)
		if err != nil {
			return nil, err
		}
		v.jwksURL = jwksURL
	}

	if err := v.refreshKeys(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

type discoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// discoverJWKSURL fetches issuerURL's discovery document and returns its
// jwks_uri, having checked two things a hostile or misconfigured document
// could otherwise abuse:
//
//   - The document's own "issuer" field must equal issuerURL exactly (RFC
//     8414 §3.3). Skipping this lets a document fetched from the right place
//     still claim to speak for a different issuer.
//   - jwks_uri must share issuerURL's origin. Nothing in the discovery
//     protocol otherwise stops a document from pointing this package at an
//     arbitrary host, and this package would then fetch and trust whatever
//     keys that host serves.
func discoverJWKSURL(ctx context.Context, client *http.Client, issuerURL string) (string, error) {
	discoveryURL := strings.TrimSuffix(issuerURL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", fmt.Errorf("auth: build discovery request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: fetch discovery document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth: discovery document request returned status %d", resp.StatusCode)
	}

	var doc discoveryDocument
	body := io.LimitReader(resp.Body, maxDiscoveryBodyBytes)
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return "", fmt.Errorf("auth: decode discovery document: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("auth: discovery document has no jwks_uri")
	}
	if doc.Issuer != issuerURL {
		return "", fmt.Errorf("auth: discovery document issuer %q does not match configured issuer %q", doc.Issuer, issuerURL)
	}
	if err := sameOrigin(issuerURL, doc.JWKSURI); err != nil {
		return "", fmt.Errorf("auth: discovery document jwks_uri rejected: %w", err)
	}
	return doc.JWKSURI, nil
}

// sameOrigin reports an error unless candidate shares issuer's scheme and
// host.
func sameOrigin(issuer, candidate string) error {
	issuerURL, err := url.Parse(issuer)
	if err != nil {
		return fmt.Errorf("parse issuer URL: %w", err)
	}
	candidateURL, err := url.Parse(candidate)
	if err != nil {
		return fmt.Errorf("parse jwks_uri: %w", err)
	}
	if !strings.EqualFold(issuerURL.Scheme, candidateURL.Scheme) || !strings.EqualFold(issuerURL.Host, candidateURL.Host) {
		return fmt.Errorf("jwks_uri %q is not on the issuer's origin (%s://%s)", candidate, issuerURL.Scheme, issuerURL.Host)
	}
	return nil
}

type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jsonWebKeySet struct {
	Keys []jsonWebKey `json:"keys"`
}

// refreshKeys fetches the current key set and replaces the cache. Concurrent
// callers collapse into one HTTP request via the singleflight group.
func (v *OIDCVerifier) refreshKeys(ctx context.Context) error {
	_, err, _ := v.group.Do("refresh", func() (any, error) {
		return nil, v.fetchAndStoreKeys(ctx)
	})
	return err
}

func (v *OIDCVerifier) fetchAndStoreKeys(ctx context.Context) error {
	// v.jwksURL is either a caller-configured static value or the jwks_uri
	// discovery returned, and discoverJWKSURL already required that value to
	// share the configured issuer's origin (sameOrigin, checked once, at
	// construction) before ever storing it here — so despite v.jwksURL being
	// a variable, this request cannot be redirected to an arbitrary host by
	// anything an untrusted party controls. Confirmed by removing either the
	// discovery issuer check or the sameOrigin check and watching the
	// corresponding discovery test fail.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil) //nolint:gosec // jwksURL is same-origin-checked before it is ever stored, see above
	if err != nil {
		return fmt.Errorf("auth: build JWKS request: %w", err)
	}
	resp, err := v.httpClient.Do(req) //nolint:gosec // same request built above; see its comment
	if err != nil {
		return fmt.Errorf("auth: fetch JWKS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: JWKS request returned status %d", resp.StatusCode)
	}

	var set jsonWebKeySet
	body := io.LimitReader(resp.Body, maxJWKSBodyBytes)
	if err := json.NewDecoder(body).Decode(&set); err != nil {
		return fmt.Errorf("auth: decode JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}

	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.mu.Unlock()
	return nil
}

func parseRSAPublicKey(nEncoded, eEncoded string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nEncoded)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eEncoded)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(new(big.Int).SetBytes(eBytes).Int64()),
	}, nil
}

// keyForKID returns the key for kid, fetching a fresh key set if kid is not
// in the current one (key rotation) or the current one is past its TTL.
//
// An on-miss refetch is itself rate-limited to at most once per
// minRefetchInterval: without that, a caller presenting tokens with random or
// otherwise unrecognized kid values would force one HTTP request per token,
// against either the issuer's JWKS endpoint or this process's own outbound
// connection budget.
func (v *OIDCVerifier) keyForKID(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	key, ok := v.keys[kid]
	stale := time.Since(v.fetchedAt) > v.cacheTTL
	refetchAllowed := time.Now().After(v.nextAllowedRefetch)
	v.mu.Unlock()

	if ok && !stale {
		return key, nil
	}
	if !refetchAllowed {
		if ok {
			return key, nil
		}
		return nil, fmt.Errorf("auth: no key found for kid %q (refetch rate-limited)", kid)
	}

	v.mu.Lock()
	v.nextAllowedRefetch = time.Now().Add(minRefetchInterval)
	v.mu.Unlock()

	if err := v.refreshKeys(ctx); err != nil {
		if ok {
			// The old key still works even though a refresh failed; do not
			// fail a request over a transient JWKS-endpoint problem.
			return key, nil
		}
		return nil, err
	}

	v.mu.Lock()
	key, ok = v.keys[kid]
	v.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("auth: no key found for kid %q", kid)
	}
	return key, nil
}

// VerifyIDToken verifies signature, issuer, audience and expiry. A token with
// no exp claim is rejected: jwt.WithExpirationRequired treats a missing exp
// as an error rather than as a token that never expires.
func (v *OIDCVerifier) VerifyIDToken(ctx context.Context, tokenString string) (Identity, error) {
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token has no kid header")
		}
		return v.keyForKID(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !token.Valid {
		return Identity{}, fmt.Errorf("auth: verify oidc token: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return Identity{}, errors.New("auth: verify oidc token: invalid claims")
	}
	if err := requireExactAudience(claims, v.audience); err != nil {
		return Identity{}, fmt.Errorf("auth: verify oidc token: %w", err)
	}
	return identityFromOIDCClaims(claims), nil
}

// requireExactAudience rejects a token whose aud claim names more than the
// one audience this verifier expects. jwt.WithAudience (already applied in
// VerifyIDToken) only checks that our audience is CONTAINED in the claim,
// which is correct per RFC 7519 but accepts a token that was ALSO issued
// for a second, unrelated audience — one that could replay or relay the
// token outside this verifier's trust boundary. This is a stricter check on
// top of that: aud must be exactly this verifier's audience, alone.
func requireExactAudience(claims jwt.MapClaims, audience string) error {
	switch aud := claims["aud"].(type) {
	case string:
		if aud != audience {
			return fmt.Errorf("aud claim %q does not exactly match the expected audience", aud)
		}
		return nil
	case []any:
		if len(aud) != 1 {
			return fmt.Errorf("aud claim names %d audiences, want exactly one", len(aud))
		}
		if s, ok := aud[0].(string); ok && s == audience {
			return nil
		}
		return errors.New("aud claim does not exactly match the expected audience")
	default:
		return errors.New("aud claim has an unexpected shape")
	}
}

// VerifyIDTokenCheckRevoked performs the same verification as VerifyIDToken.
// See the OIDCVerifier doc comment for why: generic OIDC has no standard
// revocation check.
func (v *OIDCVerifier) VerifyIDTokenCheckRevoked(ctx context.Context, tokenString string) (Identity, error) {
	return v.VerifyIDToken(ctx, tokenString)
}

func identityFromOIDCClaims(claims jwt.MapClaims) Identity {
	id := Identity{}
	if sub, ok := claims["sub"].(string); ok {
		id.UID = sub
	}
	if email, ok := claims["email"].(string); ok {
		id.Email = email
	}
	if name, ok := claims["name"].(string); ok {
		id.Name = name
	}
	if verified, ok := claims["email_verified"].(bool); ok {
		id.EmailVerified = verified
	}
	return id
}
