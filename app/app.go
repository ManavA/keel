package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/jobs"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
)

// Config holds the lifecycle-owned settings: the fields every service needs
// whatever its domain is. Embed it in the service's own config struct and load
// both with one call:
//
//	type Config struct {
//		app.Config
//		// domain fields follow
//	}
//
//	var cfg Config
//	if err := config.Load(&cfg); err != nil { ... }
//
// Anonymous embedding matters: envconfig reads the tags below as PORT,
// DATABASE_URL and the rest. A named field would prefix them instead.
type Config struct {
	Port int    `envconfig:"PORT" default:"8080"`
	Env  string `envconfig:"ENV" default:"development"`

	DatabaseURL string `envconfig:"DATABASE_URL" required:"true"`

	TrustedProxies []string `envconfig:"TRUSTED_PROXIES"`
	CORSOrigins    []string `envconfig:"CORS_ORIGINS"`

	// Keep under the platform's own termination grace period, or the platform
	// kills the shutdown partway through.
	ShutdownTimeout time.Duration `envconfig:"SHUTDOWN_TIMEOUT" default:"20s"`

	// Every prober and load balancer polls on its own schedule, and without a
	// cache all of it reaches the database.
	ReadinessCacheTTL time.Duration `envconfig:"READINESS_CACHE_TTL" default:"5s"`
}

// Validate stops a configuration this build cannot honour at startup rather
// than at the first request that needs it.
func (c Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535, got %d", c.Port)
	}
	return nil
}

// Addr binds every interface: binding 127.0.0.1 inside a container makes the
// service unreachable from outside it, with nothing in the logs to say why.
func (c Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }

// Cloud reports whether the service runs outside development, for the logger:
// Cloud Logging reads a "severity" field and ignores slog's "level", so
// without it every entry outside a laptop is DEFAULT severity.
func (c Config) Cloud() bool { return strings.TrimSpace(c.Env) != "development" }

// Options carries these settings into an app.Options, leaving the behaviour
// — routes, migrations, checks, jobs, mounts — to set explicitly.
func (c Config) Options() Options {
	return Options{
		Addr:              c.Addr(),
		DatabaseURL:       c.DatabaseURL,
		TrustedProxies:    c.TrustedProxies,
		CORSOrigins:       c.CORSOrigins,
		ShutdownTimeout:   c.ShutdownTimeout,
		ReadinessCacheTTL: c.ReadinessCacheTTL,
	}
}

// MigrationSource is one directory of migration files to apply at startup,
// for example the service's own embedded migrations or auth/pg's:
//
//	Migrations: []app.MigrationSource{
//		{FS: myMigrations, Dir: "migrations"},
//		{FS: authpg.MigrationsFS, Dir: "migrations"},
//	}
//
// Every source shares one ledger, so a file name must be unique across
// sources: a collision reads as already applied, or as checksum drift when
// the contents differ. Prefix per package, as auth/pg's 0001_auth_users does.
type MigrationSource struct {
	// FS holds the migration files; Dir is the path inside it.
	FS  fs.FS
	Dir string
}

// Mount attaches a handler built elsewhere — typically an auth or admin
// service's Router — under Path. A nil Handler disables the mount.
type Mount struct {
	Handler http.Handler
	// Path defaults to "/auth" for Options.Auth and "/admin" for
	// Options.Admin.
	Path string
}

