package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The admin browser UI is form-encoded pages over the same Service the JSON
// API uses: a login through the form lands on the users list, and the cookie
// opens the same rows the JSON token does. These tests pin that contract,
// including that admin-only data never renders without a valid admin session.

const uiBasePath = "/admin/ui"

func newUITestRouter(t *testing.T) (*Service, chi.Router) {
	t.Helper()
	s := newTestService(t)
	r := chi.NewRouter()
	s.MountUI(r, uiBasePath)
	return s, r
}

// testAdminPassword is the one password every test account uses. Unused
// flexibility here would be a lint finding, not coverage.
const testAdminPassword = "hunter2hunter2"

func seedUITestAdmin(t *testing.T, s *Service, email string) {
	t.Helper()
	require.NoError(t, s.users.Create(context.Background(), &Admin{
		Email: email, Name: "Ops", Role: "admin", PasswordHash: mustHashAdmin(t, testAdminPassword),
	}))
}

func uiLogin(t *testing.T, r http.Handler, email string) *http.Cookie {
	t.Helper()
	rec := doUIForm(t, r, uiBasePath+"/login", url.Values{
		"email":    {email},
		"password": {testAdminPassword},
	})
	require.Equal(t, http.StatusSeeOther, rec.Code, "setup login failed: %s", rec.Body.String())
	for _, c := range rec.Result().Cookies() {
		if c.Name == AdminSessionCookieName {
			return c
		}
	}
	t.Fatalf("form login set no %q cookie", AdminSessionCookieName)
	return nil
}

// doUIForm always posts: every UI write in these tests is a form POST.
func doUIForm(t *testing.T, r http.Handler, path string, values url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, body)
	if values != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func doUIGet(t *testing.T, r http.Handler, path string, hx bool, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func uiSessionCookie(token string) *http.Cookie {
	return &http.Cookie{Name: AdminSessionCookieName, Value: token, Path: "/"}
}

func TestAdminUILoginPageRenders(t *testing.T) {
	_, r := newUITestRouter(t)

	rec := doUIGet(t, r, uiBasePath+"/login", false)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, rec.Body.String(), "<form")
	assert.Contains(t, rec.Body.String(), `name="password"`)
}

func TestAdminUILoginSuccessSetsCookieAndRedirects(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")

	rec := doUIForm(t, r, uiBasePath+"/login", url.Values{
		"email":    {"ops@example.com"},
		"password": {testAdminPassword},
	})
	require.Equal(t, http.StatusSeeOther, rec.Code, "a form login lands on the users list: %s", rec.Body.String())
	assert.Equal(t, uiBasePath+"/users", rec.Header().Get("Location"))

	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == AdminSessionCookieName {
			session = c
		}
	}
	require.NotNil(t, session, "a form login must set the session cookie")
	assert.True(t, session.HttpOnly)

	users := doUIGet(t, r, uiBasePath+"/users", false, session)
	require.Equal(t, http.StatusOK, users.Code, "the cookie opens the users list: %s", users.Body.String())
	assert.Contains(t, users.Body.String(), "ops@example.com")
}

func TestAdminUILoginFailureReRendersWithoutCookie(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")

	rec := doUIForm(t, r, uiBasePath+"/login", url.Values{
		"email":    {"ops@example.com"},
		"password": {"wrongpassword"},
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid credentials")
	for _, c := range rec.Result().Cookies() {
		assert.NotEqual(t, AdminSessionCookieName, c.Name, "a failed login must not set a session")
	}
}

func TestAdminUIUsersRequiresAdmin(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")

	rec := doUIGet(t, r, uiBasePath+"/users", false)
	require.Equal(t, http.StatusSeeOther, rec.Code, "an anonymous page load leaves for the login page")
	assert.Equal(t, uiBasePath+"/login", rec.Header().Get("Location"))
	assert.NotContains(t, rec.Body.String(), "ops@example.com", "admin-only data must not render without a session")
}

func TestAdminUIUsersRejectsForgedToken(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")

	forged := uiSessionCookie("not-a-valid-token")
	rec := doUIGet(t, r, uiBasePath+"/users", false, forged)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.NotContains(t, rec.Body.String(), "ops@example.com")
}

func TestAdminUIUsersRejectsEndUserToken(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")

	// An end-user session from the sibling auth package shares the shape but
	// not the audience; it must not open the admin console even when both
	// packages were misconfigured with the same secret.
	now := time.Now()
	authToken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user_1",
		"aud": "keel:auth",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	signed, err := authToken.SignedString([]byte(testSecret))
	require.NoError(t, err)

	rec := doUIGet(t, r, uiBasePath+"/users", false, uiSessionCookie(signed))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.NotContains(t, rec.Body.String(), "ops@example.com")
}

func TestAdminUIUsersListsAdminsWithoutSecrets(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")
	session := uiLogin(t, r, "ops@example.com")

	rec := doUIGet(t, r, uiBasePath+"/users", false, session)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "ops@example.com")
	assert.NotContains(t, body, "PasswordHash", "the users page must not render credential fields")
}

func TestAdminUIUsersFragmentForHtmx(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")
	session := uiLogin(t, r, "ops@example.com")

	rec := doUIGet(t, r, uiBasePath+"/users", true, session)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "ops@example.com")
	assert.NotContains(t, rec.Body.String(), "<html", "an htmx fragment is the rows, not the page")
}

