package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/ManavA/keel/log"
)

// ErrorBody is the shape of every error this package writes. The message is a
// fixed phrase for the status code, never the Go error, the identifier the
// caller sent, or the name of the table that rejected it. The request id lets a
// user quote something that leads to the log line holding the detail.
type ErrorBody struct {
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

// JSON writes v as a JSON response with the given status. A nil v writes the
// status and no body.
//
// A nil slice is written as [] rather than null, because `null.length` throws
// in the browser clients that consume list endpoints.
func JSON(w http.ResponseWriter, status int, v any) {
	if v == nil {
		// No Content-Type: an empty body labelled application/json is what a
		// strict client rejects.
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	// The status line has already gone out, so the only error Encode can return
	// now is the client having hung up.
	_ = json.NewEncoder(w).Encode(normalizeNilSlice(v))
}

// normalizeNilSlice turns a nil slice into an empty one, at the top level only.
// Walking the whole value graph would cost something on every response, and a
// nested null is a field a client can check.
func normalizeNilSlice(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
	}
	return v
}

// NoContent writes 204.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Error writes a generic error body for status and logs the real reason with
// the request id, through the logger NewRouter put on the request context.
// err may be nil, for a refusal with no underlying failure.
//
// The body never carries the input or the Go error. An identifier echoed back
// is a reflection bug once it contains markup and confirms the guess was well
// formed; "duplicate key value violates unique constraint users_email_key"
// names the schema and answers "is this address registered" on the way past.
// For the same reason a record belonging to someone else is 404, never 403: a
// 403 confirms it exists.
func Error(w http.ResponseWriter, r *http.Request, status int, err error) {
	id := log.RequestID(r.Context())

	level := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	Logger(r.Context()).LogAttrs(r.Context(), level, "request failed",
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

// BadRequest refuses a malformed request. err is logged, not returned:
// validation messages describe internal field names. An endpoint that should
// tell a caller which field was wrong needs a response designed for that.
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

// statusMessage is the only text an error response carries. It is derived from
// the status code alone, so no value from the request can reach the body.
func statusMessage(status int) string {
	if text := http.StatusText(status); text != "" {
		return strings.ToLower(text)
	}
	return "error"
}