// Options configures an App. The zero value is a working Postgres-only
// service: only Options.DB.URL is required, at Open rather than at New.
type Options struct {
	// Logger defaults to slog.Default, and is passed to the pool, the router
	// middleware and the scheduler.
	Logger *slog.Logger

	// DB configures the pool. DB.URL is required; DB.Logger defaults to
	// Logger above.
	DB pg.Options

	// DatabaseURL is shorthand for DB.URL. Set one or the other; when both
	// are set, DB.URL wins. It exists so the common case reads as one line.
	DatabaseURL string

	// Migrations apply in order at Open, before the server accepts traffic.
	// Empty means no migrations, which is how a deployment that migrates as
	// its own step says so: it runs migrate.Run against the same sources
	// outside the app, sharing the ledger.
	Migrations []MigrationSource

	// Addr is the listen address, default ":8080".
	Addr string

	// ShutdownTimeout bounds graceful shutdown. Zero takes httpx's default.
	ShutdownTimeout time.Duration

	// TrustedProxies configures client-address recovery. Empty ignores
	// forwarding headers entirely, which is correct when nothing is in front
	// of the service.
	TrustedProxies []string

	// CORSOrigins are the exact origins browsers may call from. Empty allows
	// no origin, which is correct for an API no browser calls.
	CORSOrigins []string

	// RequestLogSkip leaves matching requests out of the request log. Nil
	// skips the health endpoints, which probers poll continuously.
	RequestLogSkip func(*http.Request) bool

	// RateLimit applies one limit to every route. Nil means none: the tight
	// limits (login, signup, password reset) belong on their own routes, and
	// the auth and admin services already apply theirs.
	RateLimit *middleware.RateLimitOptions

	// RequestTimeout bounds a request. Zero takes httpx's 30 second default;
	// negative disables it, for streaming responses and uploads.
	RequestTimeout time.Duration

	// Checks are extra readiness checks alongside "database", which the app
	// adds itself. A "database" entry here overrides the default.
	Checks map[string]httpx.Check

	// ReadinessCacheTTL caches the readiness result. Zero takes httpx's
	// 5 second default.
	ReadinessCacheTTL time.Duration

	// ExposeCheckErrors puts a failing check's error text into the readiness
	// response. Off by default: a driver error names the host and database.
	ExposeCheckErrors bool

	// Auth and Admin mount those services' routers, defaulting to "/auth"
	// and "/admin". A nil Handler leaves the mount out.
	Auth  Mount
	Admin Mount

	// Routes registers the service's own API, after health and the mounts.
	Routes func(r chi.Router)

	// Jobs run on the in-process scheduler while the server serves. A
	// duplicate name fails Open, so two entries never share a log identity.
	Jobs []jobs.Entry
}

// App is a wired service: pool, router, server and scheduler built from one
// Options. Build it with New, start it with Open or Run.
type App struct {
	opts     Options
	logger   *slog.Logger
	pool     *pgxpool.Pool
	router   *chi.Mux
	server   *httpx.Server
	schedule *jobs.Scheduler
	haveJobs bool
	opened   bool

	mu           sync.Mutex
	lastShutdown ShutdownReport
}

// ShutdownStage names where Run's staged shutdown stopped: the server stage
// stops accepting traffic and waits for in-flight requests, then the jobs
// stage signals the scheduler to stop and waits for the running jobs.
type ShutdownStage string

const (
	// ShutdownClean means every stage finished inside the timeout.
	ShutdownClean ShutdownStage = "clean"
	// ShutdownServer means the server stage returned an error, usually its
	// shutdown timeout with requests still in flight.
	ShutdownServer ShutdownStage = "server"
	// ShutdownJobs means the jobs stage timed out with a job still running.
	// The pool is closed after Run returns, so that job loses the pool.
	ShutdownJobs ShutdownStage = "jobs"
)

// ShutdownReport says how the most recent Run shutdown finished. Stage is
// clean, or the stage that timed out. Err carries the server stage's error
// when it failed; a jobs timeout leaves Err as the server stage left it,
// usually nil, because the stuck job reports nothing to wrap.
type ShutdownReport struct {
	Stage ShutdownStage
	Err   error
}

// LastShutdown reports how the most recent Run shutdown finished. Before any
// Run it is the zero report, with an empty stage.
func (a *App) LastShutdown() ShutdownReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastShutdown
}

func (a *App) setLastShutdown(r ShutdownReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastShutdown = r
}

// New stores opts and fills in the logger. It performs no I/O; Open does.
func New(opts Options) *App {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &App{opts: opts, logger: logger}
}

// Logger returns the logger this app logs through.
func (a *App) Logger() *slog.Logger { return a.logger }

