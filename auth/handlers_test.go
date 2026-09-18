package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doJSON posts a JSON-encoded body to handler and returns the recorded
// response. Every caller in this package tests a POST endpoint, so the
// method is fixed rather than taken as a parameter nothing varies.
func doJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, &buf)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestSignupSuccess(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.Signup), "/signup", SignupRequest{
		Email: "new@example.com", Password: "hunter2hunter2",
	})

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp SessionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Token)
	assert.Equal(t, "new@example.com", resp.User.Email)
	assert.True(t, resp.VerificationSent)
}

func TestSignupDuplicateEmailIs409(t *testing.T) {
	h := newTestService(t)
	require.NoError(t, h.users.Create(context.Background(), &User{Email: "dup@example.com", PasswordHash: "x"}))

	rec := doJSON(t, http.HandlerFunc(h.Signup), "/signup", SignupRequest{
		Email: "dup@example.com", Password: "hunter2hunter2",
	})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestSignupRejectsBadEmail(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.Signup), "/signup", SignupRequest{
		Email: "not-an-email", Password: "hunter2hunter2",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSignupRejectsShortPassword(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.Signup), "/signup", SignupRequest{
		Email: "short@example.com", Password: "short",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestSignupRejectsPasswordOverBcryptsLimit pins maxPasswordLength at the
// handler boundary: bcrypt reads only the first 72 bytes of its input and
// silently ignores the rest, so without this check two passwords differing
// only after byte 72 would hash identically — a signup that reports success
// for either one is not a good place to discover that.
func TestSignupRejectsPasswordOverBcryptsLimit(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.Signup), "/signup", SignupRequest{
		Email: "toolong@example.com", Password: strings.Repeat("a", maxPasswordLength+1),
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestValidatePasswordLength pins the exact boundary, not just "some long
// password is rejected": exactly maxPasswordLength bytes must pass, and
// maxPasswordLength+1 must not, so a future off-by-one either direction
// fails here instead of only showing up as a subtly weaker password.
func TestValidatePasswordLength(t *testing.T) {
	tests := []struct {
		name    string
		length  int
		wantErr bool
	}{
		{"one under the minimum", minPasswordLength - 1, true},
		{"exactly the minimum", minPasswordLength, false},
		{"exactly the maximum", maxPasswordLength, false},
		{"one over the maximum", maxPasswordLength + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePasswordLength(strings.Repeat("a", tt.length))
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLoginSuccess(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.Signup), "/signup", SignupRequest{
		Email: "login@example.com", Password: "hunter2hunter2",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = doJSON(t, http.HandlerFunc(h.Login), "/login", LoginRequest{
		Email: "login@example.com", Password: "hunter2hunter2",
	})
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestLoginWrongPasswordAndUnknownEmailAreIndistinguishable(t *testing.T) {
	h := newTestService(t)
	require.NoError(t, h.users.Create(context.Background(), func() *User {
		hash, err := HashPassword("realpassword1")
		require.NoError(t, err)
		return &User{Email: "known@example.com", PasswordHash: hash}
	}()))

	wrongPassword := doJSON(t, http.HandlerFunc(h.Login), "/login", LoginRequest{
		Email: "known@example.com", Password: "wrongpassword",
	})
	unknownEmail := doJSON(t, http.HandlerFunc(h.Login), "/login", LoginRequest{
		Email: "nosuchuser@example.com", Password: "wrongpassword",
	})

	assert.Equal(t, http.StatusUnauthorized, wrongPassword.Code)
	assert.Equal(t, http.StatusUnauthorized, unknownEmail.Code)
	assert.Equal(t, wrongPassword.Body.String(), unknownEmail.Body.String(),
		"the two failure modes must be indistinguishable to the caller")
}

func TestRefreshRequiresAuth(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, h.RequireAuth(http.HandlerFunc(h.Refresh)), "/refresh", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRefreshIssuesNewToken(t *testing.T) {
	h := newTestService(t)
	u := &User{Email: "refresh@example.com", PasswordHash: "x"}
	require.NoError(t, h.users.Create(context.Background(), u))
	token, err := h.session.Issue(context.Background(), u.ID)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.Refresh)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp SessionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Token)
}

func TestExchangeIDTokenWithoutVerifierConfiguredIs503(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.ExchangeIDToken), "/exchange", ExchangeIDTokenRequest{IDToken: "whatever"})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestExchangeIDTokenCreatesUserOnFirstSignIn(t *testing.T) {
	h := newTestService(t)
	require.NoError(t, h.SetIDTokenVerifier(&fakeIDTokenVerifier{
		identity: Identity{UID: "ext-uid-1", Email: "sso@example.com", EmailVerified: true},
	}))

	rec := doJSON(t, http.HandlerFunc(h.ExchangeIDToken), "/exchange", ExchangeIDTokenRequest{IDToken: "valid"})
	require.Equal(t, http.StatusOK, rec.Code)

	var resp SessionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "sso@example.com", resp.User.Email)
	assert.True(t, resp.User.EmailVerified)

	_, err := h.users.GetByExternalUID(context.Background(), "ext-uid-1")
	assert.NoError(t, err)
}

func TestExchangeIDTokenReturnsExistingUser(t *testing.T) {
	h := newTestService(t)
	require.NoError(t, h.users.Create(context.Background(), &User{Email: "existing@example.com", ExternalUID: "ext-uid-2"}))
	require.NoError(t, h.SetIDTokenVerifier(&fakeIDTokenVerifier{
		identity: Identity{UID: "ext-uid-2", Email: "existing@example.com"},
	}))

	rec := doJSON(t, http.HandlerFunc(h.ExchangeIDToken), "/exchange", ExchangeIDTokenRequest{IDToken: "valid"})
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestExchangeIDTokenRejectsInvalidToken(t *testing.T) {
	h := newTestService(t)
	require.NoError(t, h.SetIDTokenVerifier(&fakeIDTokenVerifier{err: assert.AnError}))

	rec := doJSON(t, http.HandlerFunc(h.ExchangeIDToken), "/exchange", ExchangeIDTokenRequest{IDToken: "bad"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestVerifyEmailFlow(t *testing.T) {
	h := newTestService(t)
	u := &User{Email: "verifyme@example.com", PasswordHash: "x"}
	require.NoError(t, h.users.Create(context.Background(), u))

	raw, err := GenerateVerificationToken()
	require.NoError(t, err)
	require.NoError(t, h.verifications.Create(context.Background(), u.ID, HashVerificationToken(raw), time.Now().Add(time.Hour)))

	rec := doJSON(t, http.HandlerFunc(h.VerifyEmail), "/verify-email", VerifyEmailRequest{Token: raw})
	require.Equal(t, http.StatusOK, rec.Code)

	got, err := h.users.GetByID(context.Background(), u.ID)
	require.NoError(t, err)
	assert.True(t, got.EmailVerified)
}

func TestVerifyEmailRejectsUnknownToken(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.VerifyEmail), "/verify-email", VerifyEmailRequest{Token: "never-issued"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestResendVerificationAlreadyVerified(t *testing.T) {
	h := newTestService(t)
	u := &User{Email: "already@example.com", PasswordHash: "x", EmailVerified: true}
	require.NoError(t, h.users.Create(context.Background(), u))
	token, err := h.session.Issue(context.Background(), u.ID)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/resend-verification", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.ResendVerification)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]bool
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body["email_verified"])
	assert.False(t, body["sent"])
}

func TestResendVerificationSendsWhenUnverified(t *testing.T) {
	h := newTestService(t)
	u := &User{Email: "unverified@example.com", PasswordHash: "x"}
	require.NoError(t, h.users.Create(context.Background(), u))
	token, err := h.session.Issue(context.Background(), u.ID)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/resend-verification", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.ResendVerification)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	emailer := h.emailer.(*recordingEmailer)
	assert.Contains(t, emailer.sentTo, "unverified@example.com")
}

// failingUserStore wraps a real UserStore and makes GetByEmail return an
// arbitrary error instead of ErrUserNotFound, standing in for a database
// outage rather than an unregistered address.
type failingUserStore struct {
	UserStore
}

func (s *failingUserStore) GetByEmail(context.Context, string) (*User, error) {
	return nil, errors.New("connection refused")
}

// TestLoginReturns503OnAStoreFailureNotA401 is the regression test for a
// login handler that cannot tell "no such user" from "the database is
// down": both used to answer 401, which hides an outage behind what reads
// as a wrong password and tells an operator debugging one that every
// attempt was simply invalid.
func TestLoginReturns503OnAStoreFailureNotA401(t *testing.T) {
	h := newTestService(t)
	h.users = &failingUserStore{UserStore: h.users}

	rec := doJSON(t, http.HandlerFunc(h.Login), "/login", LoginRequest{Email: "anyone@example.com", Password: "whatever1"})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
