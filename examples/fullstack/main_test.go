package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/admin"
	adminpg "github.com/ManavA/keel/admin/pg"
	"github.com/ManavA/keel/app"
	authpg "github.com/ManavA/keel/auth/pg"
	"github.com/ManavA/keel/httpx"
	idempg "github.com/ManavA/keel/idempotency/pg"
	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/outbox"
	outboxpg "github.com/ManavA/keel/outbox/pg"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

// An example nobody runs stops working quietly. These tests exercise the
// full-stack example the way its README says to, against a real database:
// a user registers, writes a note, and the outbox relay publishes exactly
// one event for it.

func TestMain(m *testing.M) {
	slog.SetDefault(keellog.New(keellog.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// recordingPublisher stands in for the message broker: the relay publishes
// into it, and the test reads back what arrived.
type recordingPublisher struct {
	mu    sync.Mutex
	calls []recordedCall
}

type recordedCall struct {
	topic string
	env   outbox.Envelope
}

func (p *recordingPublisher) Publish(_ context.Context, topic string, event any) error {
	env, ok := event.(outbox.Envelope)
	if !ok {
		return errors.New("recordingPublisher: relay must publish an outbox.Envelope")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, recordedCall{topic: topic, env: env})
	return nil
}

func (p *recordingPublisher) all() []recordedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]recordedCall, len(p.calls))
	copy(out, p.calls)
	return out
}

// testService is the example the way run() builds it — an app holding the
// pool, migrations, routes and checks, with auth and admin mounted — wrapped
// in an httptest server with its own schema so tests do not collide.
type testService struct {
	srv  *httptest.Server
	pool *pgxpool.Pool
}

func (ts *testService) close() {
	ts.srv.Close()
	ts.pool.Close()
}

// testAdminSecret signs the admin sessions in tests. It is a constant
// because every test seeds the same admin and nothing here is a secret.
const testAdminSecret = "test-admin-secret-that-is-long-enough"

// newTestServer builds the service the way run() does and returns it wrapped
// in an httptest server, with its own schema so tests do not collide.
func newTestServer(t *testing.T) *testService {
	t.Helper()

	db := testdb.Shared(t)
	ctx := context.Background()
	schema := "fullstack_" + randomSuffix(t)

	adminConn, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL})
	require.NoError(t, err)
	_, err = adminConn.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	adminConn.Close()

	// The pool only exists after Open, so the checks close over the variable
	// and read it when they run — requests and probes happen after the
	// wiring below, never while it is being registered.
	var pool *pgxpool.Pool

	a := app.New(app.Options{
		Logger:      slog.Default(),
		DatabaseURL: db.URL + "&search_path=" + schema,
		Migrations: []app.MigrationSource{
			{FS: authpg.MigrationsFS, Dir: "migrations"},
			{FS: adminpg.MigrationsFS, Dir: "migrations"},
			{FS: outboxpg.MigrationsFS, Dir: "migrations"},
			{FS: idempg.MigrationsFS, Dir: "migrations"},
			{FS: migrationFiles, Dir: "migrations"},
		},
		Checks: map[string]httpx.Check{
			"outbox":      func(ctx context.Context) error { return checkOutbox(ctx, pool) },
			"idempotency": func(ctx context.Context) error { return checkIdempotency(ctx, pool) },
		},
		// A tiny readiness cache so a test can observe a dependency going
		// away. The default is five seconds, which is right in production
		// and too slow to assert against.
		ReadinessCacheTTL: time.Millisecond,
	})
	require.NoError(t, a.Open(ctx))
	t.Cleanup(a.Close)

	pool = a.Pool()

	authSvc, err := buildAuthService(slog.Default(), pool, "http://example.com")
	require.NoError(t, err)

	adminSvc, err := buildAdminService(slog.Default(), pool, testAdminSecret)
	require.NoError(t, err)

	_, err = admin.Seed(ctx, adminpg.NewAdminStore(pool),
		"admin@example.com", "adminpassword123", "Owner", "admin")
	require.NoError(t, err)

	api := &API{notes: NewNotes(pool), auth: authSvc, idem: idempg.New(pool)}
	api.Routes(a.Router())

	// The auth and admin packages own their routes; they live under /auth
	// and /admin so the example stays one service with three concerns
	// rather than three services. chi's Mount does not strip the prefix,
	// so StripPrefix does.
	a.Router().Mount("/auth", http.StripPrefix("/auth", authSvc.Router()))
	a.Router().Mount("/admin", http.StripPrefix("/admin", adminSvc.Router()))

	srv := httptest.NewServer(a.Router())

	ts := &testService{srv: srv, pool: pool}
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
	resp, raw := post(t, ts, "/auth/signup", "", "", `{"email":"`+email+`","password":"`+testPassword+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	var session sessionToken
	require.NoError(t, json.Unmarshal(raw, &session))
	require.NotEmpty(t, session.Token)
	return session
}

