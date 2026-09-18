package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/events"
	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
	"github.com/ManavA/keel/search"
)

// An example nobody runs stops working quietly. These tests exercise it the way
// the README says to, against a real database, so a signature drifting in one
// of the packages it wires shows up as a failure rather than as a stale file.

func TestMain(m *testing.M) {
	slog.SetDefault(keellog.New(keellog.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// newTestServer builds the service the way run() does and returns it wrapped in
// an httptest server, with its own schema so tests do not collide.
func newTestServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()

	db := testdb.Shared(t)
	ctx := context.Background()
	schema := "example_" + randomSuffix(t)

	admin, err := pg.Open(ctx, pg.Options{URL: db.URL})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	admin.Close()

	pool, err := pg.Open(ctx, pg.Options{URL: db.URL + "&search_path=" + schema})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	sub, err := fs.Sub(migrationFiles, "migrations")
	require.NoError(t, err)
	_, err = migrate.Run(ctx, pool, migrate.Options{FS: sub})
	require.NoError(t, err)

	index := search.NewPostgresIndex(search.PostgresIndexOptions{
		Pool: pool,
		Config: search.PostgresConfig{
			Table:            searchTable,
			SearchableFields: []string{"title", "body"},
			TextSearchConfig: "english",
		},
	})
	require.NoError(t, index.EnsureSchema(ctx))

	api := &API{notes: NewNotes(pool), index: index, publisher: events.NewNoopPublisher()}
	// A tiny readiness cache so a test can observe a dependency going away.
	// The default is five seconds, which is right in production and too slow
	// to assert against.
	cfg := Config{Port: 8080, Env: "development", ReadinessCacheTTL: time.Millisecond}
	srv := httptest.NewServer(newRouter(cfg, slog.Default(), api, pool, index))
	t.Cleanup(srv.Close)

	return srv, pool
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return hex.EncodeToString(b[:])
}

// postNote sends a create request. Every caller uses the same path, so it is
// not a parameter.
func postNote(t *testing.T, srv *httptest.Server, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := srv.Client().Post(srv.URL+"/api/notes", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

func get(t *testing.T, srv *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

func TestCreateAndReadANote(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, raw := postNote(t, srv, `{"title":"Roof repair","body":"Slate tiles"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "Roof repair", created.Title)
	assert.False(t, created.CreatedAt.IsZero())

	resp, raw = get(t, srv, "/api/notes/"+created.ID)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var fetched Note
	require.NoError(t, json.Unmarshal(raw, &fetched))
	assert.Equal(t, created.ID, fetched.ID)
}

func TestSearchFindsAFreshNote(t *testing.T) {
	// Indexing happens inside the request, so a note is findable as soon as the
	// create call returns. If it moved onto the event bus this would go red.
	srv, _ := newTestServer(t)

	_, raw := postNote(t, srv, `{"title":"Roof repair","body":"Slate tiles, south side"}`)
	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))

	resp, raw := get(t, srv, "/api/notes/search?q=tile")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result listResponse
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result.Notes, 1, "body was %s", raw)
	assert.Equal(t, created.ID, result.Notes[0].ID)
}

func TestListPagesByKeyset(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, title := range []string{"one", "two", "three", "four", "five"} {
		resp, raw := postNote(t, srv, `{"title":"`+title+`"}`)
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	}

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		path := "/api/notes?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		resp, raw := get(t, srv, path)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		var page listResponse
		require.NoError(t, json.Unmarshal(raw, &page))
		for _, note := range page.Notes {
			require.False(t, seen[note.ID], "note %s appeared on two pages", note.ID)
			seen[note.ID] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}

	assert.Len(t, seen, 5, "paging skipped or repeated rows")
	assert.Empty(t, cursor, "the last page must not offer a cursor")
}

func TestDeleteRemovesFromTheIndexToo(t *testing.T) {
	srv, _ := newTestServer(t)

	_, raw := postNote(t, srv, `{"title":"Boiler service","body":"Annual check"}`)
	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodDelete, srv.URL+"/api/notes/"+created.ID, nil)
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, _ = get(t, srv, "/api/notes/"+created.ID)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, raw = get(t, srv, "/api/notes/search?q=boiler")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result listResponse
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Empty(t, result.Notes, "the document outlived its row")
}

func TestErrorsNeverEchoTheInput(t *testing.T) {
	srv, _ := newTestServer(t)

	tests := []struct {
		name   string
		path   string
		status int
		absent string
	}{
		{
			// An id that is not a UUID and one that simply does not exist get
			// the same answer, so neither confirms which ids are well formed.
			// No slash in the value: an encoded one makes chi answer before the
			// handler runs, which would test the router rather than this code.
			name:   "an id that is not a uuid",
			path:   "/api/notes/not-a-uuid",
			status: http.StatusNotFound,
			absent: "not-a-uuid",
		},
		{
			name:   "an id carrying markup",
			path:   "/api/notes/%3Cscript%3Ealert(1)%3C%2Fscript%3E",
			status: http.StatusNotFound,
			absent: "script",
		},
		{
			name:   "a missing note",
			path:   "/api/notes/00000000-0000-0000-0000-000000000000",
			status: http.StatusNotFound,
		},
		{
			name:   "a pagination parameter this endpoint does not read",
			path:   "/api/notes?per_page=5",
			status: http.StatusBadRequest,
			absent: "per_page",
		},
		{
			name:   "page zero, since page is 1-based",
			path:   "/api/notes?page=0",
			status: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, raw := get(t, srv, tt.path)
			assert.Equal(t, tt.status, resp.StatusCode, string(raw))
			if tt.absent != "" {
				assert.NotContains(t, string(raw), tt.absent)
			}
			// Every error carries an id that leads to the log line holding the
			// detail the body deliberately omits.
			var body struct {
				Error     string `json:"error"`
				RequestID string `json:"request_id"`
			}
			require.NoError(t, json.Unmarshal(raw, &body))
			assert.NotEmpty(t, body.Error)
			assert.NotEmpty(t, body.RequestID)
		})
	}
}

func TestCreateRejectsABadBody(t *testing.T) {
	srv, _ := newTestServer(t)

	tests := []struct {
		name string
		body string
	}{
		// A valid title alongside the unknown field, or the empty-title check
		// answers first and DisallowUnknownFields goes untested.
		{"an unknown field", `{"title":"fine","tilte":"typo"}`},
		{"an empty title", `{"title":"   "}`},
		{"not json", `not json at all`},
		{"two values", `{"title":"a"}{"title":"b"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, raw := postNote(t, srv, tt.body)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
		})
	}
}

func TestHealthAndReadiness(t *testing.T) {
	srv, pool := newTestServer(t)

	resp, raw := get(t, srv, "/healthz")
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	resp, raw = get(t, srv, "/readyz")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), `"database":"ok"`)
	assert.Contains(t, string(raw), `"search":"ok"`)

	// With the database gone, readiness says so once the cached result expires.
	pool.Close()
	time.Sleep(5 * time.Millisecond)
	resp, raw = get(t, srv, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
	assert.NotContains(t, string(raw), "127.0.0.1", "a driver error names hosts")

	// Liveness does not consult the database, so it is still 200.
	resp, _ = get(t, srv, "/healthz")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestReconcileJobRepairsTheIndex(t *testing.T) {
	srv, pool := newTestServer(t)
	ctx := context.Background()

	_, raw := postNote(t, srv, `{"title":"Garden fence","body":"Replace two panels"}`)
	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))

	index := search.NewPostgresIndex(search.PostgresIndexOptions{
		Pool: pool,
		Config: search.PostgresConfig{
			Table:            searchTable,
			SearchableFields: []string{"title", "body"},
			TextSearchConfig: "english",
		},
	})
	notes := NewNotes(pool)

	// A document the table has no row for, as a failed delete would leave.
	require.NoError(t, index.IndexDocuments(ctx, []search.Document{
		search.MapDocument{"id": "00000000-0000-0000-0000-000000000000", "title": "stale"},
	}))

	outcome, err := reconcileSearchIndex(notes, index)(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, outcome.Attempted)
	assert.Equal(t, 1, outcome.Succeeded)
	assert.Equal(t, "success", outcome.Status())

	count, err := index.DocumentCount(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count, "the stale document survived the prune")
}

func TestReconcileReportsIdleOnAnEmptyTable(t *testing.T) {
	// Not "success": nothing was attempted, and a job that reports success
	// having done nothing is indistinguishable from one that worked.
	_, pool := newTestServer(t)
	ctx := context.Background()

	index := search.NewPostgresIndex(search.PostgresIndexOptions{
		Pool:   pool,
		Config: search.PostgresConfig{Table: searchTable},
	})
	outcome, err := reconcileSearchIndex(NewNotes(pool), index)(ctx)
	require.NoError(t, err)
	assert.Equal(t, "idle", outcome.Status())
	assert.True(t, outcome.OK())
}

func TestReconcileIsFatalWhenItCannotReadTheTable(t *testing.T) {
	// Pruning against a set that could not be read would delete documents whose
	// rows exist. "Could not measure" is a different claim from "measured".
	_, pool := newTestServer(t)
	index := search.NewPostgresIndex(search.PostgresIndexOptions{
		Pool:   pool,
		Config: search.PostgresConfig{Table: searchTable},
	})
	notes := NewNotes(pool)

	pool.Close()

	outcome, err := reconcileSearchIndex(notes, index)(context.Background())
	require.Error(t, err)
	assert.True(t, outcome.Fatal)
	assert.Equal(t, "failed", outcome.Status())
	assert.False(t, outcome.OK())
}
