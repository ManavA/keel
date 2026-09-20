package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The auth shells are form-encoded pages over the same auth service the JSON
// API uses: a signup through the form sets a session cookie and lands on the
// verify-sent page, a login through the form lands on the dashboard, and the
// cookie opens the same rows the JSON token does. These tests pin that
// contract, including that flashes are fixed strings and input echoes escaped.

// noRedirectClient answers with the redirect itself, so a test can assert on
// the 303 and its Location rather than following it.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// postAuthForm sends a form-encoded POST with optional cookies. hx marks the
// request as coming from htmx, which asks for the block rather than the page.
func postAuthForm(t *testing.T, client *http.Client, ts *testService, path string, values url.Values, hx bool, cookies ...*http.Cookie) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.srv.URL+path, strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

// getAuthPage sends a GET with optional cookies. hx marks the request as
// coming from htmx, which asks for the block rather than the page.
func getAuthPage(t *testing.T, client *http.Client, ts *testService, path string, hx bool, cookies ...*http.Cookie) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.srv.URL+path, nil)
	require.NoError(t, err)
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

// authCookie returns the session cookie from a response, failing when the
// handler did not set one.
func authCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("response set no %q cookie", sessionCookieName)
	return nil
}

// loginAuthCookie signs up (or logs in, when the account exists) through the
// form handlers, returning the session cookie the browser would hold.
func loginAuthCookie(t *testing.T, ts *testService, email string) *http.Cookie {
	t.Helper()
	client := noRedirectClient()
	values := url.Values{"email": {email}, "password": {testPassword}}
	resp, _ := postAuthForm(t, client, ts, "/signup", values, false)
	if resp.StatusCode != http.StatusSeeOther {
		resp, _ = postAuthForm(t, client, ts, "/login", values, false)
	}
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	return authCookie(t, resp)
}

func TestAuthShellsRender(t *testing.T) {
	ts := newTestServer(t)

	for _, tt := range []struct {
		path   string
		marker string
	}{
		{"/login", "<h1>Log in</h1>"},
		{"/signup", "<h1>Sign up</h1>"},
		{"/verify-sent", "Check your inbox"},
		{"/reset", "<h1>Reset your password</h1>"},
		{"/health", "Service health"},
	} {
		resp, raw := getAuthPage(t, http.DefaultClient, ts, tt.path, false)
		require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s: %s", tt.path, raw)
		assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
		assert.Contains(t, string(raw), tt.marker)
		assert.Contains(t, string(raw), `data-theme="landing"`)
	}
}

func TestHealthShellReportsChecks(t *testing.T) {
	ts := newTestServer(t)

	resp, raw := getAuthPage(t, http.DefaultClient, ts, "/health", false)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "database")
	assert.Contains(t, string(raw), "search")
	assert.Contains(t, string(raw), "ok")
}

func TestFormSignupSetsCookieAndShowsVerifySent(t *testing.T) {
	ts := newTestServer(t)
	client := noRedirectClient()

	resp, _ := postAuthForm(t, client, ts, "/signup",
		url.Values{"email": {"browser@example.com"}, "password": {testPassword}}, false)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/verify-sent", resp.Header.Get("Location"))

	cookie := authCookie(t, resp)
	assert.True(t, cookie.HttpOnly, "the session cookie must not reach JavaScript")
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)

	resp, raw := getAuthPage(t, http.DefaultClient, ts, "/verify-sent", false, cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "Check your inbox")

	// The JSON signup flow is untouched: it still answers with a token.
	session := signup(t, ts, "api@example.com")
	assert.NotEmpty(t, session.Token)
}

func TestFormSignupConflictRerendersShell(t *testing.T) {
	ts := newTestServer(t)
	signup(t, ts, "taken@example.com")
	client := noRedirectClient()

	resp, raw := postAuthForm(t, client, ts, "/signup",
		url.Values{"email": {"taken@example.com"}, "password": {testPassword}}, false)
	require.Equal(t, http.StatusConflict, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "an account with this email already exists")
	assert.Contains(t, string(raw), "<h1>Sign up</h1>")
	assert.Empty(t, resp.Cookies(), "a failed signup must not set a session")
}

