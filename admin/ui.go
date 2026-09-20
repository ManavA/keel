package admin

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/httpx"
)

// AdminSessionCookieName carries the admin session token for the browser UI.
// The JSON API keeps answering bearer tokens; the cookie is the same token in
// a jar the browser sends on its own, so pages and fragments work from a
// plain link.
const AdminSessionCookieName = "keel_admin_session"

// uiTemplates ships the browser UI with the package, so which templates a
// build carries is never a question of what was deployed alongside it.
// pages holds the full documents; every fragment a page composes is also
// defined there, and an htmx request executes the same fragment standalone
// rather than a second copy.
//
//go:embed templates/*.html
var uiTemplates embed.FS

// parseUITemplates parses every admin UI page and fragment once, at NewService.
// It fails fast rather than serving half a console: a template that does not
// parse stops the service from being built.
func parseUITemplates() (*template.Template, error) {
	tmpl, err := template.New("admin-ui").ParseFS(uiTemplates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("admin: parse UI templates: %w", err)
	}
	return tmpl, nil
}

// maxUIBodyBytes caps a form body. Without it a client can make the server
// allocate whatever it likes.
const maxUIBodyBytes = 64 << 10

// MountUI mounts the browser UI under basePath: the login page, the users
// list with per-admin session revoke, and the filterable audit trail. A nil
// or empty basePath mounts at "/".
//
// The pages are plain forms that work without script; with htmx served by the
// host app at /static/vendor/htmx.min.js (the same path the examples'
// MountStatic serves), the revoke button and the audit filter swap fragments
// instead of reloading. Every page requires a valid admin session except
// /login, and no admin-only data renders without one: anonymous page loads
// leave for the login page, anonymous fragments keep the JSON API's 401.
func (s *Service) MountUI(r chi.Router, basePath string) {
	base := strings.TrimSuffix(strings.TrimSpace(basePath), "/")
	ui := chi.NewRouter()
	ui.Get("/login", s.showLogin(base))
	ui.Post("/login", s.doLogin(base))
	ui.Post("/logout", s.doLogout(base))
	ui.Group(func(r chi.Router) {
		r.Use(s.requirePageAdmin(base))
		r.Get("/users", s.usersPage(base))
		r.Post("/users/{id}/revoke", s.revokeSession(base))
		r.Get("/audit", s.auditPage(base))
	})
	if base == "" {
		r.Mount("/", ui)
		return
	}
	r.Mount(base, http.StripPrefix(base, ui))
}

