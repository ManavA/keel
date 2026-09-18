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
	srv := templateServer(t, map[string]bool{"welcome": true, "password-reset": true})
	defer srv.Close()

	err := CheckTemplates(context.Background(), []string{"welcome", "password-reset"}, CheckTemplatesOptions{
		ServerToken: "tok",
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

	err := CheckTemplates(context.Background(), []string{"welcome", "password-reset", "goodbye"}, CheckTemplatesOptions{
		ServerToken: "tok",
		BaseURL:     srv.URL,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "password-reset")
	assert.Contains(t, err.Error(), "goodbye")
	assert.NotContains(t, err.Error(), "welcome",
		"a present template must not be listed among the missing ones")
}

func TestCheckTemplates_EmptyRequiredListPasses(t *testing.T) {
	srv := templateServer(t, map[string]bool{})
	defer srv.Close()

	err := CheckTemplates(context.Background(), nil, CheckTemplatesOptions{ServerToken: "tok", BaseURL: srv.URL})
	assert.NoError(t, err)
}

func TestCheckTemplates_TransportFailureIsAnError(t *testing.T) {
	// A server that is not listening at all: the HTTP call itself fails,
	// which must surface as an error rather than being read as "not found".
	err := CheckTemplates(context.Background(), []string{"welcome"}, CheckTemplatesOptions{
		ServerToken: "tok",
		BaseURL:     "http://127.0.0.1:1", // nothing listens on port 1
	})
	require.Error(t, err)
}

func TestCheckTemplates_NonJSONBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	err := CheckTemplates(context.Background(), []string{"welcome"}, CheckTemplatesOptions{
		ServerToken: "tok",
		BaseURL:     srv.URL,
	})
	require.Error(t, err)
}