// post sends a JSON request with an optional bearer token and an optional
// idempotency key.
func post(t *testing.T, ts *testService, path, token, idemKey, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.srv.URL+path, bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
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
func postNote(t *testing.T, ts *testService, token, idemKey, body string) (*http.Response, []byte) {
	t.Helper()
	return post(t, ts, "/api/notes", token, idemKey, body)
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

// TestSignupRegistersAUser proves the example wired the local auth source to
// its database: a signup returns a session whose token opens the API.
func TestSignupRegistersAUser(t *testing.T) {
	ts := newTestServer(t)
	session := signup(t, ts, "owner@example.com")
	assert.Equal(t, "owner@example.com", session.User.Email)

	resp, _ := get(t, ts, "/api/notes/"+emptyUUID, session.Token)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// emptyUUID is a well-formed id that names nothing, so the notes handler
// answers from the database rather than from input validation.
const emptyUUID = "00000000-0000-0000-0000-000000000000"

// TestCreateNotePublishesOneOutboxEvent is the acceptance check: end to end,
// a registered user writes a note and the relay publishes exactly one event
// carrying that note.
func TestCreateNotePublishesOneOutboxEvent(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "writer@example.com").Token
	ctx := context.Background()

	resp, raw := postNote(t, ts, token, "", `{"title":"Roof repair","body":"Slate tiles"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	var created Note
	require.NoError(t, json.Unmarshal(raw, &created))
	require.NotEmpty(t, created.ID)

	recorder := &recordingPublisher{}
	relay, err := outbox.NewRelay(ts.pool, outbox.Options{Publisher: recorder})
	require.NoError(t, err)

	published, err := relay.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, published)

	calls := recorder.all()
	require.Len(t, calls, 1)
	assert.Equal(t, "note.created", calls[0].topic)
	assert.NotEmpty(t, calls[0].env.ID, "the envelope must carry a dedupe id")

	var payload struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(calls[0].env.Payload, &payload))
	assert.Equal(t, created.ID, payload.ID, "the event must describe the note that was written")

	// The row is marked published, so the next tick has nothing to do.
	published, err = relay.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, published)
	assert.Len(t, recorder.all(), 1, "the event was delivered twice")
}

// TestIdempotentRetryReplaysTheCreate proves the write path is safe to
// retry: the same key and body returns the first response again instead of
// writing a second note and a second event.
func TestIdempotentRetryReplaysTheCreate(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "retrying@example.com").Token
	ctx := context.Background()

	resp, first := postNote(t, ts, token, "key-1", `{"title":"Boiler service"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(first))

	resp, second := postNote(t, ts, token, "key-1", `{"title":"Boiler service"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(second))
	assert.Equal(t, "true", resp.Header.Get("Idempotency-Replayed"))

	var firstNote, secondNote Note
	require.NoError(t, json.Unmarshal(first, &firstNote))
	require.NoError(t, json.Unmarshal(second, &secondNote))
	assert.Equal(t, firstNote.ID, secondNote.ID, "the retry wrote a second note")

	var rows int
	require.NoError(t, ts.pool.QueryRow(ctx,
		"select count(*) from outbox_events where topic = 'note.created'").Scan(&rows))
	assert.Equal(t, 1, rows, "the retry enqueued a second event")
}

// TestAdminCanLogIn proves the example mounted the admin service against its
// database: the seeded admin logs in, and a wrong password does not.
func TestAdminCanLogIn(t *testing.T) {
	ts := newTestServer(t)

	resp, raw := post(t, ts, "/admin/login", "", "",
		`{"email":"admin@example.com","password":"adminpassword123"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var session struct {
		Token string `json:"token"`
		Admin struct {
			Email string `json:"email"`
		} `json:"admin"`
	}
	require.NoError(t, json.Unmarshal(raw, &session))
	assert.NotEmpty(t, session.Token)
	assert.Equal(t, "admin@example.com", session.Admin.Email)

	resp, _ = post(t, ts, "/admin/login", "", "",
		`{"email":"admin@example.com","password":"wrongpassword123"}`)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHealthAndReadiness(t *testing.T) {
	ts := newTestServer(t)

	resp, raw := get(t, ts, "/healthz", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	resp, raw = get(t, ts, "/readyz", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), `"database":"ok"`)
	assert.Contains(t, string(raw), `"outbox":"ok"`)
	assert.Contains(t, string(raw), `"idempotency":"ok"`)

	// With the database gone, readiness says so once the cached result
	// expires. Liveness does not consult the database, so it is still 200.
	ts.pool.Close()
	time.Sleep(5 * time.Millisecond)
	resp, _ = get(t, ts, "/readyz", "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	resp, _ = get(t, ts, "/healthz", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
