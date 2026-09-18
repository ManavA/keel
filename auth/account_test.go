package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteAccountRequiresAuth(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, h.RequireAuth(http.HandlerFunc(h.DeleteAccount)), "/delete-account", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestDeleteAccountRemovesTheCallersOwnAccount(t *testing.T) {
	h := newTestService(t)
	u := &User{Email: "deleteme@example.com", PasswordHash: "x"}
	require.NoError(t, h.users.Create(context.Background(), u))
	token, err := h.session.Issue(context.Background(), u.ID)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/delete-account", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.DeleteAccount)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	_, err = h.users.GetByID(context.Background(), u.ID)
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestDeleteAccountHasNoPathParameter(t *testing.T) {
	// DeleteAccount must take the id ONLY from the authenticated session, so
	// there is no request field that could name a different account. This
	// test pins that: a request body naming another user's id is ignored.
	h := newTestService(t)
	victim := &User{Email: "victim@example.com"}
	require.NoError(t, h.users.Create(context.Background(), victim))
	caller := &User{Email: "caller@example.com"}
	require.NoError(t, h.users.Create(context.Background(), caller))
	token, err := h.session.Issue(context.Background(), caller.ID)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/delete-account", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.DeleteAccount)).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	_, err = h.users.GetByID(context.Background(), caller.ID)
	assert.ErrorIs(t, err, ErrUserNotFound)
	_, err = h.users.GetByID(context.Background(), victim.ID)
	assert.NoError(t, err, "an unrelated account must be untouched")
}