// Pool returns the pool Opened, or nil before Open.
func (a *App) Pool() *pgxpool.Pool { return a.pool }

// Router returns the router Open built, or nil before Open.
func (a *App) Router() *chi.Mux { return a.router }

// Listen binds Addr without serving. Run calls it when needed; call it
// yourself when you need Addr before the server starts, such as a test
// building a URL against port 0. It requires Open first.
func (a *App) Listen() error {
	if !a.opened {
		return fmt.Errorf("app: Listen before Open")
	}
	return a.server.Listen()
}

// Addr is the address actually bound, or "" before Listen.
func (a *App) Addr() string {
	if !a.opened {
		return ""
	}
	return a.server.Addr()
}

// Open connects the pool, applies migrations, and builds the router, server
// and scheduler. It is idempotent: Run calls it, so a main that only serves
// never calls it directly, while a test calls it to reach the router without
// binding a port.
func (a *App) Open(ctx context.Context) error {
	if a.opened {
		return nil
	}

	dbOpts := a.opts.DB
	if dbOpts.URL == "" {
		dbOpts.URL = a.opts.DatabaseURL
	}
	if dbOpts.Logger == nil {
		dbOpts.Logger = a.logger
	}
	pool, err := pg.Open(ctx, dbOpts)
	if err != nil {
		return err
	}
	a.pool = pool
	// A failure below must not leave a pool without a router or the reverse.
	succeeded := false
	defer func() {
		if !succeeded {
			pool.Close()
			a.pool = nil
			a.router = nil
		}
	}()

	for i, src := range a.opts.Migrations {
		_, err := migrate.Run(ctx, pool, migrate.Options{
			FS:     src.FS,
			Dir:    src.Dir,
			Logger: a.logger,
		})
		if err != nil {
			return fmt.Errorf("app: migrations source %d: %w", i, err)
		}
	}

	r := httpx.NewRouter(httpx.RouterOptions{
		Logger:     a.logger,
		RealIP:     middleware.RealIPOptions{TrustedProxies: a.opts.TrustedProxies},
		RequestLog: middleware.RequestLogOptions{Logger: a.logger, Skip: a.requestLogSkip()},
		RateLimit:  a.opts.RateLimit,
		Timeout:    a.opts.RequestTimeout,
		CORS:       corsOptions(a.opts.CORSOrigins),
	})
	a.router = r

	checks := map[string]httpx.Check{"database": pg.HealthCheck(pool)}
	for name, check := range a.opts.Checks {
		checks[name] = check
	}
	health := httpx.Health(httpx.HealthOptions{
		Logger:   a.logger,
		CacheTTL: a.opts.ReadinessCacheTTL,
		Checks:   checks,
		// The errors name hosts and databases, and readiness is reachable
		// from wherever the service is.
		ExposeCheckErrors: a.opts.ExposeCheckErrors,
	})
	// Exact routes, not a Mount at "/": a mount is a wildcard, so an unknown
	// path would fall through to the health mux's plain-text 404 instead of
	// the router's JSON one.
	r.Get("/healthz", health.ServeHTTP)
	r.Get("/readyz", health.ServeHTTP)

	if err := mount(r, a.opts.Auth, "/auth"); err != nil {
		return err
	}
	if err := mount(r, a.opts.Admin, "/admin"); err != nil {
		return err
	}

	if a.opts.Routes != nil {
		a.opts.Routes(r)
	}

	scheduler := jobs.NewScheduler(jobs.SchedulerOptions{Logger: a.logger})
	for _, e := range a.opts.Jobs {
		if err := scheduler.Register(e); err != nil {
			return fmt.Errorf("app: register job: %w", err)
		}
	}
	a.schedule = scheduler
	a.haveJobs = len(a.opts.Jobs) > 0

	addr := defaultAddr(a.opts.Addr)
	a.server = httpx.NewServer(httpx.ServerOptions{
		Addr:            addr,
		Handler:         r,
		ShutdownTimeout: a.opts.ShutdownTimeout,
		Logger:          a.logger,
	})

	a.opened = true
	succeeded = true
	return nil
}

