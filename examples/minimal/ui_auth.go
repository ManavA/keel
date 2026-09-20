package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/auth"
	"github.com/ManavA/keel/httpx"
)

// sessionCookieName carries the session token for the browser UI. The JSON
// API keeps answering bearer tokens; the cookie is the same token in a jar
// the browser sends on its own, so pages and fragments work without script.
const sessionCookieName = "keel_session"

// authPageData is what the auth shells render. Flash is the one inline
// message and holds only fixed strings chosen by status code, never anything
// from the request; Email carries the caller's address back into the form,
// escaped by the template.
type authPageData struct {
	Title string
	Flash string
	Email string
	Token string
	Sent  bool
	// Checks backs the health shell: one line per dependency.
	Checks []healthStatus
}

type healthStatus struct {
	Name string
	OK   bool
}

// AuthUIRoutes mounts the browser auth shells and the cookie-authenticated UI
// beside the JSON API. The form handlers reuse the auth service's own JSON
// endpoints in-process (see callAuthJSON), never over HTTP to this process
// itself, so the rules, status codes and rate limit cannot drift between the
// two surfaces.
func (a *API) AuthUIRoutes(r chi.Router) {
	r.Get("/login", a.showLogin)
	r.Post("/login", a.doLogin)
	r.Get("/signup", a.showSignup)
	r.Post("/signup", a.doSignup)
	r.Get("/verify-sent", a.verifySent)
	// The mailed links point here: SiteURL plus /verify-email?token= and
	// /reset?token=, so a click from an inbox lands on a page, not a 404.
	r.Get("/verify-email", a.doVerifyEmail)
	r.Get("/reset", a.showReset)
	r.Post("/reset", a.doResetRequest)
	r.Post("/reset/confirm", a.doResetConfirm)
	r.Post("/logout", a.doLogout)
	r.Get("/health", a.healthPage)

	// The dashboard and its fragments run the unchanged RequireAuth behind a
	// middleware that copies the session cookie into Authorization, so a
	// browser session and a bearer token authenticate the same way. The
	// dashboard turns RequireAuth's 401 into a login redirect; the fragments
	// keep it, for htmx to handle.
	r.With(a.requirePageAuth).Get("/app", a.appPage)
	r.With(a.requirePageAuth).Get("/app/", a.appPage)
	r.Route("/ui", func(r chi.Router) {
		r.Use(a.cookieToAuth, a.auth.RequireAuth)
		r.Get("/notes", a.notesPage)
		r.Post("/notes", a.notesCreate)
		r.Delete("/notes/{id}", a.notesDelete)
	})
}

// cookieToAuth copies the session cookie into Authorization when no bearer
// token is present. RequireAuth runs unchanged behind it: a request carrying
// both stays a bearer request, and a forged cookie is the same 401 as a
// forged token.
func (a *API) cookieToAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
				r.Header.Set("Authorization", "Bearer "+c.Value)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requirePageAuth runs RequireAuth and turns its 401 into a redirect to the
