package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/admin"
	"github.com/ManavA/keel/auth"
	"github.com/ManavA/keel/config"
	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/jobs"
	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(keellog.New(keellog.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// testMigrations is the smallest schema migrate.Run accepts: one file, one
// table. Every test gets its own database schema, so the table name needs no
// uniquifying.
var testMigrations = fstest.MapFS{
	"migrations/001_probe.up.sql": {Data: []byte(`create table if not exists probe (id text primary key);`)},
}

// openTestApp opens an app against the shared database in its own schema, so
// tests writing rows or ledger entries do not collide.
func openTestApp(t *testing.T, mutate func(*Options)) *App {
	t.Helper()

	db := testdb.Shared(t)
	ctx := context.Background()
	schema := "app_" + randomSuffix(t)

	adminPool, err := pg.Open(ctx, pg.Options{URL: db.URL})
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	adminPool.Close()

	opts := Options{
		Logger:      slog.Default(),
		DatabaseURL: db.URL + "&search_path=" + schema,
		Migrations:  []MigrationSource{{FS: testMigrations, Dir: "migrations"}},
	}
	if mutate != nil {
		mutate(&opts)
	}
	a := New(opts)
	require.NoError(t, a.Open(ctx))
	t.Cleanup(a.Close)
	return a
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return hex.EncodeToString(b[:])
}

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, rec.Body.Bytes()
}

func TestZeroValueStartServesHealth(t *testing.T) {
	a := openTestApp(t, nil)

	rec, _ := get(t, a.Router(), "/healthz")
	assert.Equal(t, http.StatusOK, rec.Code)

	rec, raw := get(t, a.Router(), "/readyz")
	require.Equal(t, http.StatusOK, rec.Code, string(raw))
	assert.Contains(t, string(raw), `"database":"ok"`)

	// The migration ran: the ledger and the probe table exist in this schema.
	var table string
	err := a.Pool().QueryRow(context.Background(),
		`select tablename from pg_tables where schemaname = current_schema() and tablename = 'probe'`).Scan(&table)
	require.NoError(t, err)
	assert.Equal(t, "probe", table)
}

func TestGracefulShutdown(t *testing.T) {
	a := openTestApp(t, func(o *Options) { o.Addr = "127.0.0.1:0" })
	require.NoError(t, a.Listen())
	addr := a.Addr()
	require.NotEmpty(t, addr)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	// Wait for the server to accept traffic rather than sleeping a fixed
	// amount: too short flakes, too long wastes every run.
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz") //nolint:noctx,bodyclose // a shutdown test, not a handler test
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("server never accepted traffic")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stop()
	select {
	case err := <-runErr:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestMiddlewareDefaults(t *testing.T) {
	a := openTestApp(t, func(o *Options) {
		o.Routes = func(r chi.Router) {
			r.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("ok"))
			})
			r.Get("/panic", func(w http.ResponseWriter, r *http.Request) { panic("boom") })
		}
	})

	// Every response carries a request id.
	rec, _ := get(t, a.Router(), "/ok")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, rec.Header().Get(middleware.RequestIDHeader))

	// A panic becomes a 500, not a dropped connection.
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec = httptest.NewRecorder()
	assert.NotPanics(t, func() { a.Router().ServeHTTP(rec, req) })
	assert.Equal(t, http.StatusInternalServerError, rec.Code)

	// Unknown routes and methods are JSON errors carrying the request id,
	// rather than chi's plain-text defaults.
	rec, raw := get(t, a.Router(), "/missing")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var body struct {
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.NotEmpty(t, body.Error)
	assert.NotEmpty(t, body.RequestID)

	// HEAD is answered wherever GET is.
	req = httptest.NewRequest(http.MethodHead, "/ok", nil)
	rec = httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthMountStripsItsPrefix(t *testing.T) {
	svc, err := auth.NewService(auth.Options{})
	require.NoError(t, err)

	a := openTestApp(t, func(o *Options) {
		o.Auth = Mount{Handler: svc.Router()}
	})

	// Without the strip, the auth mux would see "/auth/signup" and answer 404.
	// A short password answers 400 from the signup handler itself.
	resp, err := http.Post(
		//nolint:noctx // a mount test, not a handler test
		httptest.NewServer(a.Router()).URL+"/auth/signup",
		"application/json", strings.NewReader(`{"email":"a@example.com","password":"x"}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdminMountStripsItsPrefix(t *testing.T) {
	svc, err := admin.NewService(admin.Options{
		Users:  admin.NewMemoryAdminStore(),
		Secret: "test-secret-long-enough",
	})
	require.NoError(t, err)

	a := openTestApp(t, func(o *Options) {
		o.Admin = Mount{Handler: svc.Router()}
	})

	// An unknown admin answers 401 from the login handler, not 404 from the
	// router — the mount reached the handler.
	srv := httptest.NewServer(a.Router())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/admin/login", //nolint:noctx // a mount test, not a handler test
		"application/json", strings.NewReader(`{"email":"nobody@example.com","password":"wrong-password"}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestOpenFailsFast(t *testing.T) {
	db := testdb.Shared(t)
	ctx := context.Background()

	t.Run("missing database url", func(t *testing.T) {
		a := New(Options{Logger: slog.Default()})
		defer a.Close()
		assert.Error(t, a.Open(ctx))
	})

	t.Run("duplicate job names", func(t *testing.T) {
		a := New(Options{
			Logger:      slog.Default(),
			DatabaseURL: db.URL,
			Jobs: []jobs.Entry{
				{Name: "same", Interval: time.Minute, Func: func(context.Context) (jobs.Outcome, error) {
					return jobs.Outcome{}, nil
				}},
				{Name: "same", Interval: time.Minute, Func: func(context.Context) (jobs.Outcome, error) {
					return jobs.Outcome{}, nil
				}},
			},
		})
		defer a.Close()
		assert.ErrorContains(t, a.Open(ctx), "same")
	})

	t.Run("mount path without a leading slash", func(t *testing.T) {
		svc, err := auth.NewService(auth.Options{})
		require.NoError(t, err)
		a := New(Options{
			Logger:      slog.Default(),
			DatabaseURL: db.URL,
			Auth:        Mount{Handler: svc.Router(), Path: "auth"},
		})
		defer a.Close()
		assert.ErrorContains(t, a.Open(ctx), `must start with "/"`)
	})

	t.Run("a migration that does not apply", func(t *testing.T) {
		bad := fstest.MapFS{
			"migrations/001_bad.up.sql": {Data: []byte(`create table nope (`)},
		}
		a := New(Options{
			Logger:      slog.Default(),
			DatabaseURL: db.URL,
			Migrations:  []MigrationSource{{FS: bad, Dir: "migrations"}},
		})
		defer a.Close()
		assert.Error(t, a.Open(ctx))
		assert.Nil(t, a.Pool(), "a failed migration must not leave a pool behind")
		assert.Nil(t, a.Router(), "a failed migration must not leave a router behind")
	})
}

func TestExtraHealthCheckFailureDegradesReadiness(t *testing.T) {
	a := openTestApp(t, func(o *Options) {
		o.ReadinessCacheTTL = time.Millisecond
		o.Checks = map[string]httpx.Check{
			"downstream": func(ctx context.Context) error { return errDownstream },
		}
	})

	rec, raw := get(t, a.Router(), "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, string(raw))
	assert.Contains(t, string(raw), `"downstream":"down"`)

	rec, _ = get(t, a.Router(), "/healthz")
	assert.Equal(t, http.StatusOK, rec.Code, "liveness ignores checks")
}

var errDownstream = errDownstreamType{}

type errDownstreamType struct{}

func (errDownstreamType) Error() string { return "downstream unreachable" }

func TestConfigEmbedsAndMaps(t *testing.T) {
	t.Setenv("PORT", "8090")
	t.Setenv("ENV", "production")
	t.Setenv("DATABASE_URL", "postgres://db:5432/app?sslmode=disable")
	t.Setenv("CORS_ORIGINS", "https://app.example.com")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")

	type serviceConfig struct {
		App Config
	}

	var cfg serviceConfig
	require.NoError(t, config.Load(&cfg))
	assert.Equal(t, 8090, cfg.App.Port)
	assert.Equal(t, "postgres://db:5432/app?sslmode=disable", cfg.App.DatabaseURL)
	assert.Equal(t, []string{"https://app.example.com"}, cfg.App.CORSOrigins)
	assert.Equal(t, 10*time.Second, cfg.App.ShutdownTimeout)
	assert.Equal(t, ":8090", cfg.App.Addr())
	assert.True(t, cfg.App.Cloud())

	opts := cfg.App.Options()
	assert.Equal(t, ":8090", opts.Addr)
	assert.Equal(t, cfg.App.DatabaseURL, opts.DatabaseURL)

	assert.Error(t, serviceConfig{App: Config{Port: 0}}.App.Validate())
}
