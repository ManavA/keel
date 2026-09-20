package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/auth"
	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/idempotency"
)

// topicNoteCreated carries a note that was just written. The create handler
// enqueues it in the same transaction as the row; the relay publishes it
// afterward.
const topicNoteCreated = "note.created"

// API holds what the handlers need.
type API struct {
	notes *Notes
	auth  *auth.Service
	idem  idempotency.Store
}

// Routes registers this API under r. Every notes route requires a session:
// the caller is whoever the bearer token says they are, and every query is
// scoped to that account, so one account's notes are invisible to another's.
//
// The create route additionally sits behind the idempotency middleware, so
// a client that retries a POST whose response it never saw gets the first
// response replayed instead of a second note.
func (a *API) Routes(r chi.Router) {
	r.Route("/api/notes", func(r chi.Router) {
		r.Use(a.auth.RequireAuth)
		r.With(idempotency.Middleware(idempotency.Options{Store: a.idem})).Post("/", a.create)
		r.Get("/{id}", a.get)
	})
}

// maxBodyBytes caps a request body. Without it a client can make the server
// allocate whatever it likes — and the idempotency middleware buffers the
// body whole, so an unbounded one reaches further here than elsewhere.
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

	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		httpx.BadRequest(w, r, errors.New("title is empty"))
		return
	}

	note, err := a.notes.CreateWithEvent(r.Context(), auth.UserIDFromContext(r.Context()), req.Title, req.Body)
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
		if pgErrorCode(err) == "22P02" {
			httpx.NotFound(w, r)
			return
		}
		httpx.InternalError(w, r, err)
	default:
		httpx.JSON(w, http.StatusOK, note)
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

// pgErrorCode is the SQLSTATE of a Postgres error, or "" for anything else.
// Matching on the code rather than on the message keeps the caller's input
// out of the comparison.
func pgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
