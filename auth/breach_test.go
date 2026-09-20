package auth

import (
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hibpSuffix returns the k-anonymity suffix (everything after the first 5 hex
// characters of the uppercased SHA-1) for a password, the same split a real
// range endpoint answers with.
func hibpSuffix(password string) (prefix, suffix string) {
	sum := sha1.Sum([]byte(password))
	digest := strings.ToUpper(hex.EncodeToString(sum[:]))
	return digest[:5], digest[5:]
}

// fakeBreachServer answers the k-anonymity range protocol: GET /{prefix}
// returns "SUFFIX:COUNT" lines. breachedSuffixes are reported with a nonzero
// count; everything else gets an unrelated suffix. It records every request
// path so tests can assert what (little) the client transmitted.
func fakeBreachServer(breachedSuffixes map[string]bool, paths *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		lines := []string{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA:2"}
		for s := range breachedSuffixes {
			lines = append(lines, s+":5")
		}
		_, _ = w.Write([]byte(strings.Join(lines, "\r\n")))
	}))
}

func TestHIBPBreachCheckerDetectsMatch(t *testing.T) {
	const candidate = "correct horse battery staple"
	_, suffix := hibpSuffix(candidate)
	var paths []string
	srv := fakeBreachServer(map[string]bool{suffix: true}, &paths)
	defer srv.Close()

	checker := &HIBPBreachChecker{Endpoint: srv.URL}
	breached, err := checker.Breached(t.Context(), candidate)
	require.NoError(t, err)
	assert.True(t, breached)

	// k-anonymity: the full hash — and the password — must never leave the
	// process. Only the 5-character prefix crosses the network.
	require.Len(t, paths, 1)
	prefix, _ := hibpSuffix(candidate)
	assert.Equal(t, "/"+prefix, paths[0])
	assert.NotContains(t, paths[0], suffix)
}

func TestHIBPBreachCheckerNoMatch(t *testing.T) {
	const candidate = "a fresh password nobody breached 12345"
	var paths []string
	srv := fakeBreachServer(map[string]bool{}, &paths)
	defer srv.Close()

	checker := &HIBPBreachChecker{Endpoint: srv.URL}
	breached, err := checker.Breached(t.Context(), candidate)
	require.NoError(t, err)
	assert.False(t, breached)
}

func TestSignupRejectsBreachedPasswordWhenCheckerEnabled(t *testing.T) {
	const candidate = "hunter2hunter2hunter2"
	_, suffix := hibpSuffix(candidate)
	var paths []string
	srv := fakeBreachServer(map[string]bool{suffix: true}, &paths)
	defer srv.Close()

	h, err := NewService(Options{
		Users:         NewMemoryUserStore(),
		Emailer:       &recordingEmailer{},
		SiteURL:       "https://example.com",
		BreachChecker: &HIBPBreachChecker{Endpoint: srv.URL},
	})
	require.NoError(t, err)

	rec := doJSON(t, http.HandlerFunc(h.Signup), "/signup", SignupRequest{
		Email: "breached@example.com", Password: candidate,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	require.NotEmpty(t, paths, "the enabled checker must consult the breach endpoint")
}

func TestSignupAcceptsBreachedPasswordWhenCheckerDisabled(t *testing.T) {
	// Same password the enabled test rejects: with no checker configured the
	// existing length rules are the whole policy, so signup succeeds — and
	// with no endpoint configured, no network call is possible.
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.Signup), "/signup", SignupRequest{
		Email: "breached@example.com", Password: "hunter2hunter2hunter2",
	})
	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestResetPasswordRejectsBreachedPasswordWhenCheckerEnabled(t *testing.T) {
	const candidate = "hunter2hunter2hunter2"
	_, suffix := hibpSuffix(candidate)
	var paths []string
	srv := fakeBreachServer(map[string]bool{suffix: true}, &paths)
	defer srv.Close()

	h, err := NewService(Options{
		Users:         NewMemoryUserStore(),
		Emailer:       &recordingEmailer{},
		SiteURL:       "https://example.com",
		BreachChecker: &HIBPBreachChecker{Endpoint: srv.URL},
	})
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, h.users.Create(ctx, &User{Email: "reset@example.com", PasswordHash: "x"}))
	user, err := h.users.GetByEmail(ctx, "reset@example.com")
	require.NoError(t, err)
	raw, err := GenerateVerificationToken()
	require.NoError(t, err)
	require.NoError(t, h.passwordResets.Create(ctx, user.ID, HashVerificationToken(raw), time.Now().Add(time.Hour)))

	rec := doJSON(t, http.HandlerFunc(h.ResetPassword), "/reset-password", ResetPasswordRequest{
		Token: raw, NewPassword: candidate,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	require.NotEmpty(t, paths, "the enabled checker must consult the breach endpoint")
}
