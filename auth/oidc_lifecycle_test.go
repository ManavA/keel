package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTokenEndpoint is a minimal OAuth2 token endpoint for refresh tests. It
// accepts the one current refresh token, answers with a fresh access token
// and a fresh refresh token, and rejects anything else with invalid_grant —
// the way a provider with rotation enabled answers a replayed token.
type fakeTokenEndpoint struct {
	mu         sync.Mutex
	server     *httptest.Server
	current    string
	seen       int
	grantTypes []string
	clientIDs  []string
}

func newFakeTokenEndpoint(t *testing.T) *fakeTokenEndpoint {
	t.Helper()
	f := &fakeTokenEndpoint{current: "rt-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.grantTypes = append(f.grantTypes, r.Form.Get("grant_type"))
		f.clientIDs = append(f.clientIDs, r.Form.Get("client_id"))
		if r.Form.Get("grant_type") != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unsupported_grant_type"})
			return
		}
		if r.Form.Get("refresh_token") != f.current {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "invalid_grant",
				"error_description": "refresh token already used or revoked",
			})
			return
		}
		f.seen++
		next := fmt.Sprintf("rt-%d", f.seen+1)
		f.current = next
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("access-%d", f.seen),
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": next,
			"id_token":      fmt.Sprintf("id-%d", f.seen),
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func newTestOIDCRefresher(t *testing.T, endpoint *fakeTokenEndpoint, onReuse func(context.Context, string)) *OIDCRefresher {
	t.Helper()
	r, err := NewOIDCRefresher(OIDCRefreshOptions{
		TokenURL:              endpoint.server.URL + "/oauth/token",
		ClientID:              "test-client",
		AllowInsecureTokenURL: true, // httptest.NewServer is plain HTTP
		OnReuse:               onReuse,
	})
	require.NoError(t, err)
	return r
}

// TestOIDCRefresherRotatesRefreshToken is the acceptance test for keel issue
// #24: a refresh rotation against a fake token endpoint where the old refresh
// token is single-use and its reuse is reported as an error.
func TestOIDCRefresherRotatesRefreshToken(t *testing.T) {
	endpoint := newFakeTokenEndpoint(t)
	var reuse []string
	refresher := newTestOIDCRefresher(t, endpoint, func(_ context.Context, rt string) {
		reuse = append(reuse, rt)
	})
	ctx := context.Background()

	first, err := refresher.Refresh(ctx, "rt-1")
	require.NoError(t, err)
	require.NotEmpty(t, first.RefreshToken)
	require.NotEqual(t, "rt-1", first.RefreshToken, "a rotation must issue a new refresh token")
	assert.Equal(t, "Bearer", first.TokenType)
	assert.NotEmpty(t, first.AccessToken)

	// The old token is single-use: presenting it again is reuse.
	_, err = refresher.Refresh(ctx, "rt-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefreshTokenReused)
	assert.Equal(t, []string{"rt-1"}, reuse, "the reuse hook must fire with the replayed token")

	// The rotated token still works, chaining the rotation forward.
	second, err := refresher.Refresh(ctx, first.RefreshToken)
	require.NoError(t, err)
	assert.NotEqual(t, first.RefreshToken, second.RefreshToken)

	// The client spoke plain OAuth2 refresh to the endpoint.
	for _, grantType := range endpoint.grantTypes {
		assert.Equal(t, "refresh_token", grantType)
	}
	for _, clientID := range endpoint.clientIDs {
		assert.Equal(t, "test-client", clientID)
	}
}

func TestOIDCRefresherSurfacesNonReuseErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "temporarily_unavailable"})
	}))
	defer server.Close()

	refresher, err := NewOIDCRefresher(OIDCRefreshOptions{
		TokenURL:              server.URL,
		ClientID:              "test-client",
		AllowInsecureTokenURL: true,
	})
	require.NoError(t, err)

	_, err = refresher.Refresh(context.Background(), "rt-1")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRefreshTokenReused, "only invalid_grant means reuse")
}

func TestCodeChallengeS256MatchesRFC7636Vector(t *testing.T) {
	// RFC 7636 Appendix B test vector.
	assert.Equal(t,
		"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
		CodeChallengeS256("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"))
}

func TestGenerateCodeVerifierShape(t *testing.T) {
	a, err := GenerateCodeVerifier()
	require.NoError(t, err)
	b, err := GenerateCodeVerifier()
	require.NoError(t, err)
	for _, v := range []string{a, b} {
		assert.GreaterOrEqual(t, len(v), 43)
		assert.LessOrEqual(t, len(v), 128)
		assert.Regexp(t, `^[A-Za-z0-9\-._~]+$`, v)
	}
	assert.NotEqual(t, a, b, "two verifiers must differ")
	assert.Len(t, CodeChallengeS256(a), 43, "a SHA-256 challenge encodes to 43 unpadded characters")
}
