package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/ManavA/keel/log"
)

// ErrorBody is the shape of every error this package writes.
//
// One field, and it holds a fixed phrase for the status code. Not the error
// Go produced, not the identifier the caller sent, not the name of the table
// that rejected it. The request id is there so that a user quoting it from a
// support ticket leads you straight to the log line that does hold all of that.
type ErrorBody struct {
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

// JSON writes v as a JSON response with the given status.
//
// A nil slice is written as [] rather than null. The two are the same thing in
// Go and are not the same thing in a browser, where `null.length` throws and
// every consumer of a list endpoint has to learn that the hard way.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if v == nil {
		return
	}
	// Encode writes to a connection whose status line has already gone out, so
	// the only errors it can return now are the client having hung up. There is
	// nothing to say to them and nothing worth logging.
	_ = json.NewEncoder(w).Encode(normalizeNilSlice(v))
}

// normalizeNilSlice turns a nil slice into an empty one. Only at the top level:
// walking a whole value graph to fix up nested nil slices would cost something
// on every response, and a nested null is a field a client can check, whereas a
// null where the entire body should be a list is a crash.
func normalizeNilSlice(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
	}
	return v
}

// NoContent writes 204.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Error writes a generic error body for status, and logs the real reason with
// the request id.
//
// The caller sees "not found". The log sees the error, the path and the id that
// ties them together. Keeping those apart is the whole rule:
//
//   - 404, never 403, for an identifier that belongs to someone else. A 403
//     says the thing exists, which is the one fact the request was trying to
//     establish. Answer a foreign id exactly as you answer an invented one.
//   - Never echo the input. An identifier that comes back in the response body
//     is a reflection bug as soon as somebody puts markup in it, and quoting it
//     tells the caller their guess was well formed.
//   - Never return the error text. "pq: duplicate key value violates unique
//     constraint users_email_key" names your database, your schema and your
//     index, and it answers "is this address registered" on the way past.
//
// err may be nil, for a refusal that has no underlying failure.
func Error(w http.ResponseWriter, r *http.Request, status int, err error) {
	id := log.RequestID(r.Context())

	level := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	slog.Default().LogAttrs(r.Context(), level, "request failed",
		slog.Int("status", status),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("error", err),
	)

	JSON(w, status, ErrorBody{Error: statusMessage(status), RequestID: id})
}

// NotFound is the response for anything the caller may not have, whether it
// does not exist, was deleted, or belongs to somebody else.
func NotFound(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusNotFound, nil)
}

// BadRequest refuses a malformed request. err is logged, not returned: a
// validation message written for a developer describes your internal field
// names, and a caller who needs to know which field was wrong needs a
// deliberately designed response, not a leaked one.
func BadRequest(w http.ResponseWriter, r *http.Request, err error) {
	Error(w, r, http.StatusBadRequest, err)
}

// Unauthorized refuses a request with no valid credentials.
func Unauthorized(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusUnauthorized, nil)
}

// InternalError reports a failure that is not the caller's fault.
func InternalError(w http.ResponseWriter, r *http.Request, err error) {
	Error(w, r, http.StatusInternalServerError, err)
}

// statusMessage is the only text an error response ever carries. It comes from
// the status code and nothing else, so there is no path by which a value from
// the request reaches the body.
func statusMessage(status int) string {
	if text := http.StatusText(status); text != "" {
		return strings.ToLower(text)
	}
	return "error"
}
