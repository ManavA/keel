package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/auth"
	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/search"
)

// topicNoteCreated carries a note that was just written.
const topicNoteCreated = "note.created"

// API holds what the handlers need. Concrete types where there is one
// implementation, interfaces where the point is that the implementation is
// chosen by configuration.
type API struct {
	notes     *Notes
	index     search.Index
	publisher events.Publisher
	auth      *auth.Service
	// checks backs the health shell with the same probes readiness reports.
	checks map[string]httpx.Check
}

// Routes registers this API under r. Every notes route requires a session:
// the caller is whoever the bearer token says they are, and every query is
// scoped to that account, so one account's notes are invisible to another's.
func (a *API) Routes(r chi.Router) {
	r.Route("/api/notes", func(r chi.Router) {
		r.Use(a.auth.RequireAuth)
		r.Post("/", a.create)
		r.Get("/", a.list)
		// Before the {id} route, or "search" is read as an id.
		r.Get("/search", a.search)
		r.Get("/{id}", a.get)
		r.Delete("/{id}", a.delete)
	})
}

// maxBodyBytes caps a request body. Without it a client can make the server
// allocate whatever it likes.
const maxBodyBytes = 64 << 10

type createRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err)
		return
	}

	title, err := validateNoteTitle(req.Title)
	if err != nil {
		httpx.BadRequest(w, r, err)
		return
	}

	note, err := a.createNoteWithSideEffects(r.Context(), auth.UserIDFromContext(r.Context()), title, req.Body)
	if err != nil {
		httpx.InternalError(w, r, err)
		return
	}

	httpx.JSON(w, http.StatusCreated, note)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	note, err := a.notes.Get(r.Context(), auth.UserIDFromContext(r.Context()), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, ErrNoteNotFound):
		httpx.NotFound(w, r)
	case err != nil:
		// An id that is not a UUID reaches Postgres as a cast error rather
		// than as no rows, and that is a bad request, not a server fault.
		if isInvalidTextRepresentation(err) {
			httpx.NotFound(w, r)
			return
		}
		httpx.InternalError(w, r, err)
	default:
		httpx.JSON(w, http.StatusOK, note)
	}
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
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

	httpx.NoContent(w)
}

type listResponse struct {
	Notes []Note `json:"notes"`
	// NextCursor is empty on the last page. It is opaque: the shape can change
	// without breaking a client that only ever hands it back.
	NextCursor string `json:"next_cursor,omitempty"`
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	page, err := pg.ParsePage(r.URL.Query(), pg.PageOptions{DefaultLimit: 20, MaxLimit: 100})
	if err != nil {
		httpx.BadRequest(w, r, err)
		return
	}

	notes, next, err := a.notes.List(r.Context(), auth.UserIDFromContext(r.Context()), page.Limit, r.URL.Query().Get("cursor"))
	if err != nil {
		httpx.BadRequest(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, listResponse{Notes: notes, NextCursor: next})
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	page, err := pg.ParsePage(r.URL.Query(), pg.PageOptions{DefaultLimit: 20, MaxLimit: 100})
	if err != nil {
		httpx.BadRequest(w, r, err)
		return
	}

	result, err := a.index.Search(r.Context(), search.Query{
		Text:   r.URL.Query().Get("q"),
		Limit:  int64(page.Limit),
		Offset: int64(page.Offset),
		// The index holds every account's notes; the filter keeps a caller
		// to their own, the same scoping the listing queries apply.
		Filters: []search.Filter{search.Eq(userIDField, auth.UserIDFromContext(r.Context()))},
		Sort:    []search.SortField{{Field: createdAtField, Dir: search.Desc}},
	})
	if err != nil {
		httpx.InternalError(w, r, err)
		return
	}

	notes, err := search.DecodeHits[Note](result.Hits)
	if err != nil {
		httpx.InternalError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, listResponse{Notes: notes})
}

// noteDocument is the note as the search index stores it. The field names match
// Note's JSON tags so DecodeHits can read a hit straight back into a Note.
func noteDocument(n Note) search.Document {
	return search.MapDocument{
		"id":           n.ID,
		userIDField:    n.UserID,
		"title":        n.Title,
		"body":         n.Body,
		createdAtField: n.CreatedAt,
	}
}

// decodeJSON reads a JSON body, refusing anything oversized or malformed. It
// rejects unknown fields: a client sending "tilte" has made a mistake, and
// accepting the request silently drops what they meant to say.
func decodeJSON(w http.ResponseWriter, r *http.Request, dest any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	// A second value in the body means the client sent something other than
	// the one object this endpoint reads.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body has more than one JSON value")
	}
	return nil
}

// isInvalidTextRepresentation reports a Postgres cast failure, which is what an
// id that is not a UUID produces. Matching on the SQLSTATE rather than on the
// message keeps the caller's input out of the comparison.
func isInvalidTextRepresentation(err error) bool {
	return pgErrorCode(err) == "22P02"
}

// pgErrorCode is the SQLSTATE of a Postgres error, or "" for anything else.
func pgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// validateNoteTitle is the title rule: surrounding whitespace is not content,
// and an empty title is refused rather than stored.
func validateNoteTitle(title string) (string, error) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", errors.New("title is empty")
	}
	return trimmed, nil
}

// createNoteWithSideEffects writes a note through the store and keeps the
// derived state agreeing with it: the search index is updated inline, and the
// event is published for the side effects that may be missed.
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
