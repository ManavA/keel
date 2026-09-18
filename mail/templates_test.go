package mail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// templateServer serves a fixed set of known aliases; anything else answers
// exactly the shape Postmark uses for "no such template": HTTP 200 with a
// nonzero ErrorCode in the body.
func templateServer(t *testing.T, known map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alias := r.URL.Path[len("/templates/"):]
		w.WriteHeader(http.StatusOK)
		if known[alias] {
			_ = json.NewEncoder(w).Encode(templateProbeResponse{})
		} else {
			_ = json.NewEncoder(w).Encode(templateProbeResponse{ErrorCode: 1101, Message: "template alias not found"})
		}
	}))
}

func TestCheckTemplates_AllPresent(t *testing.T) {
	srv := templateServer(t, map[string]bool{"welcome": true, testPasswordResetAlias: true})
	defer srv.Close()

	err := CheckTemplates(context.Background(), []string{"welcome", testPasswordResetAlias}, CheckTemplatesOptions{
		ServerToken: testServerToken,
		BaseURL:     srv.URL,
	})
	assert.NoError(t, err)
}

// TestCheckTemplates_NamesEveryMissingAlias is the regression this function
// exists for: an account with a hole in its templates must fail loudly, at
// startup, naming exactly what is missing — not surface as a silent 1101
// the first time a real send needs that alias.
func TestCheckTemplates_NamesEveryMissingAlias(t *testing.T) {
	srv := templateServer(t, map[string]bool{"welcome": true})
	defer srv.Close()

	err := CheckTemplates(context.Background(), []string{"welcome", testPasswordResetAlias, "goodbye"}, CheckTemplatesOptions{
		ServerToken: testServerToken,
		BaseURL:     srv.URL,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), testPasswordResetAlias)
	assert.Contains(t, err.Error(), "goodbye")
	assert.NotContains(t, err.Error(), "welcome",
		"a present template must not be listed among the missing ones")
}

func TestCheckTemplates_EmptyRequiredListPasses(t *testing.T) {
	srv := templateServer(t, map[string]bool{})
	defer srv.Close()

	err := CheckTemplates(context.Background(), nil, CheckTemplatesOptions{ServerToken: testServerToken, BaseURL: srv.URL})
	assert.NoError(t, err)
}

func TestCheckTemplates_TransportFailureIsAnError(t *testing.T) {
	// A server that is not listening at all: the HTTP call itself fails,
	// which must surface as an error rather than being read as "not found".
	err := CheckTemplates(context.Background(), []string{"welcome"}, CheckTemplatesOptions{
		ServerToken: testServerToken,
		BaseURL:     "http://127.0.0.1:1", // nothing listens on port 1
	})
	require.Error(t, err)
}

// TestCheckTemplates_AuthFailureIsAnErrorNotMissing is the regression test
// for conflating "could not check" with "template missing": an invalid
// server token must not be reported the same way as a real missing
// template, or an operator ends up looking for a template that exists
// while the actual problem — the token — goes unreported.
func TestCheckTemplates_AuthFailureIsAnErrorNotMissing(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"401 unauthorized", http.StatusUnauthorized},
		{"403 forbidden", http.StatusForbidden},
		{"429 rate limited", http.StatusTooManyRequests},
		{"500 internal server error", http.StatusInternalServerError},
		{"503 service unavailable", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("server said no"))
			}))
			defer srv.Close()

			err := CheckTemplates(context.Background(), []string{"welcome"}, CheckTemplatesOptions{
				ServerToken: testServerToken,
				BaseURL:     srv.URL,
			})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "not found in this Postmark account",
				"a %d must not be reported using the same wording as a confirmed missing template", tt.status)
		})
	}
}

// TestCheckTemplates_404And422AreConfirmedMissing checks the two shapes
// Postmark's real API actually uses for "no such template": both must
// still be reported as missing, not as an inconclusive error.
func TestCheckTemplates_404And422AreConfirmedMissing(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"404 not found", http.StatusNotFound},
		{"422 unprocessable entity", http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(templateProbeResponse{ErrorCode: 1101, Message: "not found"})
			}))
			defer srv.Close()

			err := CheckTemplates(context.Background(), []string{"welcome"}, CheckTemplatesOptions{
				ServerToken: testServerToken,
				BaseURL:     srv.URL,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not found in this Postmark account")
		})
	}
}

func TestCheckTemplates_AliasIsURLEscaped(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(templateProbeResponse{})
	}))
	defer srv.Close()

	err := CheckTemplates(context.Background(), []string{"weird alias/with slash"}, CheckTemplatesOptions{
		ServerToken: testServerToken,
		BaseURL:     srv.URL,
	})
	require.NoError(t, err)
	assert.Equal(t, "/templates/weird%20alias%2Fwith%20slash", gotPath,
		"an alias containing '/' or ' ' must not change the request's path structure")
}

func TestCheckTemplates_NonJSONBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	err := CheckTemplates(context.Background(), []string{"welcome"}, CheckTemplatesOptions{
		ServerToken: testServerToken,
		BaseURL:     srv.URL,
	})
	require.Error(t, err)
}