func TestFormLoginSetsCookieAndOpensDashboard(t *testing.T) {
	ts := newTestServer(t)
	signup(t, ts, "returning@example.com")
	client := noRedirectClient()

	resp, _ := postAuthForm(t, client, ts, "/login",
		url.Values{"email": {"returning@example.com"}, "password": {testPassword}}, false)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/app", resp.Header.Get("Location"))
	cookie := authCookie(t, resp)

	resp, raw := getAuthPage(t, http.DefaultClient, ts, "/app", false, cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), `data-theme="dashboard"`)
	assert.Contains(t, string(raw), "Your notes")
}

func TestDashboardWithoutACookieRedirectsToLogin(t *testing.T) {
	ts := newTestServer(t)
	client := noRedirectClient()

	resp, _ := getAuthPage(t, client, ts, "/app", false)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))

	// A forged cookie is the same redirect, not a different one.
	forged := &http.Cookie{Name: sessionCookieName, Value: "not-a-real-token"}
	resp, _ = getAuthPage(t, client, ts, "/app", false, forged)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
}

func TestFormLoginFailureRerendersShell(t *testing.T) {
	ts := newTestServer(t)
	client := noRedirectClient()

	// A valid email with the wrong password: the fixed 401 message.
	resp, raw := postAuthForm(t, client, ts, "/login",
		url.Values{"email": {"nobody@example.com"}, "password": {testPassword}}, false)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "invalid credentials")
	assert.Contains(t, string(raw), "<h1>Log in</h1>")
	assert.Empty(t, resp.Cookies(), "a failed login must not set a session")

	// Markup in the email echoes back escaped, never as markup. The JSON login
	// has no email-format check — an unknown address is 401, not 400 — and
	// the form agrees with it.
	resp, raw = postAuthForm(t, client, ts, "/login",
		url.Values{"email": {`"><script>alert(1)</script>@example.com`}, "password": {testPassword}}, false)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "invalid credentials")
	assert.NotContains(t, string(raw), "<script>alert(1)</script>")
	assert.Contains(t, string(raw), "&lt;script&gt;")
}

func TestCookieOpensTheSameRowsAsTheToken(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "both@example.com").Token
	cookie := loginAuthCookie(t, ts, "both@example.com")

	// Written through the JSON API, read through the cookie fragment.
	_, raw := postNote(t, ts, token, `{"title":"JSON note","body":"From the API"}`)
	require.Contains(t, string(raw), "JSON note")

	resp, raw := getAuthPage(t, http.DefaultClient, ts, "/ui/notes", true, cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "JSON note")
	assert.NotContains(t, string(raw), "<html",
		"a fragment is its block rendered standalone, not a shell")

	// Written through the cookie fragment, read through the JSON API.
	resp, raw = postAuthForm(t, noRedirectClient(), ts, "/ui/notes",
		url.Values{"title": {"Cookie note"}}, true, cookie)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "Cookie note")

	resp, raw = get(t, ts, "/api/notes", token)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "Cookie note")

	// And the dashboard shell lists both.
	resp, raw = getAuthPage(t, http.DefaultClient, ts, "/app", false, cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "JSON note")
	assert.Contains(t, string(raw), "Cookie note")
}

func TestCookieFragmentsRequireAuth(t *testing.T) {
	ts := newTestServer(t)

	resp, _ := getAuthPage(t, http.DefaultClient, ts, "/ui/notes", true)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	forged := &http.Cookie{Name: sessionCookieName, Value: "not-a-real-token"}
	resp, _ = getAuthPage(t, http.DefaultClient, ts, "/ui/notes", true, forged)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestCookieFragmentCreateValidationError(t *testing.T) {
	ts := newTestServer(t)
	cookie := loginAuthCookie(t, ts, "particular-cookie@example.com")

	// The same rule as the other two surfaces: 422 with the form re-rendered
	// around the flash, and nothing stored.
	resp, raw := postAuthForm(t, noRedirectClient(), ts, "/ui/notes",
		url.Values{"title": {"   "}}, true, cookie)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "A title is required.")
	assert.Contains(t, string(raw), "<form")
}

func TestCookieFragmentsAreScopedToOwner(t *testing.T) {
	ts := newTestServer(t)
	tokenA := signup(t, ts, "alice-cookie@example.com").Token
	cookieA := loginAuthCookie(t, ts, "alice-cookie@example.com")
	cookieB := loginAuthCookie(t, ts, "bob-cookie@example.com")

	resp, raw := postAuthForm(t, noRedirectClient(), ts, "/ui/notes",
		url.Values{"title": {"Alice cookie note"}}, true, cookieA)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	resp, raw = getAuthPage(t, http.DefaultClient, ts, "/ui/notes", true, cookieB)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.NotContains(t, string(raw), "Alice cookie note")

	resp, raw = get(t, ts, "/api/notes", tokenA)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "Alice cookie note")
}