func TestAdminUIRevokeSessionsEndsThatAdminsTokens(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "revoker@example.com")
	seedUITestAdmin(t, s, "target@example.com")
	revoker := uiLogin(t, r, "revoker@example.com")
	targetSession := uiLogin(t, r, "target@example.com")

	target, err := s.users.GetByEmail(context.Background(), "target@example.com")
	require.NoError(t, err)

	rec := doUIForm(t, r, uiBasePath+"/users/"+target.ID+"/revoke", nil, revoker)
	require.Equal(t, http.StatusSeeOther, rec.Code, "a form revoke lands back on the users list: %s", rec.Body.String())

	stale := doUIGet(t, r, uiBasePath+"/users", false, targetSession)
	assert.Equal(t, http.StatusSeeOther, stale.Code, "the revoked admin's token must stop opening the console")
	assert.Equal(t, uiBasePath+"/login", stale.Header().Get("Location"))

	fresh := doUIGet(t, r, uiBasePath+"/users", false, revoker)
	assert.Equal(t, http.StatusOK, fresh.Code, "revoking one admin must not end another's session")
}

func TestAdminUIRevokeRequiresAdmin(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "target@example.com")
	targetSession := uiLogin(t, r, "target@example.com")

	target, err := s.users.GetByEmail(context.Background(), "target@example.com")
	require.NoError(t, err)

	rec := doUIForm(t, r, uiBasePath+"/users/"+target.ID+"/revoke", nil)
	require.Equal(t, http.StatusSeeOther, rec.Code, "an anonymous revoke leaves for the login page")
	assert.Equal(t, uiBasePath+"/login", rec.Header().Get("Location"))

	still := doUIGet(t, r, uiBasePath+"/users", false, targetSession)
	assert.Equal(t, http.StatusOK, still.Code, "an anonymous revoke must not end the target's session")
}

func TestAdminUIRevokeMissingAdminIsNotFound(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")
	session := uiLogin(t, r, "ops@example.com")

	rec := doUIForm(t, r, uiBasePath+"/users/no-such-admin/revoke", nil, session)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminUIAuditRequiresAdmin(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")
	require.NoError(t, s.audit.Append(context.Background(), &AuditEntry{
		Actor: "admin_1", Action: "user.disable", Target: "user_42", Outcome: AuditOutcomeOK,
	}))

	rec := doUIGet(t, r, uiBasePath+"/audit", false)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, uiBasePath+"/login", rec.Header().Get("Location"))
	assert.NotContains(t, rec.Body.String(), "user.disable", "the trail must not render without a session")
}

func TestAdminUIAuditListsAndFilters(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")
	session := uiLogin(t, r, "ops@example.com")
	ctx := context.Background()
	require.NoError(t, s.audit.Append(ctx, &AuditEntry{Actor: "admin_1", Action: "user.disable", Target: "user_42", Outcome: AuditOutcomeOK}))
	require.NoError(t, s.audit.Append(ctx, &AuditEntry{Actor: "admin_2", Action: "user.enable", Target: "user_43", Outcome: AuditOutcomeError}))

	full := doUIGet(t, r, uiBasePath+"/audit", false, session)
	require.Equal(t, http.StatusOK, full.Code)
	assert.Contains(t, full.Body.String(), "user.disable")
	assert.Contains(t, full.Body.String(), "user.enable")

	filtered := doUIGet(t, r, uiBasePath+"/audit?action=user.disable", false, session)
	require.Equal(t, http.StatusOK, filtered.Code)
	assert.Contains(t, filtered.Body.String(), "user.disable")
	assert.NotContains(t, filtered.Body.String(), "user.enable", "the action filter must narrow the trail")
}

func TestAdminUIAuditFragmentForHtmx(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")
	session := uiLogin(t, r, "ops@example.com")
	require.NoError(t, s.audit.Append(context.Background(), &AuditEntry{Actor: "admin_1", Action: "user.disable", Outcome: AuditOutcomeOK}))

	rec := doUIGet(t, r, uiBasePath+"/audit", true, session)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "user.disable")
	assert.NotContains(t, rec.Body.String(), "<html", "an htmx fragment is the rows, not the page")
}

func TestAdminUILogoutClearsCookie(t *testing.T) {
	s, r := newUITestRouter(t)
	seedUITestAdmin(t, s, "ops@example.com")
	session := uiLogin(t, r, "ops@example.com")

	rec := doUIForm(t, r, uiBasePath+"/logout", nil, session)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, uiBasePath+"/login", rec.Header().Get("Location"))
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == AdminSessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	assert.True(t, cleared, "logout must clear the session cookie")
}
