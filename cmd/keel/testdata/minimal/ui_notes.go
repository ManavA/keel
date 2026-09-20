package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/auth"
	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/search"
)

// notesPageLimit bounds the UI list. Keyset paging stays on the JSON API,
// which hands back cursors; the page reads the newest rows and says nothing
// about a next one.
const notesPageLimit = 50

// notesPageData is what the notes page and its blocks render. One shape for
// all of them, so a fragment is the same block the page composed rather than
// a second copy wired to a second struct.
type notesPageData struct {
	Notes []Note
	// Flash is the one inline message, rendered by the flash block and empty
	// when there is nothing to say.
	Flash string
	// Title and Body carry the caller's input back into the form after a
	// validation error, so a missing title does not eat the body they typed.
	Title string
	Body  string
}

// validateNoteTitle is the title rule for JSON creates and form creates
// alike: surrounding whitespace is not content, and an empty title is refused
// rather than stored.
func validateNoteTitle(title string) (string, error) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", errors.New("title is empty")
	}
	return trimmed, nil
}

// createNoteWithSideEffects writes a note through the store and keeps the
// derived state agreeing with it: the search index is updated inline, and the
// event is published for the side effects that may be missed. Both callers —
// the JSON API and the form below — share it, so a note written through one
// is listed and searched through the other.
func (a *API) createNoteWithSideEffects(ctx context.Context, userID, title, body string) (Note, error) {
	note, err := a.notes.Create(ctx, userID, title, body)
	if err != nil {
		return Note{}, err
	}

	// Indexed inline, not on the event. The listing and the search results are
	// expected to agree, and the in-memory bus is at-most-once: a dropped
	// message would leave a note that exists and cannot be found, with nothing
	// saying so.
	if err := a.index.IndexDocuments(ctx, []search.Document{noteDocument(note)}); err != nil {
		return Note{}, fmt.Errorf("index note %s: %w", note.ID, err)
	}

	// Published for the side effects that may be missed: a notification, a
	// metric. The reconcile job is what repairs the index if this is ever
	// wrong.
	if err := a.publisher.Publish(ctx, topicNoteCreated, note); err != nil {
		// Not fatal to the request: the note is written and indexed, and a
		// failed notification must not undo that.
		httpx.Logger(ctx).WarnContext(ctx, "publish note.created",
			"note_id", note.ID, "error", err)
	}
	return note, nil
}

// deleteNoteWithSideEffects removes a note and its index document. The row
// going away is the request succeeding; an index left stale by one document
// is what the reconcile job repairs.
func (a *API) deleteNoteWithSideEffects(ctx context.Context, userID, id string) error {
	if err := a.notes.Delete(ctx, userID, id); err != nil {
		return err
	}
	if err := a.index.RemoveDocuments(ctx, []string{id}); err != nil {
		httpx.Logger(ctx).ErrorContext(ctx, "remove note from index",
			"note_id", id, "error", err)
	}
	return nil
}

// NotesUIRoutes mounts the browser UI for notes. Every route requires a
// session, exactly like the JSON API: the caller is whoever the bearer token
// says they are, and every query is scoped to that account. The handlers read
// the store, index and auth service directly, never the API over HTTP.
func (a *API) NotesUIRoutes(r chi.Router) {
	r.Route("/notes", func(r chi.Router) {
		// The cookie rides along for browsers: cookieToAuth only fills in
		// Authorization when no bearer token is present, so API clients are
		// unaffected and the page works from a plain link.
		r.Use(a.cookieToAuth, a.auth.RequireAuth)
		r.Get("/", a.notesPage)
		r.Post("/", a.notesCreate)
		r.Delete("/{id}", a.notesDelete)
	})
}

// notesPage renders the notes page, or the list block on its own when htmx
// asks for the fragment.
func (a *API) notesPage(w http.ResponseWriter, r *http.Request) {
	notes, _, err := a.notes.List(r.Context(), auth.UserIDFromContext(r.Context()), notesPageLimit, "")
	if err != nil {
		// The cursor is always empty here, so a failure is the database, not
		// the caller.
		httpx.InternalError(w, r, err)
		return
	}
	data := notesPageData{Notes: notes}
	if isHXRequest(r) {
		renderTemplate(w, r, a.tmpl, "note_list", data)
		return
	}
	renderTemplate(w, r, a.tmpl, "notes", data)
}

// notesCreate reads a form-encoded note and writes it through the same path
// as a JSON create. Over htmx the answer is the new card at 201; without it
// the answer is a redirect back to the list. A missing title is 422 with the
// form re-rendered around a flash, never a silent redirect that drops the
// input.
func (a *API) notesCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		a.notesFormError(w, r, notesPageData{}, "The form could not be read.")
		return
	}
	form := notesPageData{Title: r.PostFormValue("title"), Body: r.PostFormValue("body")}

	title, err := validateNoteTitle(form.Title)
	if err != nil {
		a.notesFormError(w, r, form, "A title is required.")
		return
	}

	note, err := a.createNoteWithSideEffects(r.Context(), auth.UserIDFromContext(r.Context()), title, form.Body)
	if err != nil {
		httpx.InternalError(w, r, err)
		return
	}

	if !isHXRequest(r) {
		http.Redirect(w, r, "/notes", http.StatusSeeOther)
		return
	}
	renderTemplateStatus(w, r, a.tmpl, "note_card", note, http.StatusCreated)
}

// notesFormError answers a form validation failure. The status is 422 with
// the flash set; htmx swaps the form block back in place, a plain submit gets
// the whole page, and neither echoes the input beyond returning what was
// typed into the fields it came from.
func (a *API) notesFormError(w http.ResponseWriter, r *http.Request, form notesPageData, flash string) {
	form.Flash = flash
	if !isHXRequest(r) {
		notes, _, err := a.notes.List(r.Context(), auth.UserIDFromContext(r.Context()), notesPageLimit, "")
		if err != nil {
			httpx.InternalError(w, r, err)
			return
		}
		form.Notes = notes
		renderTemplateStatus(w, r, a.tmpl, "notes", form, http.StatusUnprocessableEntity)
		return
	}
	renderTemplateStatus(w, r, a.tmpl, "note_form", form, http.StatusUnprocessableEntity)
}

// notesDelete removes one of the caller's notes. Over htmx the answer is 200
// with nothing to swap in, so the card removes itself; without it the answer
// is a redirect back to the list. Another account's row answers 404, the same
// as for a row that does not exist.
func (a *API) notesDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	err := a.deleteNoteWithSideEffects(r.Context(), auth.UserIDFromContext(r.Context()), id)
	switch {
	case errors.Is(err, ErrNoteNotFound):
		httpx.NotFound(w, r)
		return
	case err != nil:
		if isInvalidTextRepresentation(err) {
			httpx.NotFound(w, r)
			return
		}
		httpx.InternalError(w, r, err)
		return
	}

	if !isHXRequest(r) {
		http.Redirect(w, r, "/notes", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusOK)
}
