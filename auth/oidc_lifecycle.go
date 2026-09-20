package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxTokenResponseBodyBytes bounds how much of a token endpoint response this
// package reads, so a compromised or misconfigured endpoint cannot exhaust
// memory with an oversized response.
const maxTokenResponseBodyBytes = 1 << 20 // 1 MiB

// ErrRefreshTokenReused reports that the token endpoint answered invalid_grant
// to a refresh request. With rotation enabled that answer means the refresh
// token was already rotated out or revoked, so the grant may be compromised:
// revoke the local session and require sign-in again. A plain expired token
// can also produce invalid_grant, and this package cannot tell the two apart,
// so it always reports the stricter case.
var ErrRefreshTokenReused = errors.New("auth: refresh token reuse detected")

// GenerateCodeVerifier returns a random PKCE code_verifier (RFC 7636 §4.1):
// 32 random bytes in unpadded base64url, which is 43 characters — inside the
// required 43-128 range and using only unreserved characters. The caller keeps
// the verifier and sends CodeChallengeS256 of it with the authorize request;
// on redemption the provider checks the verifier against the challenge.
func GenerateCodeVerifier() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("auth: generate code verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// CodeChallengeS256 returns the S256 PKCE code_challenge for a verifier: the
// SHA-256 of the verifier in unpadded base64url. S256 is the only method this
// package produces; plain exists only for clients that cannot hash, which a
// Go caller never is.
func CodeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// GeneratePKCEPair returns a fresh verifier and its S256 challenge together,
// the two values a public client needs to start an authorization-code flow.
func GeneratePKCEPair() (verifier, challenge string, err error) {
	verifier, err = GenerateCodeVerifier()
	if err != nil {
		return "", "", err
	}
	return verifier, CodeChallengeS256(verifier), nil
}

// OIDCRefreshOptions configures an OIDCRefresher.
type OIDCRefreshOptions struct {
	// TokenURL is the provider's token endpoint. Required.
	TokenURL string
	// ClientID is this application's OAuth client ID. Required.
	ClientID string
	// ClientSecret authenticates a confidential client (a server holding a
	// secret). Public clients using PKCE leave it empty. Apple has no static
	// secret: pass the ES256 client_secret JWT there instead.
	ClientSecret string
	// HTTPClient makes the token requests. Defaults to a client with a
	// DefaultHTTPTimeout timeout — never http.DefaultClient, which has none
	// and would let an unresponsive endpoint hang a refresh indefinitely.
	HTTPClient *http.Client
	// OnReuse runs when the endpoint answers invalid_grant, which under
	// rotation means the refresh token was replayed after rotation. It runs
	// synchronously before Refresh returns the ErrRefreshTokenReused error.
	OnReuse func(ctx context.Context, refreshToken string)
	// AllowInsecureTokenURL permits a plain-http TokenURL. Left false (the
	// default), NewOIDCRefresher refuses one: refresh requests carry bearer
	// credentials, which must never travel unencrypted. Set it only for a
	// local development issuer that never runs over the network.
	AllowInsecureTokenURL bool
}

// OIDCTokens is the token set a refresh request returns.
type OIDCTokens struct {
	// AccessToken is the new access token. Always present on success.
	AccessToken string
	// IDToken is the new ID token, when the provider issues one on refresh.
	IDToken string
	// RefreshToken is the replacement refresh token under rotation. Empty
	// when the provider reuses long-lived refresh tokens instead of rotating.
	RefreshToken string
	// TokenType is the access token's type, normally "Bearer".
	TokenType string
	// ExpiresIn is how long the access token lasts, from the response's
	// expires_in seconds. Zero when the provider omits it.
	ExpiresIn time.Duration
}

// OIDCRefresher refreshes OAuth2 access tokens at a provider's token
// endpoint, for sign-ins that came through OIDCVerifier. Providers that
// rotate refresh tokens hand back a new one per call and reject the old one
// with invalid_grant; this type surfaces that rejection as
// ErrRefreshTokenReused and runs OnReuse, so the caller can revoke the
// session. Providers with long-lived reusable tokens simply return no new
// refresh token, and the caller keeps using the old one.
//
// # Provider flows
//
// Public clients (single-page apps, mobile apps) cannot keep a secret, so
// they redeem authorization codes with PKCE (GeneratePKCEPair) instead of a
// secret. Confidential clients (a server) redeem with ClientSecret. Typical
// setup per provider:
//
//	Provider   Code redemption              Refresh tokens
//	Auth0      PKCE for public clients,     rotation with reuse detection is a
//	         secret for confidential ones   per-application toggle
//	Google     PKCE for installed apps,     long-lived and reusable; no rotation
//	         secret for web apps
//	Cognito    PKCE for clients without a   long-lived bearer tokens, reusable
//	         secret, secret otherwise       until expiry or revocation
//	Apple      a client_secret JWT          reusable until expiry or revocation
//	         (ES256, team key) in place
//	         of a static secret
//	Clerk      handled by Clerk's SDKs;     use Clerk's session helpers rather
//	         not this helper                than calling the token endpoint
//
// Provider behavior changes; confirm the row above in the provider's current
// docs before relying on it.
type OIDCRefresher struct {
	tokenURL     string
	clientID     string
	clientSecret string
	httpClient   *http.Client
	onReuse      func(ctx context.Context, refreshToken string)
}

// NewOIDCRefresher validates the options and returns a refresher.
func NewOIDCRefresher(opts OIDCRefreshOptions) (*OIDCRefresher, error) {
	if opts.TokenURL == "" {
		return nil, errors.New("auth: OIDCRefreshOptions.TokenURL is required")
	}
	if opts.ClientID == "" {
		return nil, errors.New("auth: OIDCRefreshOptions.ClientID is required")
	}
	if !opts.AllowInsecureTokenURL && !strings.HasPrefix(opts.TokenURL, "https://") {
		return nil, errors.New("auth: OIDCRefreshOptions.TokenURL must use https (set AllowInsecureTokenURL for local development only)")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &OIDCRefresher{
		tokenURL:     opts.TokenURL,
		clientID:     opts.ClientID,
		clientSecret: opts.ClientSecret,
		httpClient:   client,
		onReuse:      opts.OnReuse,
	}, nil
}

type oidcTokenResponse struct {
	AccessToken      string `json:"access_token"`
	IDToken          string `json:"id_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Refresh trades a refresh token for a new token set. Under rotation the
// returned RefreshToken replaces the one passed in, which becomes single-use;
// presenting it again makes the endpoint answer invalid_grant, and Refresh
// returns ErrRefreshTokenReused after running OnReuse.
func (r *OIDCRefresher) Refresh(ctx context.Context, refreshToken string) (OIDCTokens, error) {
	if refreshToken == "" {
		return OIDCTokens{}, errors.New("auth: refresh token is required")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {r.clientID},
	}
	if r.clientSecret != "" {
		form.Set("client_secret", r.clientSecret)
	}
	// r.tokenURL is caller configuration validated at construction, not
	// anything an untrusted party controls.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.tokenURL, strings.NewReader(form.Encode())) //nolint:gosec // tokenURL is caller configuration, see above
	if err != nil {
		return OIDCTokens{}, fmt.Errorf("auth: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := r.httpClient.Do(req) //nolint:gosec // same request built above; see its comment
	if err != nil {
		return OIDCTokens{}, fmt.Errorf("auth: refresh token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload oidcTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponseBodyBytes)).Decode(&payload); err != nil {
		return OIDCTokens{}, fmt.Errorf("auth: decode token response: %w", err)
	}
	if payload.Error != "" {
		if payload.Error == "invalid_grant" {
			if r.onReuse != nil {
				r.onReuse(ctx, refreshToken)
			}
			msg := "auth: refresh token reuse detected"
			if payload.ErrorDescription != "" {
				msg += ": " + payload.ErrorDescription
			}
			return OIDCTokens{}, fmt.Errorf("%s: %w", msg, ErrRefreshTokenReused)
		}
		if payload.ErrorDescription != "" {
			return OIDCTokens{}, fmt.Errorf("auth: refresh token request failed (%s): %s", payload.Error, payload.ErrorDescription)
		}
		return OIDCTokens{}, fmt.Errorf("auth: refresh token request failed (%s)", payload.Error)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return OIDCTokens{}, fmt.Errorf("auth: refresh token request returned status %d", resp.StatusCode)
	}
	if payload.AccessToken == "" {
		return OIDCTokens{}, errors.New("auth: token response has no access token")
	}
	return OIDCTokens{
		AccessToken:  payload.AccessToken,
		IDToken:      payload.IDToken,
		RefreshToken: payload.RefreshToken,
		TokenType:    payload.TokenType,
		ExpiresIn:    time.Duration(payload.ExpiresIn) * time.Second,
	}, nil
}
