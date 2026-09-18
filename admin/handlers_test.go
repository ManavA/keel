package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func mustHashAdmin(t *testing.T, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	require.NoError(t, err)
	return string(hash)
}

func TestLoginSuccess(t *testing.T) {
	s := newTestService(t)
	require.NoError(t, s.users.Create(context.Background(), &Admin{
		Email: "ops@example.com", PasswordHash: mustHashAdmin(t, "hunter2hunter2"), Role: "admin",
	}))

	rec := doJSON(t, http.HandlerFunc(s.Login), "/login", LoginRequest{Email: "ops@example.com", Password: "hunter2hunter2"})
	require.Equal(t, http.StatusOK, rec.Code)

	var resp SessionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Token)
	assert.Equal(t, "ops@example.com", resp.Admin.Email)
}

func TestLoginWrongPasswordAndUnknownEmailAreIndistinguishable(t *testing.T) {
	s := newTestService(t)
	require.NoError(t, s.users.Create(context.Background(), &Admin{
		Email: "known@example.com", PasswordHash: mustHashAdmin(t, "realpassword1"),
	}))

	wrongPassword := doJSON(t, http.HandlerFunc(s.Login), "/login", LoginRequest{Email: "known@example.com", Password: "wrongpassword"})
	unknownEmail := doJSON(t, http.HandlerFunc(s.Login), "/login", LoginRequest{Email: "nosuchadmin@example.com", Password: "wrongpassword"})

	assert.Equal(t, http.StatusUnauthorized, wrongPassword.Code)
	assert.Equal(t, http.StatusUnauthorized, unknownEmail.Code)
	assert.Equal(t, wrongPassword.Body.String(), unknownEmail.Body.String())
}

func TestLoginUpdatesLastLogin(t *testing.T) {
	s := newTestService(t)
	a := &Admin{Email: "ops2@example.com", PasswordHash: mustHashAdmin(t, "hunter2hunter2")}
	require.NoError(t, s.users.Create(context.Background(), a))

	rec := doJSON(t, http.HandlerFunc(s.Login), "/login", LoginRequest{Email: "ops2@example.com", Password: "hunter2hunter2"})
	require.Equal(t, http.StatusOK, rec.Code)

	got, err := s.users.GetByID(context.Background(), a.ID)
	require.NoError(t, err)
	assert.False(t, got.LastLoginAt.IsZero())
}

func TestRefreshRequiresAuth(t *testing.T) {
	s := newTestService(t)
	rec := doJSON(t, s.RequireAdmin(http.HandlerFunc(s.Refresh)), "/refresh", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRefreshIssuesNewToken(t *testing.T) {
	s := newTestService(t)
	a := &Admin{Email: "refresh@example.com", PasswordHash: "x"}
	require.NoError(t, s.users.Create(context.Background(), a))
	token, err := s.session.IssueToken(a.ID)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.RequireAdmin(http.HandlerFunc(s.Refresh)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

// failingAdminStore wraps a real AdminStore and makes GetByEmail return an
// arbitrary error instead of ErrAdminNotFound, standing in for a database
// outage rather than an unregistered address.
type failingAdminStore struct {
	AdminStore
}

func (s *failingAdminStore) GetByEmail(context.Context, string) (*Admin, error) {
	return nil, errors.New("connection refused")
}

func TestLoginReturns503OnAStoreFailureNotA401(t *testing.T) {
	s := newTestService(t)
	s.users = &failingAdminStore{AdminStore: s.users}

	rec := doJSON(t, http.HandlerFunc(s.Login), "/login", LoginRequest{Email: "anyone@example.com", Password: "whatever1"})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
