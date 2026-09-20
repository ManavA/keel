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
// tests writing rows or ledger entries do not collide. It sets only
// DatabaseURL, the one documented-required field, so every default stays
// under test; a test needing anything else says so through mutate.
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
		DatabaseURL: db.URL + "&search_path=" + schema,
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
	// Only DatabaseURL is set, by the helper: this is the documented zero
	// value, a working Postgres-only service.
	a := openTestApp(t, nil)

	rec, _ := get(t, a.Router(), "/healthz")
	assert.Equal(t, http.StatusOK, rec.Code)

	rec, raw := get(t, a.Router(), "/readyz")
	require.Equal(t, http.StatusOK, rec.Code, string(raw))
	assert.Contains(t, string(raw), `"database":"ok"`)
}

func TestMigrationsApplyBeforeServing(t *testing.T) {
	a := openTestApp(t, func(o *Options) {
		o.Migrations = []MigrationSource{{FS: testMigrations, Dir: "migrations"}}
	})

	// The migration ran: the ledger and the probe table exist in this schema.
	var table string
	err := a.Pool().QueryRow(context.Background(),
		`select tablename from pg_tables where schemaname = current_schema() and tablename = 'probe'`).Scan(&table)
	require.NoError(t, err)
	assert.Equal(t, "probe", table)
}

func TestDefaultAddrIs8080(t *testing.T) {
	// Empty Addr binds every interface on 8080: binding 127.0.0.1 inside a
	// container is unreachable from outside it, so the default must stay the
	// bare port. A unit test rather than a bind, so it holds on a machine
	// with 8080 already taken.
	assert.Equal(t, ":8080", defaultAddr(""))
	assert.Equal(t, "127.0.0.1:0", defaultAddr("127.0.0.1:0"), "an explicit Addr passes through")
}

func TestCloseReleasesThePool(t *testing.T) {
	db := testdb.Shared(t)
	ctx := context.Background()

	a := New(Options{DatabaseURL: db.URL})
	require.NoError(t, a.Open(ctx))
	pool := a.Pool()
	require.NotNil(t, pool)

	a.Close()
	assert.Nil(t, a.Pool(), "Close must release the pool")
	_, err := pool.Acquire(ctx)
	assert.Error(t, err, "Close must close the pool, not just forget it")

	// Safe before Open and more than once.
	assert.NotPanics(t, func() {
		New(Options{}).Close()
		a.Close()
	})
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

func TestJobsRunAndDrainOnShutdown(t *testing.T) {
	// A job in flight when the context is cancelled must finish before Run
	// returns: Close releases the pool, and returning early would pull the
	// pool out from under the running job.
	started := make(chan struct{})
	finished := make(chan struct{}, 1)
	a := openTestApp(t, func(o *Options) {
		o.Addr = "127.0.0.1:0"
		o.ShutdownTimeout = 10 * time.Second
		o.Jobs = []jobs.Entry{{
			Name:     "drain",
			Interval: time.Hour, // runs once immediately, then never again
			Func: func(context.Context) (jobs.Outcome, error) {
				close(started)
				// Longer than the shutdown sequence takes: without the
				// drain wait, Run returns while this is still running.
				time.Sleep(time.Second)
				finished <- struct{}{}
				return jobs.Outcome{Attempted: 1, Succeeded: 1}, nil
			},
		}}
	})
	require.NoError(t, a.Listen())
	addr := a.Addr()
	require.NotEmpty(t, addr)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

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

	// The scheduler runs each entry once immediately, so by now the job is
	// inside its sleep.
	<-started
	stop()
	select {
	case err := <-runErr:
		assert.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	select {
	case <-finished:
	default:
		t.Fatal("Run returned before the in-flight job finished")
	}
}

func TestRequestLogSkipDefaultAndOverride(t *testing.T) {
	a := New(Options{})
	skip := a.requestLogSkip()
	require.NotNil(t, skip, "the default must exist: probers poll continuously")

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		assert.True(t, skip(req), "%s must stay out of the request log", path)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/notes", nil)
	assert.False(t, skip(req), "anything else must still be logged")

	custom := func(*http.Request) bool { return true }
	a = New(Options{RequestLogSkip: custom})
	assert.True(t, a.requestLogSkip()(req), "an explicit predicate replaces the default")
}

func TestCORSDisabledByDefault(t *testing.T) {
	assert.Nil(t, corsOptions(nil), "no origins must not reach go-chi/cors, which reads empty as every origin")
	assert.Nil(t, corsOptions([]string{}))

	opts := corsOptions([]string{"https://app.example.com"})
	require.NotNil(t, opts)
	assert.Equal(t, []string{"https://app.example.com"}, opts.AllowedOrigins)
}

func TestReadinessCheckErrorVisibility(t *testing.T) {
	// A driver error names the host and database, and readiness is reachable
	// from wherever the service is — so the error text stays out unless the
	// caller opts in.
	t.Run("hidden by default", func(t *testing.T) {
		a := openTestApp(t, func(o *Options) {
			o.ReadinessCacheTTL = time.Millisecond
			o.Checks = map[string]httpx.Check{
				"downstream": func(ctx context.Context) error { return errDownstream },
			}
		})
		rec, raw := get(t, a.Router(), "/readyz")
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, string(raw))
		assert.NotContains(t, string(raw), "downstream unreachable")
	})

	t.Run("exposed when asked", func(t *testing.T) {
		a := openTestApp(t, func(o *Options) {
			o.ReadinessCacheTTL = time.Millisecond
			o.ExposeCheckErrors = true
			o.Checks = map[string]httpx.Check{
				"downstream": func(ctx context.Context) error { return errDownstream },
			}
		})
		rec, raw := get(t, a.Router(), "/readyz")
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, string(raw))
		assert.Contains(t, string(raw), "downstream unreachable")
	})
}

