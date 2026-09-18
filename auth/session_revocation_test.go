package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemorySessionStoreRevoke(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySessionStore()
	token, err := store.Create(ctx, "user_1", time.Hour)
	require.NoError(t, err)

	require.NoError(t, store.Revoke(ctx, token))

	_, err = store.Validate(ctx, token)
	assert.ErrorIs(t, err, ErrSessionNotFound)
}

func TestMemorySessionStoreRevokeUnknownTokenIsNotAnError(t *testing.T) {
	store := NewMemorySessionStore()
	assert.NoError(t, store.Revoke(context.Background(), "never-issued"))
}

func TestMemorySessionStoreRevokeAllForUser(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySessionStore()
	tokenA, err := store.Create(ctx, "user_1", time.Hour)
	require.NoError(t, err)
	tokenB, err := store.Create(ctx, "user_1", time.Hour)
	require.NoError(t, err)
	otherUserToken, err := store.Create(ctx, "user_2", time.Hour)
	require.NoError(t, err)

	require.NoError(t, store.RevokeAllForUser(ctx, "user_1"))

	_, err = store.Validate(ctx, tokenA)
	assert.ErrorIs(t, err, ErrSessionNotFound)
	_, err = store.Validate(ctx, tokenB)
	assert.ErrorIs(t, err, ErrSessionNotFound)

	stillValid, err := store.Validate(ctx, otherUserToken)
	require.NoError(t, err, "a different user's session must be untouched")
	assert.Equal(t, "user_2", stillValid)
}

func TestJWTSessionBackendRevokeIsANoOp(t *testing.T) {
	backend := &jwtSessionBackend{issuer: newSessionIssuer("test-secret-placeholder-16bytes", time.Hour)}
	token, err := backend.Issue(context.Background(), "user_1")
	require.NoError(t, err)

	require.NoError(t, backend.Revoke(context.Background(), token))
	require.NoError(t, backend.RevokeAllForUser(context.Background(), "user_1"))

	// The whole point of documenting this as a no-op: the token still works.
	subject, err := backend.Validate(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "user_1", subject)

	assert.False(t, backend.Revocable())
}

// TestLogoutReportsFalseUnderJWTMode is the regression test for a Logout
// response that reads the same whether a token actually stopped working or
// not: under SessionJWT, Logout still answers 200 (the client-side half of
// logout — discarding the token — is genuine), but sessions_revoked must be
// false, since the token itself keeps validating until it expires.
func TestLogoutReportsFalseUnderJWTMode(t *testing.T) {
	h, err := NewService(Options{
		SessionMode:              SessionJWT,
		AllowUnrevocableSessions: true,
		Secret:                   "test-secret-placeholder-16bytes",
	})
	require.NoError(t, err)
	u := &User{Email: "jwt-logout@example.com"}
	require.NoError(t, h.users.Create(context.Background(), u))
	token, err := h.session.Issue(context.Background(), u.ID)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.Logout)).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]bool
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.False(t, body["sessions_revoked"])

	// And, unlike opaque mode, the token genuinely still works.
	subject, err := h.session.Validate(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, u.ID, subject)
}

func TestLogoutRevokesTheCallersSessionUnderOpaqueMode(t *testing.T) {
	h := newTestService(t) // opaque by default
	u := &User{Email: "logout@example.com", PasswordHash: "x"}
	require.NoError(t, h.users.Create(context.Background(), u))
	token, err := h.session.Issue(context.Background(), u.ID)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.Logout)).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]bool
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body["sessions_revoked"], "opaque sessions must report a real revocation, not a bare success")

	// The same token must no longer authenticate anything.
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/refresh", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.Refresh)).ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusUnauthorized, rec2.Code)
}

func TestLogoutRequiresAuth(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, h.RequireAuth(http.HandlerFunc(h.Logout)), "/logout", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestResetPasswordRevokesAStolenSessionToken is the regression test for the
// vulnerability ResetPassword's revoke call exists to close: a session token
// obtained before the reset (the scenario a reset is meant to recover from)
// must stop working the moment the account holder proves control via the
// reset token — not remain valid until it happens to expire.
func TestResetPasswordRevokesAStolenSessionToken(t *testing.T) {
	h := newTestService(t)
	u := &User{Email: "stolen-token@example.com", PasswordHash: mustHash(t, "oldpassword1")}
	require.NoError(t, h.users.Create(context.Background(), u))

	stolenToken, err := h.session.Issue(context.Background(), u.ID)
	require.NoError(t, err)

	raw, err := GenerateVerificationToken()
	require.NoError(t, err)
	require.NoError(t, h.passwordResets.Create(context.Background(), u.ID, HashVerificationToken(raw), time.Now().Add(time.Hour)))

	rec := doJSON(t, http.HandlerFunc(h.ResetPassword), "/reset-password", ResetPasswordRequest{
		Token: raw, NewPassword: "newpassword1",
	})
	require.Equal(t, http.StatusOK, rec.Code)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+stolenToken)
	respRec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.Refresh)).ServeHTTP(respRec, req)
	assert.Equal(t, http.StatusUnauthorized, respRec.Code, "a session token issued before the reset must not survive it")
}

func TestDeleteAccountRevokesRemainingSessions(t *testing.T) {
	h := newTestService(t)
	u := &User{Email: "delete-sessions@example.com", PasswordHash: "x"}
	require.NoError(t, h.users.Create(context.Background(), u))

	tokenA, err := h.session.Issue(context.Background(), u.ID)
	require.NoError(t, err)
	tokenB, err := h.session.Issue(context.Background(), u.ID)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/delete-account", nil)
	req.Header.Set("Authorization", "Bearer "+tokenA)
	rec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.DeleteAccount)).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	_, err = h.session.Validate(context.Background(), tokenB)
	assert.Error(t, err, "a second session for the deleted account must also be revoked")
}
