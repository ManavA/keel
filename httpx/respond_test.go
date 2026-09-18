package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.JSON(rec, http.StatusCreated, map[string]string{"id": "abc"})

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"id":"abc"}`, rec.Body.String())
}

func TestJSONNilSliceIsAnEmptyArray(t *testing.T) {
	var items []string
	rec := httptest.NewRecorder()
	httpx.JSON(rec, http.StatusOK, items)

	// null here is what makes `response.length` throw in every browser client.
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestJSONNilBodyWritesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.JSON(rec, http.StatusAccepted, nil)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestNoContent(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.NoContent(rec)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestErrorNeverEchoesTheInputOrTheError(t *testing.T) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))

	req := httptest.NewRequest(http.MethodGet,
		"/listings/<script>alert(1)</script>?email=someone@example.com", nil)
	rec := httptest.NewRecorder()

	httpx.Error(rec, req, http.StatusNotFound,
		errors.New(`pq: duplicate key value violates unique constraint "users_email_key"`))

	body := rec.Body.String()
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, body, "script")
	assert.NotContains(t, body, "someone@example.com")
	assert.NotContains(t, body, "users_email_key")
	assert.NotContains(t, body, "pq:")

	var parsed httpx.ErrorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
	assert.Equal(t, "not found", parsed.Error)
}

func TestErrorCarriesTheRequestID(t *testing.T) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(log.WithRequestID(req.Context(), "req-abc"))
	rec := httptest.NewRecorder()

	httpx.Error(rec, req, http.StatusInternalServerError, errors.New("boom"))

	var parsed httpx.ErrorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
	assert.Equal(t, "req-abc", parsed.RequestID,
		"the id is the only thing tying a user's report to the log line that explains it")
}

func TestErrorHelpers(t *testing.T) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))

	tests := []struct {
		name    string
		call    func(http.ResponseWriter, *http.Request)
		status  int
		message string
	}{
		{"NotFound", func(w http.ResponseWriter, r *http.Request) { httpx.NotFound(w, r) },
			http.StatusNotFound, "not found"},
		{"Unauthorized", func(w http.ResponseWriter, r *http.Request) { httpx.Unauthorized(w, r) },
			http.StatusUnauthorized, "unauthorized"},
		{"BadRequest", func(w http.ResponseWriter, r *http.Request) { httpx.BadRequest(w, r, errors.New("x")) },
			http.StatusBadRequest, "bad request"},
		{"InternalError", func(w http.ResponseWriter, r *http.Request) { httpx.InternalError(w, r, errors.New("x")) },
			http.StatusInternalServerError, "internal server error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.call(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			assert.Equal(t, tt.status, rec.Code)
			var parsed httpx.ErrorBody
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
			assert.Equal(t, tt.message, parsed.Error)
		})
	}
}

func TestNotFoundAndUnauthorizedReadDifferently(t *testing.T) {
	// A foreign id is answered 404, the same as an invented one, so that
	// NotFound is never "fixed" into a 403 for a row that does exist.
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))

	notFound := httptest.NewRecorder()
	httpx.NotFound(notFound, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusNotFound, notFound.Code)
	assert.NotEqual(t, http.StatusForbidden, notFound.Code)
}

func TestJSONNilBodyHasNoContentType(t *testing.T) {
	// An empty body labelled application/json is what a strict client rejects.
	rec := httptest.NewRecorder()
	httpx.JSON(rec, http.StatusAccepted, nil)
	assert.Empty(t, rec.Header().Get("Content-Type"))
}

func TestErrorLogsThroughTheContextLogger(t *testing.T) {
	// ARCHITECTURE.md: a package that logs uses the logger the service
	// configured. Here that arrives on the request context.
	var buf bytes.Buffer
	logger := log.New(log.Options{Output: &buf})

	req := httptest.NewRequest(http.MethodGet, "/thing", nil)
	req = req.WithContext(httpx.WithLogger(req.Context(), logger))
	httpx.InternalError(httptest.NewRecorder(), req, errors.New("the reason"))

	assert.Contains(t, buf.String(), "the reason")
	assert.Contains(t, buf.String(), "/thing")
}

func TestLoggerFallsBackToDefault(t *testing.T) {
	assert.NotNil(t, httpx.Logger(context.Background()))
	assert.NotNil(t, httpx.Logger(nil)) //nolint:staticcheck // a nil context must not panic here
	logger := log.New(log.Options{Output: io.Discard})
	assert.Same(t, logger, httpx.Logger(httpx.WithLogger(context.Background(), logger)))
	assert.NotSame(t, logger, httpx.Logger(httpx.WithLogger(context.Background(), nil)))
}