// Run opens the app if needed, then serves HTTP and runs the jobs until ctx
// is cancelled, shutting down in stages: the server stops accepting traffic
// and waits for in-flight requests, then the scheduler is signalled to stop
// and the running jobs are waited for up to ShutdownTimeout. It returns nil
// on a clean shutdown, and the stage that timed out is logged and kept on
// LastShutdown. Run does not close the pool: call Close after Run returns,
// once the jobs it waited for no longer need it. Signal handling stays in
// main, where it can be seen:
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//	defer stop()
//	if err := app.Run(ctx); err != nil { ... }
func (a *App) Run(ctx context.Context) error {
	if err := a.Open(ctx); err != nil {
		return err
	}
	if !a.haveJobs {
		err := a.server.ListenAndServe(ctx)
		if err != nil {
			a.logger.Warn("shutdown timed out waiting for the server", "stage", ShutdownServer, "error", err)
			a.setLastShutdown(ShutdownReport{Stage: ShutdownServer, Err: err})
			return err
		}
		a.setLastShutdown(ShutdownReport{Stage: ShutdownClean})
		return nil
	}
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		a.schedule.Run(ctx)
	}()
	err := a.server.ListenAndServe(ctx)
	if err != nil {
		a.logger.Warn("shutdown timed out waiting for the server", "stage", ShutdownServer, "error", err)
	}
	// The scheduler stops when ctx is cancelled, but a job already running
	// keeps going until it finishes. Wait for it, so Close does not release
	// the pool from under a running job — bounded by ShutdownTimeout, so a
	// hung job still cannot hold shutdown open past the platform's grace
	// period.
	timeout := a.opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = httpx.DefaultShutdownTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-schedDone:
		if err != nil {
			a.setLastShutdown(ShutdownReport{Stage: ShutdownServer, Err: err})
			return err
		}
		a.setLastShutdown(ShutdownReport{Stage: ShutdownClean})
		return nil
	case <-timer.C:
		a.logger.Warn("shutdown timed out waiting for jobs, continuing without them", "stage", ShutdownJobs)
		a.setLastShutdown(ShutdownReport{Stage: ShutdownJobs, Err: err})
		return err
	}
}

// Close releases the pool. Safe to call before Open and more than once.
func (a *App) Close() {
	if a.pool != nil {
		a.pool.Close()
		a.pool = nil
	}
	a.opened = false
}

// requestLogSkip is the caller's predicate, or the default: health endpoints
// are polled continuously and would bury everything else.
func (a *App) requestLogSkip() func(*http.Request) bool {
	if a.opts.RequestLogSkip != nil {
		return a.opts.RequestLogSkip
	}
	return func(r *http.Request) bool {
		return r.URL.Path == "/healthz" || r.URL.Path == "/readyz"
	}
}

// defaultAddr is the listen address: an explicit Addr, or ":8080", which
// binds every interface. Binding 127.0.0.1 inside a container makes the
// service unreachable from outside it, with nothing in the logs to say why.
func defaultAddr(addr string) string {
	if addr == "" {
		return ":8080"
	}
	return addr
}

// corsOptions selects the policy: configured origins, or nothing. An empty
// list must not reach go-chi/cors, which reads it as every origin.
func corsOptions(origins []string) *middleware.CORSOptions {
	if len(origins) == 0 {
		return nil
	}
	return &middleware.CORSOptions{AllowedOrigins: origins}
}

// mount attaches h under path, stripping the prefix first: a handler built by
// another package routes its own paths ("/login", not "/admin/login"), and
// chi's Mount does not rewrite the URL on the way in.
func mount(r chi.Router, m Mount, def string) error {
	if m.Handler == nil {
		return nil
	}
	path := m.Path
	if path == "" {
		path = def
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("app: mount path %q must start with \"/\"", m.Path)
	}
	handler := m.Handler
	if path != "/" {
		path = strings.TrimSuffix(path, "/")
		handler = http.StripPrefix(path, m.Handler)
	}
	r.Mount(path, handler)
	return nil
}