// login page. Fragments keep the raw 401; a page that answered 401 would show
// a JSON error inside a browser tab, which helps nobody.
func (a *API) requirePageAuth(next http.Handler) http.Handler {
	// The cookie fills in Authorization first, so RequireAuth behind it sees
	// the browser session exactly as it sees a bearer token.
	authed := a.cookieToAuth(a.auth.RequireAuth(next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		authed.ServeHTTP(rec, r)
		if rec.Code == http.StatusUnauthorized {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		for k, vs := range rec.Header() {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

// callAuthJSON runs one auth JSON endpoint in-process against the same router
// mounted under /auth: the same handler, the same rate limiter, no network.
// The caller's RemoteAddr travels along, so the per-IP limiter still buckets
// the browser behind the request rather than every form submit together.
func (a *API) callAuthJSON(r *http.Request, method, path string, body any, token string) (int, []byte) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return http.StatusInternalServerError, nil
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = r.RemoteAddr
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.authRoutes.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// sessionCookie wraps a session token for the browser. Secure follows SiteURL
// rather than a flag: an https site that set a non-Secure cookie would send
// the token in the clear, and an http site (local development) that set
// Secure would never send it at all.
func (a *API) sessionCookie(token string) *http.Cookie {
	secure := strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.siteURL)), "https://")
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(a.sessionCookieTTL().Seconds()),
	}
}

// clearSessionCookie ends the browser half of a session: an expired cookie
// the browser drops.
func clearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// authFlash maps a delegated status to a fixed message. The 401 and 409 texts
// match the auth package's own generic bodies, so the form and the JSON API
// say the same thing for the same outcome; nothing from the request or the
// delegated body ever reaches the page.
func authFlash(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid credentials"
	case http.StatusConflict:
		return "an account with this email already exists"
	case http.StatusTooManyRequests:
		return "too many requests"
	case http.StatusBadRequest:
		return "That request was not valid."
	default:
		return "Something went wrong."
	}
}

func (a *API) showLogin(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, r, a.tmpl, "page-login", authPageData{Title: "Log in"})
}

func (a *API) doLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		renderTemplateStatus(w, r, a.tmpl, "page-login",
			authPageData{Title: "Log in", Flash: authFlash(http.StatusBadRequest)}, http.StatusBadRequest)
		return
	}
	email := r.PostFormValue("email")

	status, raw := a.callAuthJSON(r, http.MethodPost, "/login", map[string]string{
		"email":    email,
		"password": r.PostFormValue("password"),
	}, "")
	if status != http.StatusOK {
		renderTemplateStatus(w, r, a.tmpl, "page-login",
			authPageData{Title: "Log in", Flash: authFlash(status), Email: email}, status)
		return
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &session); err != nil || session.Token == "" {
		httpx.InternalError(w, r, err)
		return
	}
	http.SetCookie(w, a.sessionCookie(session.Token))
	http.Redirect(w, r, "/app", http.StatusSeeOther)
}

func (a *API) showSignup(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, r, a.tmpl, "page-signup", authPageData{Title: "Sign up"})
}

func (a *API) doSignup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		renderTemplateStatus(w, r, a.tmpl, "page-signup",
			authPageData{Title: "Sign up", Flash: authFlash(http.StatusBadRequest)}, http.StatusBadRequest)
		return
	}
	email := r.PostFormValue("email")

	status, raw := a.callAuthJSON(r, http.MethodPost, "/signup", map[string]string{
		"email":    email,
		"password": r.PostFormValue("password"),
	}, "")
	if status != http.StatusCreated {
		renderTemplateStatus(w, r, a.tmpl, "page-signup",
			authPageData{Title: "Sign up", Flash: authFlash(status), Email: email}, status)
		return
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &session); err != nil || session.Token == "" {
		httpx.InternalError(w, r, err)
		return
	}
	http.SetCookie(w, a.sessionCookie(session.Token))
	http.Redirect(w, r, "/verify-sent", http.StatusSeeOther)
}

func (a *API) verifySent(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, r, a.tmpl, "page-verify-sent", authPageData{Title: "Check your inbox"})
}

// doVerifyEmail consumes the mailed link: /verify-email?token= from an inbox.
// Success renders the login shell with a fixed notice; a spent or bogus token
// renders the same shell with the failure instead.
func (a *API) doVerifyEmail(w http.ResponseWriter, r *http.Request) {
	status, _ := a.callAuthJSON(r, http.MethodPost, "/verify-email", map[string]string{
		"token": r.URL.Query().Get("token"),
	}, "")
	if status != http.StatusOK {
		renderTemplateStatus(w, r, a.tmpl, "page-login",
			authPageData{Title: "Log in", Flash: "This verification link is invalid or has expired."},
			http.StatusBadRequest)
		return
	}
	renderTemplate(w, r, a.tmpl, "page-login",
		authPageData{Title: "Log in", Flash: "Email verified. Sign in."})
}

