package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/app"
	authpg "github.com/ManavA/keel/auth/pg"
	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/httpx"
	keellog "github.com/ManavA/keel/log"
	mailtesting "github.com/ManavA/keel/mail/testing"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
	"github.com/ManavA/keel/search"
	"github.com/go-chi/chi/v5"
)

// An example nobody runs stops working quietly. These tests exercise it the way
// the README says to, against a real database, so a signature drifting in one
// of the packages it wires shows up as a failure rather than as a stale file.

func TestMain(m *testing.M) {
	slog.SetDefault(keellog.New(keellog.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// testService is the example the way run() builds it — an app holding the
// pool, migrations, routes and checks, with auth mounted — wrapped in an
// httptest server with its own schema so tests do not collide. mail captures
// every verification and reset link instead of logging it, so the auth flows
// can be walked end to end without an inbox.
type testService struct {
	srv  *httptest.Server
	pool *pgxpool.Pool
	mail *mailtesting.Recorder
}

func (ts *testService) close() {
	ts.srv.Close()
	ts.pool.Close()
}

// newTestServer builds the service the way run() does and returns it wrapped in
// an httptest server, with its own schema so tests do not collide.
func newTestServer(t *testing.T) *testService {
	t.Helper()

	db := testdb.Shared(t)
	ctx := context.Background()
	schema := "example_" + randomSuffix(t)

	admin, err := pg.Open(ctx, pg.Options{URL: db.URL})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	admin.Close()

	// The same migrations run() applies, auth tables first: a drift between
	// what the binary migrates and what the tests migrate would go green here
	// and fail there.
	//
	// Routes are registered after Open, not in the options: api.Routes needs
	// the auth service for its session middleware, and the auth service needs
	// the pool, which only exists after Open.
	api := &API{}
	var index search.Index

	a := app.New(app.Options{
		Logger:      slog.Default(),
		DatabaseURL: db.URL + "&search_path=" + schema,
		Migrations: []app.MigrationSource{
			{FS: authpg.MigrationsFS, Dir: "migrations"},
			{FS: migrationFiles, Dir: "migrations"},
		},
		Checks: map[string]httpx.Check{
			"search": func(ctx context.Context) error { return index.Health(ctx) },
		},
		// A tiny readiness cache so a test can observe a dependency going away.
		// The default is five seconds, which is right in production and too slow
		// to assert against.
		ReadinessCacheTTL: time.Millisecond,
	})
	require.NoError(t, a.Open(ctx))
	t.Cleanup(a.Close)

	pool := a.Pool()
	pgIndex := search.NewPostgresIndex(search.PostgresIndexOptions{
		Pool: pool,
		Config: search.PostgresConfig{
			Table:            searchTable,
			SearchableFields: []string{"title", "body"},
			TextSearchConfig: "english",
		},
	})
	require.NoError(t, pgIndex.EnsureSchema(ctx))
	index = pgIndex

	recorder := mailtesting.New()
	// The auth rate limit is generous: production's 15 requests a minute is
	// right for a login route and wrong for a test that signs up several
	// accounts.
	cfg := Config{
		Config: app.Config{
			Port:              8080,
			Env:               "development",
			ReadinessCacheTTL: time.Millisecond,
		},
		SiteURL:               "http://example.com",
		AuthRateLimitRequests: 10000,
		AuthRateLimitWindow:   time.Minute,
	}
	authSvc, err := buildAuthService(ctx, cfg, slog.Default(), pool, recorder)
	require.NoError(t, err)

	api.notes = NewNotes(pool)
	api.index = index
	api.publisher = events.NewNoopPublisher()
	api.auth = authSvc
	api.Routes(a.Router())

	// The same UI wiring run() applies: templates parsed once, the notes UI
	// beside the JSON API, the landing shell and static assets beside those.
	// The auth router instance is shared with the form handlers the way run()
	// shares it, so the tests exercise the same rate limiter the binary runs.
	tmpl, err := ParseTemplates()
	require.NoError(t, err)
	api.tmpl = tmpl
	api.NotesUIRoutes(a.Router())
	NewUI(tmpl).Routes(a.Router())

	// The auth package owns its routes; they live under /auth the way run()
	// mounts them.
	authRoutes := authSvc.Router()
	a.Router().Mount("/auth", http.StripPrefix("/auth", authRoutes))
	api.authRoutes = authRoutes
	api.siteURL = cfg.SiteURL
	api.tokenTTL = cfg.AuthTokenTTL
	api.checks = map[string]httpx.Check{
		"database": func(ctx context.Context) error { return pool.Ping(ctx) },
		"search":   func(ctx context.Context) error { return index.Health(ctx) },
	}
	api.AuthUIRoutes(a.Router())

	srv := httptest.NewServer(a.Router())

	ts := &testService{srv: srv, pool: pool, mail: recorder}
	t.Cleanup(ts.close)
	return ts
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return hex.EncodeToString(b[:])
}

// sessionToken is the part of an auth response the tests need. The auth
// package's own tests cover the rest of the shape.
type sessionToken struct {
	Token string `json:"token"`
	User  struct {
		ID            string `json:"id"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	} `json:"user"`
}

// testPassword is the password every test account signs up with. The auth
// package's own tests cover password rules; here it is a constant because a
// parameter every caller passes the same value is a lint finding, not
// flexibility.
const testPassword = "password123"

// signup registers an account and returns its first session token.
func signup(t *testing.T, ts *testService, email string) sessionToken {
	t.Helper()
	resp, raw := post(t, ts, "/auth/signup", "", `{"email":"`+email+`","password":"`+testPassword+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	var session sessionToken
	require.NoError(t, json.Unmarshal(raw, &session))
	require.NotEmpty(t, session.Token)
	return session
}

// post sends a JSON request with an optional bearer token.
func post(t *testing.T, ts *testService, path, token, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.srv.URL+path, bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

// postNote sends a create request. Every caller uses the same path, so it is
// not a parameter.
func postNote(t *testing.T, ts *testService, token, body string) (*http.Response, []byte) {
	t.Helper()
	return post(t, ts, "/api/notes", token, body)
}

func get(t *testing.T, ts *testService, path, token string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.srv.URL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

// lastLinkToken returns the token query parameter of the most recently mailed
// link, which is how a test walks a flow a person would walk from their inbox.
func lastLinkToken(t *testing.T, ts *testService) string {
	t.Helper()
	sent := ts.mail.Sent()
	require.NotEmpty(t, sent, "no email was sent")
	model := sent[len(sent)-1].TemplateModel
	rawURL, ok := model["url"].(string)
	require.True(t, ok, "mailed model has no url, got %#v", model)
	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)
	token := parsed.Query().Get("token")
	require.NotEmpty(t, token, "mailed link has no token: %s", rawURL)
	return token
}

func TestCreateAndReadANote(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "reader@example.com").Token

	resp, raw := postNote(t, ts, token, `{"title":"Roof repair","body":"Slate tiles"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "Roof repair", created.Title)
	assert.False(t, created.CreatedAt.IsZero())

	resp, raw = get(t, ts, "/api/notes/"+created.ID, token)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var fetched Note
	require.NoError(t, json.Unmarshal(raw, &fetched))
	assert.Equal(t, created.ID, fetched.ID)
}

func TestSearchFindsAFreshNote(t *testing.T) {
	// Indexing happens inside the request, so a note is findable as soon as the
	// create call returns. If it moved onto the event bus this would go red.
	ts := newTestServer(t)
	token := signup(t, ts, "searcher@example.com").Token

	_, raw := postNote(t, ts, token, `{"title":"Roof repair","body":"Slate tiles, south side"}`)
	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))

	resp, raw := get(t, ts, "/api/notes/search?q=tile", token)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result listResponse
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result.Notes, 1, "body was %s", raw)
	assert.Equal(t, created.ID, result.Notes[0].ID)
}

func TestListPagesByKeyset(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "pager@example.com").Token

	for _, title := range []string{"one", "two", "three", "four", "five"} {
		resp, raw := postNote(t, ts, token, `{"title":"`+title+`"}`)
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	}

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		path := "/api/notes?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		resp, raw := get(t, ts, path, token)
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
	ts := newTestServer(t)
	token := signup(t, ts, "deleter@example.com").Token

	_, raw := postNote(t, ts, token, `{"title":"Boiler service","body":"Annual check"}`)
	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodDelete, ts.srv.URL+"/api/notes/"+created.ID, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := ts.srv.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, _ = get(t, ts, "/api/notes/"+created.ID, token)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, raw = get(t, ts, "/api/notes/search?q=boiler", token)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result listResponse
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Empty(t, result.Notes, "the document outlived its row")
}

func TestErrorsNeverEchoTheInput(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "careful@example.com").Token

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
			resp, raw := get(t, ts, tt.path, token)
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
	ts := newTestServer(t)
	token := signup(t, ts, "particular@example.com").Token

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
			resp, raw := postNote(t, ts, token, tt.body)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
		})
	}
}

func TestNotesRequireAuth(t *testing.T) {
	ts := newTestServer(t)

	resp, _ := postNote(t, ts, "", `{"title":"no token"}`)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, _ = get(t, ts, "/api/notes", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, _ = get(t, ts, "/api/notes/search?q=tile", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, _ = get(t, ts, "/api/notes/00000000-0000-0000-0000-000000000000", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodDelete, ts.srv.URL+"/api/notes/00000000-0000-0000-0000-000000000000", nil)
	require.NoError(t, err)
	resp, err = ts.srv.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// A forged token is the same 401, not a different one: the failure must
	// not say which half of the credential was wrong.
	resp, _ = get(t, ts, "/api/notes", "not-a-real-token")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestAuthFlowEndToEnd walks the local auth flow the way a person would: sign
// up, verify from the mailed link, log in, use the session, log out, reset
// the password from the next mailed link, and log in again. The auth
// package's own tests cover each endpoint's edge cases; this one proves the
// example wired them to a database and to mail.
func TestAuthFlowEndToEnd(t *testing.T) {
	ts := newTestServer(t)

	session := signup(t, ts, "owner@example.com")
	assert.False(t, session.User.EmailVerified, "a fresh signup must not be verified")

	// The signup mail carries the verification link; following it verifies.
	verifyToken := lastLinkToken(t, ts)
	resp, raw := post(t, ts, "/auth/verify-email", "", `{"token":"`+verifyToken+`"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	// Login now reports the account verified, and the session works.
	resp, raw = post(t, ts, "/auth/login", "", `{"email":"owner@example.com","password":"password123"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var login sessionToken
	require.NoError(t, json.Unmarshal(raw, &login))
	assert.True(t, login.User.EmailVerified)

	resp, raw = postNote(t, ts, login.Token, `{"title":"Roof repair","body":"Slate tiles"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	// Refresh hands back a working token for the same account.
	resp, raw = post(t, ts, "/auth/refresh", login.Token, `{}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var refreshed sessionToken
	require.NoError(t, json.Unmarshal(raw, &refreshed))
	assert.Equal(t, login.User.ID, refreshed.User.ID)
	resp, _ = get(t, ts, "/api/notes", refreshed.Token)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Logout ends the session: the token stops working.
	resp, raw = post(t, ts, "/auth/logout", login.Token, `{}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	resp, _ = get(t, ts, "/api/notes", login.Token)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a logged-out token kept working")

	// Forgot password always answers the same 200; the mailed link is what
	// does the work.
	resp, _ = post(t, ts, "/auth/forgot-password", "", `{"email":"owner@example.com"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resetToken := lastLinkToken(t, ts)
	resp, raw = post(t, ts, "/auth/reset-password", "", `{"token":"`+resetToken+`","new_password":"newpassword123"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	// The old password is dead and the new one works.
	resp, _ = post(t, ts, "/auth/login", "", `{"email":"owner@example.com","password":"password123"}`)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp, raw = post(t, ts, "/auth/login", "", `{"email":"owner@example.com","password":"newpassword123"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var relogin sessionToken
	require.NoError(t, json.Unmarshal(raw, &relogin))
	resp, _ = get(t, ts, "/api/notes", relogin.Token)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestNotesAreScopedToOwner creates a note as one account and proves a second
// account cannot read, list, search or delete it — each answer is the same
// 404 or empty result the second account gets for rows that do not exist.
func TestNotesAreScopedToOwner(t *testing.T) {
	ts := newTestServer(t)
	tokenA := signup(t, ts, "alice@example.com").Token
	tokenB := signup(t, ts, "bob@example.com").Token

	_, raw := postNote(t, ts, tokenA, `{"title":"Alice roof","body":"Slate tiles, south side"}`)
	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))

	resp, _ := get(t, ts, "/api/notes/"+created.ID, tokenB)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, raw = get(t, ts, "/api/notes", tokenB)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var listing listResponse
	require.NoError(t, json.Unmarshal(raw, &listing))
	assert.Empty(t, listing.Notes)

	resp, raw = get(t, ts, "/api/notes/search?q=slate", tokenB)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var result listResponse
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Empty(t, result.Notes, "another account's note matched the search")

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodDelete, ts.srv.URL+"/api/notes/"+created.ID, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	resp, err = ts.srv.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// And the owner's copy survived all of that.
	resp, _ = get(t, ts, "/api/notes/"+created.ID, tokenA)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHealthAndReadiness(t *testing.T) {
	ts := newTestServer(t)

	resp, raw := get(t, ts, "/healthz", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	resp, raw = get(t, ts, "/readyz", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), `"database":"ok"`)
	assert.Contains(t, string(raw), `"search":"ok"`)

	// With the database gone, readiness says so once the cached result expires.
	ts.pool.Close()
	time.Sleep(5 * time.Millisecond)
	resp, raw = get(t, ts, "/readyz", "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
	assert.NotContains(t, string(raw), "127.0.0.1", "a driver error names hosts")

	// Liveness does not consult the database, so it is still 200.
	resp, _ = get(t, ts, "/healthz", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestReconcileJobRepairsTheIndex(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "gardener@example.com").Token
	ctx := context.Background()

	_, raw := postNote(t, ts, token, `{"title":"Garden fence","body":"Replace two panels"}`)
	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))

	index := search.NewPostgresIndex(search.PostgresIndexOptions{
		Pool: ts.pool,
		Config: search.PostgresConfig{
			Table:            searchTable,
			SearchableFields: []string{"title", "body"},
			TextSearchConfig: "english",
		},
	})
	notes := NewNotes(ts.pool)

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
	ts := newTestServer(t)
	ctx := context.Background()

	index := search.NewPostgresIndex(search.PostgresIndexOptions{
		Pool:   ts.pool,
		Config: search.PostgresConfig{Table: searchTable},
	})
	outcome, err := reconcileSearchIndex(NewNotes(ts.pool), index)(ctx)
	require.NoError(t, err)
	assert.Equal(t, "idle", outcome.Status())
	assert.True(t, outcome.OK())
}

func TestReconcileIsFatalWhenItCannotReadTheTable(t *testing.T) {
	// Pruning against a set that could not be read would delete documents whose
	// rows exist. "Could not measure" is a different claim from "measured".
	ts := newTestServer(t)
	index := search.NewPostgresIndex(search.PostgresIndexOptions{
		Pool:   ts.pool,
		Config: search.PostgresConfig{Table: searchTable},
	})
	notes := NewNotes(ts.pool)

	ts.pool.Close()

	outcome, err := reconcileSearchIndex(notes, index)(context.Background())
	require.Error(t, err)
	assert.True(t, outcome.Fatal)
	assert.Equal(t, "failed", outcome.Status())
	assert.False(t, outcome.OK())
}

// TestAuthSourcesRefuseTheUnconfigured makes sure a deployment written against
// AUTH_SOURCES fails at startup when the matching settings are missing, rather
// than serving an exchange endpoint with no verifier behind it.
func TestAuthSourcesRefuseTheUnconfigured(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
	}{
		{"firebase without a project", Config{AuthSources: []string{"firebase"}, Config: app.Config{Port: 8080}, SiteURL: "http://example.com"}},
		{"oidc without an issuer", Config{AuthSources: []string{"oidc"}, OIDCAudience: "aud", Config: app.Config{Port: 8080}, SiteURL: "http://example.com"}},
		{"oidc without an audience", Config{AuthSources: []string{"oidc"}, OIDCIssuerURL: "https://issuer.example.com", Config: app.Config{Port: 8080}, SiteURL: "http://example.com"}},
		{"firebase and oidc together", Config{
			AuthSources:       []string{"firebase", "oidc"},
			FirebaseProjectID: "proj",
			OIDCIssuerURL:     "https://issuer.example.com",
			OIDCAudience:      "aud",
			Config:            app.Config{Port: 8080},
			SiteURL:           "http://example.com",
		}},
		{"an unknown source", Config{AuthSources: []string{"magic"}, Config: app.Config{Port: 8080}, SiteURL: "http://example.com"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, tt.cfg.Validate())
		})
	}

	valid := Config{AuthSources: []string{"local"}, SiteURL: "http://example.com", Config: app.Config{Port: 8080}}
	assert.NoError(t, valid.Validate())
}

// TestBuildAuthServiceNeedsItsSettings proves the wiring refuses the same bad
// configurations even when Validate was skipped: buildAuthService is what
// run() actually calls.
func TestBuildAuthServiceNeedsItsSettings(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()

	_, err := buildAuthService(ctx,
		Config{AuthSources: []string{"firebase"}, SiteURL: "http://example.com"},
		slog.Default(), ts.pool, ts.mail)
	assert.Error(t, err, "firebase without a project must not build")

	_, err = buildAuthService(ctx,
		Config{AuthSources: []string{"oidc"}, SiteURL: "http://example.com"},
		slog.Default(), ts.pool, ts.mail)
	assert.Error(t, err, "oidc without issuer and audience must not build")

	_, err = buildAuthService(ctx,
		Config{
			AuthSources:       []string{"firebase", "oidc"},
			FirebaseProjectID: "proj",
			OIDCIssuerURL:     "https://issuer.example.com",
			OIDCAudience:      "aud",
			SiteURL:           "http://example.com",
		},
		slog.Default(), ts.pool, ts.mail)
	assert.ErrorContains(t, err, "together", "firebase and oidc together must not build")
}

// newUIServer mounts only the UI routes, so the boot and fragment paths run
// without a database.
func newUIServer(t *testing.T) *httptest.Server {
	t.Helper()
	tmpl, err := ParseTemplates()
	require.NoError(t, err)
	ui := NewUI(tmpl)
	r := chi.NewRouter()
	ui.Routes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// TestLandingServesBootPage proves the service boots into a page: GET /
// answers 200 in the landing theme, pulling the token stylesheet and htmx.
func TestLandingServesBootPage(t *testing.T) {
	srv := newUIServer(t)

	resp, err := srv.Client().Get(srv.URL + "/") //nolint:gosec // G107: test reads its own server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	body := string(raw)
	assert.Contains(t, body, `data-theme="landing"`)
	assert.Contains(t, body, "/static/css/tokens.css")
	assert.Contains(t, body, "/static/css/layout.css")
	assert.Contains(t, body, "/static/vendor/htmx.min.js")
}

// TestLandingFragmentViaHXRequest proves a block renders standalone over
// HTTP: an htmx request for the boot page gets the main block only, without
// the document around it. Fragments are blocks, not second copies.
func TestLandingFragmentViaHXRequest(t *testing.T) {
	srv := newUIServer(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("HX-Request", "true")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	body := string(raw)
	assert.NotContains(t, body, "<html")
	assert.Contains(t, body, "landing-main")
}
