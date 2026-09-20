package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/httpx"
)

// uiFiles ships the browser UI with the binary, next to migrationFiles: which
// templates and static assets a build carries is never a question of what was
// deployed alongside it.
//
// templates/ holds pages (landing, notes), templates/blocks/ holds the
// composable blocks pages are made of, and static/ holds the stylesheets,
// vendored scripts and icons those pages reference. A fragment is a block
// rendered on its own, never a second copy: pages compose blocks with the
// template action, and htmx requests execute the same block standalone.
//
//go:embed templates static
var uiFiles embed.FS

// ParseTemplates parses every page and block once, at startup. It fails fast
// rather than serving half a UI: a template that does not parse stops the
// process before it listens. A new template directory must be added to the
// patterns below, or it silently never loads.
func ParseTemplates() (*template.Template, error) {
	tmpl, err := template.New("ui").ParseFS(uiFiles, "templates/*.html", "templates/blocks/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse UI templates: %w", err)
	}
	return tmpl, nil
}

// UI serves what needs no database: the landing shell and the static assets.
// The notes UI lives on API instead, beside the store, index and auth service
// it is built from; both are wired in run() and in newTestServer the same way,
// so a route the binary serves and the tests never mount cannot drift.
type UI struct {
	tmpl *template.Template
}

// NewUI holds the parsed templates for the routes below.
func NewUI(tmpl *template.Template) *UI { return &UI{tmpl: tmpl} }

// Routes mounts the landing page and the static assets. The landing page
// needs no session; the auth shells flesh it out with signup and login, and
// the notes routes stay behind RequireAuth on API. Fragments are blocks
// rendered standalone: an htmx request gets the block, anything else the page.
func (u *UI) Routes(r chi.Router) {
	MountStatic(r)
	r.Get("/", u.landing)
}

func (u *UI) landing(w http.ResponseWriter, r *http.Request) {
	if isHXRequest(r) {
		renderTemplate(w, r, u.tmpl, "landing-main", nil)
		return
	}
	renderTemplate(w, r, u.tmpl, "landing", nil)
}

// MountStatic serves /static from the embedded assets. Vendored scripts are
// immutable — their bytes change only when static/vendor/VERSION does, and
// the far-future max-age says so — while the stylesheets this repo owns
// revalidate every time, so a token edit reaches browsers without a rename.
func MountStatic(r chi.Router) {
	sub, err := fs.Sub(uiFiles, "static")
	if err != nil {
		// Sub fails only when the embed patterns above stop matching, which
		// ParseTemplates already refused at startup before this runs.
		panic(fmt.Sprintf("static assets unreachable: %v", err))
	}
	r.Handle("/static/*", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "vendor/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case strings.HasSuffix(r.URL.Path, ".css"):
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
	})))
}

// isHXRequest reports an htmx request, which wants the block rather than the
// page around it.
func isHXRequest(r *http.Request) bool { return r.Header.Get("HX-Request") != "" }

// renderTemplate executes one page or block as HTML. It buffers first, so a
// template failure is a 500 rather than a half-written 200.
func renderTemplate(w http.ResponseWriter, r *http.Request, tmpl *template.Template, name string, data any) {
	renderTemplateStatus(w, r, tmpl, name, data, http.StatusOK)
}

// renderTemplateStatus is renderTemplate with an explicit status, for answers
// whose code is part of the contract: 201 for a card just created, 422 for a
// form re-rendered around a flash.
func renderTemplateStatus(w http.ResponseWriter, r *http.Request, tmpl *template.Template, name string, data any, status int) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		httpx.InternalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