func (a *API) showReset(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, r, a.tmpl, "page-reset", authPageData{
		Title: "Reset your password",
		Token: r.URL.Query().Get("token"),
	})
}

// doResetRequest asks for a reset link. Like its JSON twin it always answers
// the same 200: whether an account exists for the address is not something
// the page may reveal.
func (a *API) doResetRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		renderTemplateStatus(w, r, a.tmpl, "page-reset",
			authPageData{Title: "Reset your password", Flash: authFlash(http.StatusBadRequest)},
			http.StatusBadRequest)
		return
	}
	email := r.PostFormValue("email")
	a.callAuthJSON(r, http.MethodPost, "/forgot-password", map[string]string{"email": email}, "")
	renderTemplate(w, r, a.tmpl, "page-reset", authPageData{
		Title: "Reset your password",
		Email: email,
		Sent:  true,
	})
}

// doResetConfirm sets the new password from the mailed token. Success renders
// the login shell; a spent or bogus token re-renders the confirm form with
// the token kept, so a corrected password does not need a new link.
func (a *API) doResetConfirm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		renderTemplateStatus(w, r, a.tmpl, "page-reset",
			authPageData{Title: "Reset your password", Flash: authFlash(http.StatusBadRequest)},
			http.StatusBadRequest)
		return
	}
	token := r.PostFormValue("token")
	status, _ := a.callAuthJSON(r, http.MethodPost, "/reset-password", map[string]string{
		"token":        token,
		"new_password": r.PostFormValue("new_password"),
	}, "")
	if status != http.StatusOK {
		renderTemplateStatus(w, r, a.tmpl, "page-reset",
			authPageData{
				Title: "Reset your password",
				Flash: "This reset link is invalid or has expired.",
				Token: token,
			}, http.StatusBadRequest)
		return
	}
	renderTemplate(w, r, a.tmpl, "page-login",
		authPageData{Title: "Log in", Flash: "Your password has been reset. You can sign in now."})
}

// doLogout revokes the session server-side through the JSON endpoint, then
// clears the cookie and leaves for the landing page. The cookie is cleared
// even when the revocation fails: the browser half still happened, and the
// failure is logged rather than shown.
func (a *API) doLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if status, _ := a.callAuthJSON(r, http.MethodPost, "/logout", map[string]string{}, c.Value); status != http.StatusOK {
			httpx.Logger(r.Context()).WarnContext(r.Context(), "auth UI logout delegation failed", "status", status)
		}
	}
	http.SetCookie(w, clearSessionCookie())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// healthPage renders the dependency checks as a page. It reads the same
// checks readiness reports, so the shell and the load balancer never disagree
// about what "healthy" lists.
func (a *API) healthPage(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(a.checks))
	for name := range a.checks {
		names = append(names, name)
	}
	sort.Strings(names)

	data := authPageData{Title: "Service health"}
	for _, name := range names {
		data.Checks = append(data.Checks, healthStatus{
			Name: name,
			OK:   a.checks[name](r.Context()) == nil,
		})
	}
	renderTemplate(w, r, a.tmpl, "page-health", data)
}

// appPage is the signed-in dashboard: the same note blocks as the notes page,
// reached through the session cookie instead of a bearer token.
func (a *API) appPage(w http.ResponseWriter, r *http.Request) {
	notes, _, err := a.notes.List(r.Context(), auth.UserIDFromContext(r.Context()), notesPageLimit, "")
	if err != nil {
		httpx.InternalError(w, r, err)
		return
	}
	renderTemplate(w, r, a.tmpl, "app", notesPageData{Notes: notes})
}

// sessionCookieTTL is how long the browser cookie lives. It mirrors the token
// it carries; a cookie outliving its token would send a dead credential, and
// one dying first would log out a session still valid.
func (a *API) sessionCookieTTL() time.Duration {
	if a.tokenTTL > 0 {
		return a.tokenTTL
	}
	return auth.DefaultTokenTTL
}
