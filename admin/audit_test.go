package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuditTrailRecordsAdminAction is the acceptance check for the admin
// audit trail: an admin action performed through the router leaves exactly
// one audit row carrying actor, action, target, and outcome.
func TestAuditTrailRecordsAdminAction(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	require.NoError(t, s.users.Create(ctx, &Admin{
		Email: "ops@example.com", PasswordHash: mustHashAdmin(t, "hunter2hunter2"),
	}))

	router := s.Router()
	router.Group(func(r chi.Router) {
		r.Use(s.RequireAdmin)
		r.Use(s.Audit("user.disable", func(r *http.Request) string {
			return r.URL.Query().Get("id")
		}))
		r.Post("/users/disable", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	})

	token := loginTestAdmin(t, router, "ops@example.com", "hunter2hunter2")

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/users/disable?id=user_42", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	entries := readAuditEntries(t, router, token)
	require.Len(t, entries, 1, "one admin action must leave exactly one audit row")
	assert.NotEmpty(t, entries[0].Actor)
	assert.Equal(t, "user.disable", entries[0].Action)
	assert.Equal(t, "user_42", entries[0].Target)
	assert.Equal(t, AuditOutcomeOK, entries[0].Outcome)
	assert.False(t, entries[0].CreatedAt.IsZero())
}

func TestAuditTrailRecordsFailedOutcome(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	require.NoError(t, s.users.Create(ctx, &Admin{
		Email: "ops@example.com", PasswordHash: mustHashAdmin(t, "hunter2hunter2"),
	}))

	router := s.Router()
	router.Group(func(r chi.Router) {
		r.Use(s.RequireAdmin)
		r.Use(s.Audit("user.disable", nil))
		r.Post("/users/disable", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
	})

	token := loginTestAdmin(t, router, "ops@example.com", "hunter2hunter2")

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/users/disable", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	entries := readAuditEntries(t, router, token)
	require.Len(t, entries, 1)
	assert.Equal(t, "user.disable", entries[0].Action)
	assert.Equal(t, AuditOutcomeError, entries[0].Outcome)
}

func TestAuditTrailRequiresAuth(t *testing.T) {
	s := newTestService(t)
	router := s.Router()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(
		context.Background(), http.MethodGet, "/audit", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func loginTestAdmin(t *testing.T, router http.Handler, email, password string) string {
	t.Helper()
	rec := doJSON(t, router, "/login", LoginRequest{Email: email, Password: password})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp SessionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token)
	return resp.Token
}

func readAuditEntries(t *testing.T, router http.Handler, token string) []AuditEntry {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/audit", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp AuditListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Entries
}