// cookieToAdmin copies the session cookie into Authorization when no bearer
// token is present. RequireAdmin runs unchanged behind it: a request carrying
// both stays a bearer request, and a forged cookie is the same 401 as a
// forged token.
func (s *Service) cookieToAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if c, err := r.Cookie(AdminSessionCookieName); err == nil && c.Value != "" {
				r.Header.Set("Authorization", "Bearer "+c.Value)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requirePageAdmin runs RequireAdmin and turns its 401 into a redirect to the
// login page. Fragments keep the raw 401; a page that answered 401 would show
// a plain-text error inside a browser tab, which helps nobody.
func (s *Service) requirePageAdmin(base string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		authed := s.cookieToAdmin(s.RequireAdmin(next))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := httptest.NewRecorder()
			authed.ServeHTTP(rec, r)
			if rec.Code == http.StatusUnauthorized {
				if isHXRequest(r) {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				http.Redirect(w, r, base+"/login", http.StatusSeeOther)
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
}

// isHXRequest reports an htmx request, which wants the fragment rather than
// the page around it.
func isHXRequest(r *http.Request) bool { return r.Header.Get("HX-Request") != "" }

// renderUITemplate executes one page or fragment as HTML. It buffers first,
// so a template failure is a 500 rather than a half-written 200.
func (s *Service) renderUITemplate(w http.ResponseWriter, r *http.Request, name string, data any) {
	s.renderUITemplateStatus(w, r, name, data, http.StatusOK)
}

// renderUITemplateStatus is renderUITemplate with an explicit status, for
// answers whose code is part of the contract: 401 for a login re-rendered
// around a flash, 201 where a fragment was just created.
func (s *Service) renderUITemplateStatus(w http.ResponseWriter, r *http.Request, name string, data any, status int) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		httpx.InternalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// loginPageData is what the login page renders. Flash holds only fixed
// strings chosen by status code, never anything from the request; Email
// carries the caller's address back into the form, escaped by the template.
type loginPageData struct {
	Base  string
	Flash string
	Email string
}

func (s *Service) showLogin(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.renderUITemplate(w, r, "page-login", loginPageData{Base: base})
	}
}

// callLoginJSON runs the JSON Login endpoint in-process against the same
// handler POST /login serves: the same password check, the same rate limiter,
// no network. The caller's RemoteAddr travels along, so the per-IP limiter
// still buckets the browser behind the request rather than every form submit
// together.
func (s *Service) callLoginJSON(r *http.Request, email, password string) (int, string) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(LoginRequest{Email: email, Password: password}); err != nil {
		return http.StatusInternalServerError, ""
	}
	req := httptest.NewRequest(http.MethodPost, "/login", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = r.RemoteAddr
	rec := httptest.NewRecorder()
	s.login.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, ""
	}
	var resp SessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Token == "" {
		return http.StatusInternalServerError, ""
	}
	return rec.Code, resp.Token
}

// adminLoginFlash maps a delegated login status to a fixed message. The 401
// text matches the JSON API's own generic body, so the form and the API say
// the same thing for the same outcome; nothing from the request or the
// delegated body ever reaches the page.
func adminLoginFlash(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid credentials"
	case http.StatusTooManyRequests:
		return "too many requests"
	case http.StatusServiceUnavailable:
		return "unavailable"
	case http.StatusBadRequest:
		return "That request was not valid."
	default:
		return "Something went wrong."
	}
}

func (s *Service) doLogin(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUIBodyBytes)
		if err := r.ParseForm(); err != nil {
			s.renderUITemplateStatus(w, r, "page-login",
				loginPageData{Base: base, Flash: adminLoginFlash(http.StatusBadRequest)},
				http.StatusBadRequest)
			return
		}
		email := r.PostFormValue("email")

		status, token := s.callLoginJSON(r, email, r.PostFormValue("password"))
		if status != http.StatusOK {
			s.renderUITemplateStatus(w, r, "page-login",
				loginPageData{Base: base, Flash: adminLoginFlash(status), Email: email},
				status)
			return
		}
		http.SetCookie(w, s.sessionCookie(r, token))
		http.Redirect(w, r, base+"/users", http.StatusSeeOther)
	}
}

// sessionCookie wraps a session token for the browser. Secure follows the
// request or the console origin rather than a flag: an https console that set
// a non-Secure cookie would send the token in the clear, and an http console
// (local development) that set Secure would never send it at all.
func (s *Service) sessionCookie(r *http.Request, token string) *http.Cookie {
	secure := r.TLS != nil ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.corsOrigin)), "https://")
	return &http.Cookie{
		Name:     AdminSessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(s.session.ttl.Seconds()),
	}
}

// clearSessionCookie ends the browser half of a session: an expired cookie
// the browser drops.
func clearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     AdminSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

func (s *Service) doLogout(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Admin sessions end server-side through revoke, not through logout:
		// this clears the browser half, and a stolen token stays valid until
		// its expiry unless an admin revokes it from the users list.
		http.SetCookie(w, clearSessionCookie())
		http.Redirect(w, r, base+"/login", http.StatusSeeOther)
	}
}

// adminUserView is the one admin row the users list may render: identity and
// activity, never credential or session material. Rendering from Admin
// directly would put PasswordHash one template edit away from the page.
type adminUserView struct {
	ID        string
	Email     string
	Name      string
	Role      string
	LastLogin string
}