func TestMarkupIsEscapedInCookieSurfaces(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "markup-cookie@example.com").Token
	cookie := loginAuthCookie(t, ts, "markup-cookie@example.com")

	_, raw := postNote(t, ts, token, `{"title":"<script>alert(1)</script>","body":"<b>bold</b>"}`)
	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, "<script>alert(1)</script>", created.Title,
		"the JSON API carries the title verbatim; escaping happens at render")

	for _, tt := range []struct {
		path string
		hx   bool
	}{
		{"/ui/notes", true},
		{"/app", false},
	} {
		resp, raw := getAuthPage(t, http.DefaultClient, ts, tt.path, tt.hx, cookie)
		require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s: %s", tt.path, raw)
		assert.NotContains(t, string(raw), "<script>alert(1)</script>", tt.path)
		assert.Contains(t, string(raw), "&lt;script&gt;", tt.path)
	}
}

func TestMailedVerifyLinkRendersLoginShell(t *testing.T) {
	ts := newTestServer(t)
	client := noRedirectClient()

	resp, _ := postAuthForm(t, client, ts, "/signup",
		url.Values{"email": {"verifyme@example.com"}, "password": {testPassword}}, false)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	token := lastLinkToken(t, ts)
	resp, raw := getAuthPage(t, http.DefaultClient, ts, "/verify-email?token="+token, false)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "Email verified")
	assert.Contains(t, string(raw), "<h1>Log in</h1>")

	resp, raw = getAuthPage(t, http.DefaultClient, ts, "/verify-email?token=bogus", false)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "invalid or has expired")
}

func TestResetFlowThroughForms(t *testing.T) {
	ts := newTestServer(t)
	signup(t, ts, "forgetful@example.com")
	client := noRedirectClient()

	// The request step always answers the same 200, like its JSON twin.
	resp, raw := postAuthForm(t, client, ts, "/reset",
		url.Values{"email": {"forgetful@example.com"}}, false)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "If an account exists for that email")

	resetToken := lastLinkToken(t, ts)
	resp, raw = getAuthPage(t, http.DefaultClient, ts, "/reset?token="+resetToken, false)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "New password")

	resp, raw = postAuthForm(t, client, ts, "/reset/confirm",
		url.Values{"token": {resetToken}, "new_password": {"newpassword123"}}, false)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "Your password has been reset")

	// The new password works through the form login.
	resp, _ = postAuthForm(t, client, ts, "/login",
		url.Values{"email": {"forgetful@example.com"}, "password": {"newpassword123"}}, false)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// A spent token fails the same way the JSON endpoint fails it.
	resp, raw = postAuthForm(t, client, ts, "/reset/confirm",
		url.Values{"token": {resetToken}, "new_password": {"anotherpassword123"}}, false)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
}

func TestLogoutClearsTheCookie(t *testing.T) {
	ts := newTestServer(t)
	cookie := loginAuthCookie(t, ts, "leaver@example.com")
	client := noRedirectClient()

	resp, raw := getAuthPage(t, http.DefaultClient, ts, "/app", false, cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.srv.URL+"/logout", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/", resp.Header.Get("Location"))

	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && (c.MaxAge < 0 || c.Value == "") {
			cleared = true
		}
	}
	assert.True(t, cleared, "logout must clear the session cookie")

	// The revoked token no longer opens the dashboard: the logout ended the
	// session server-side, not just in the browser.
	resp, _ = getAuthPage(t, client, ts, "/app", false, cookie)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
}

func TestSessionCookieSecureFollowsSiteURL(t *testing.T) {
	https := (&API{siteURL: "https://example.com"}).sessionCookie("token")
	assert.True(t, https.HttpOnly, "the session cookie must not reach JavaScript")
	assert.Equal(t, http.SameSiteLaxMode, https.SameSite)
	assert.Equal(t, "/", https.Path)
	assert.True(t, https.Secure, "an https site that set a non-Secure cookie would send the token in the clear")

	plain := (&API{siteURL: "http://localhost:8080"}).sessionCookie("token")
	assert.False(t, plain.Secure, "an http site that set Secure would never send the cookie at all")
}
