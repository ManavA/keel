package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouterMountsLoginAndRefresh(t *testing.T) {
	s := newTestService(t)
	require.NoError(t, s.users.Create(context.Background(), &Admin{
		Email: "ops@example.com", PasswordHash: mustHashAdmin(t, "hunter2hunter2"),
	}))
	router := s.Router()

	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(LoginRequest{Email: "ops@example.com", Password: "hunter2hunter2"}))
	loginReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/login", &buf)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, loginReq)
	assert.Equal(t, http.StatusOK, rec.Code)

	refreshReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/refresh", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, refreshReq)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "refresh must require a token")
}

func TestRouterAppliesCORSOriginWhenConfigured(t *testing.T) {
	s, err := NewService(Options{
		Users:      NewMemoryAdminStore(),
		Secret:     testSecret,
		CORSOrigin: "https://admin.example.com",
	})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/login", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, "https://admin.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestRouterRejectsUnconfiguredOrigin(t *testing.T) {
	s, err := NewService(Options{
		Users:      NewMemoryAdminStore(),
		Secret:     testSecret,
		CORSOrigin: "https://admin.example.com",
	})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/login", nil)
	req.Header.Set("Origin", "https://not-the-admin-console.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestRouterCallerCanMountProtectedRoutes(t *testing.T) {
	s := newTestService(t)
	router := s.Router()
	router.Group(func(r chi.Router) {
		r.Use(s.RequireAdmin)
		r.Get("/protected", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/protected", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