func TestCustomMountPaths(t *testing.T) {
	svc, err := auth.NewService(auth.Options{})
	require.NoError(t, err)
	adminSvc, err := admin.NewService(admin.Options{
		Users:  admin.NewMemoryAdminStore(),
		Secret: "test-secret-long-enough",
	})
	require.NoError(t, err)

	a := openTestApp(t, func(o *Options) {
		o.Auth = Mount{Handler: svc.Router(), Path: "/identity"}
		o.Admin = Mount{Handler: adminSvc.Router(), Path: "/ops"}
	})
	srv := httptest.NewServer(a.Router())
	defer srv.Close()

	// The custom paths reach the handlers: a short password answers 400 from
	// signup itself, an unknown admin 401 from login.
	resp, err := http.Post(srv.URL+"/identity/signup", //nolint:noctx // a mount test, not a handler test
		"application/json", strings.NewReader(`{"email":"a@example.com","password":"x"}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp2, err := http.Post(srv.URL+"/ops/login", //nolint:noctx // a mount test, not a handler test
		"application/json", strings.NewReader(`{"email":"nobody@example.com","password":"wrong-password"}`))
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp2.StatusCode)

	// The defaults serve nothing now.
	resp3, err := http.Post(srv.URL+"/auth/signup", //nolint:noctx // a mount test, not a handler test
		"application/json", strings.NewReader(`{"email":"a@example.com","password":"x"}`))
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp3.StatusCode)
}

func TestRoutesRegisterAfterMounts(t *testing.T) {
	// Open registers health, then the mounts, then Routes: everything below
	// must serve at once, and an unknown path must answer with the router's
	// JSON 404 rather than a mount's plain-text one.
	svc, err := auth.NewService(auth.Options{})
	require.NoError(t, err)

	a := openTestApp(t, func(o *Options) {
		o.Auth = Mount{Handler: svc.Router()}
		o.Routes = func(r chi.Router) {
			r.Get("/api/hello", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("hello"))
			})
		}
	})

	rec, _ := get(t, a.Router(), "/healthz")
	assert.Equal(t, http.StatusOK, rec.Code)

	rec, _ = get(t, a.Router(), "/api/hello")
	assert.Equal(t, http.StatusOK, rec.Code)

	srv := httptest.NewServer(a.Router())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/auth/signup", //nolint:noctx // a mount test, not a handler test
		"application/json", strings.NewReader(`{"email":"a@example.com","password":"x"}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	rec, raw := get(t, a.Router(), "/missing")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, string(raw), "request_id")
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

func waitForServer(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz") //nolint:noctx,bodyclose // a shutdown test, not a handler test
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("server never accepted traffic")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestShutdownReportsCleanWhenJobsDrain(t *testing.T) {
	a := openTestApp(t, func(o *Options) {
		o.Addr = "127.0.0.1:0"
		o.ShutdownTimeout = 10 * time.Second
		o.Jobs = []jobs.Entry{{
			Name:     "quick",
			Interval: time.Hour,
			Func: func(context.Context) (jobs.Outcome, error) {
				return jobs.Outcome{Attempted: 1, Succeeded: 1}, nil
			},
		}}
	})
	require.NoError(t, a.Listen())

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	waitForServer(t, a.Addr())

	stop()
	select {
	case err := <-runErr:
		assert.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	assert.Equal(t, ShutdownClean, a.LastShutdown().Stage)
}

func TestShutdownReportsJobsStageOnTimeout(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	a := openTestApp(t, func(o *Options) {
		o.Addr = "127.0.0.1:0"
		o.ShutdownTimeout = 300 * time.Millisecond
		o.Jobs = []jobs.Entry{{
			Name:     "slow",
			Interval: time.Hour,
			Func: func(context.Context) (jobs.Outcome, error) {
				close(started)
				select {
				case <-release:
				case <-time.After(10 * time.Second):
				}
				return jobs.Outcome{Attempted: 1, Succeeded: 1}, nil
			},
		}}
	})
	require.NoError(t, a.Listen())

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	waitForServer(t, a.Addr())

	// The scheduler runs each entry once immediately, so by now the job is
	// inside its select.
	<-started
	stop()
	start := time.Now()
	select {
	case <-runErr:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	elapsed := time.Since(start)
	close(release)

	assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond, "shutdown must wait for the job up to the timeout, not return at once")
	assert.Less(t, elapsed, 10*time.Second, "shutdown must give up at the timeout, not wait for the slow job")
	report := a.LastShutdown()
	assert.Equal(t, ShutdownJobs, report.Stage, "the report must name the stage that timed out")
}