// usersPageData is what the users page and its rows fragment render. Base is
// the mount path the forms post back to.
type usersPageData struct {
	Base  string
	Users []adminUserView
}

// formatLastLogin is the activity column: a short timestamp, or "never" for
// an account that has not logged in yet.
func formatLastLogin(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("2006-01-02 15:04")
}

// usersPage renders the users list, or the rows block on its own when htmx
// asks for the fragment.
func (s *Service) usersPage(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admins, err := s.users.List(r.Context())
		if err != nil {
			s.logger(r.Context()).Error("admin: users list failed", "error", err)
			writeGenericError(w, http.StatusInternalServerError)
			return
		}
		data := usersPageData{Base: base}
		for _, a := range admins {
			data.Users = append(data.Users, adminUserView{
				ID:        a.ID,
				Email:     a.Email,
				Name:      a.Name,
				Role:      a.Role,
				LastLogin: formatLastLogin(a.LastLoginAt),
			})
		}
		if isHXRequest(r) {
			s.renderUITemplate(w, r, "admin_user_rows", data)
			return
		}
		s.renderUITemplate(w, r, "page-users", data)
	}
}

// revokeSession ends every session belonging to one admin. Over htmx the
// answer is a notice fragment; without it the answer is a redirect back to
// the list. An unknown id answers 404, the same as for a row that does not
// exist. The revoke is recorded in the audit trail with the acting admin as
// actor; a failing audit write is logged, not answered, the way the Audit
// middleware treats its own bookkeeping write.
func (s *Service) revokeSession(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := s.users.RevokeSessions(r.Context(), id); err != nil {
			if errors.Is(err, ErrAdminNotFound) {
				http.NotFound(w, r)
				return
			}
			s.logger(r.Context()).Error("admin: revoke sessions failed", "target", id, "error", err)
			writeGenericError(w, http.StatusInternalServerError)
			return
		}
		entry := &AuditEntry{
			Actor:   AdminIDFromContext(r.Context()),
			Action:  "admin.sessions.revoke",
			Target:  id,
			Outcome: AuditOutcomeOK,
		}
		if err := s.audit.Append(r.Context(), entry); err != nil {
			s.logger(r.Context()).Warn("admin: revoke not recorded",
				"actor", entry.Actor, "target", id, "error", err)
		}
		if !isHXRequest(r) {
			http.Redirect(w, r, base+"/users", http.StatusSeeOther)
			return
		}
		s.renderUITemplate(w, r, "notice", "Sessions revoked.")
	}
}

// auditPageData is what the audit page and its rows fragment render: the
// current filter, echoed back into the form, and the entries it selected.
type auditPageData struct {
	Base    string
	Action  string
	Actor   string
	Outcome string
	Entries []AuditEntry
}

// auditPage renders the audit trail with its filter, or the rows block on its
// own when htmx asks for the fragment. The paging rules match AuditList:
// limit defaults to 50 and caps at 200.
func (s *Service) auditPage(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		data := auditPageData{
			Base:    base,
			Action:  q.Get("action"),
			Actor:   q.Get("actor"),
			Outcome: q.Get("outcome"),
		}
		limit := 50
		if raw := q.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				http.Error(w, "invalid limit", http.StatusBadRequest)
				return
			}
			limit = n
		}
		if limit > 200 {
			limit = 200
		}
		offset := 0
		if raw := q.Get("offset"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				http.Error(w, "invalid offset", http.StatusBadRequest)
				return
			}
			offset = n
		}

		entries, err := s.audit.List(r.Context(), AuditFilter{
			Action:  data.Action,
			Actor:   data.Actor,
			Outcome: data.Outcome,
		}, limit, offset)
		if err != nil {
			s.logger(r.Context()).Error("admin: audit list failed", "error", err)
			writeGenericError(w, http.StatusInternalServerError)
			return
		}
		data.Entries = entries
		if isHXRequest(r) {
			s.renderUITemplate(w, r, "audit_rows", data)
			return
		}
		s.renderUITemplate(w, r, "page-audit", data)
	}
}
