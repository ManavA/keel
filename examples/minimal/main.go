// Command minimal is a small service built out of keel, to be read rather than
// deployed.
//
// It needs Postgres and nothing else. Search is a Postgres index, mail goes to
// the log, events are in-process and the scheduler runs in this process, so the
// whole thing comes up under a compose file holding an app and a database. Each
// of those has an external backend available when one is wanted, chosen by
// configuration rather than by a rewrite.
//
// What it shows: configuration read and logged safely, a logger every layer
// reaches, migrations applied from embedded files, password auth with
// verification, reset and DB sessions mounted next to the API, an HTTP API
// with keyset paging and generic errors, liveness and readiness that check
// the right things, a background job whose result is computed from what it
// counted, and a shutdown that finishes the requests already in flight.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/auth"
	authpg "github.com/ManavA/keel/auth/pg"
	"github.com/ManavA/keel/config"
	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/jobs"
	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/mail"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/search"
)

// migrationFiles ships with the binary, so there is never a question of which
// migrations a given build carries.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// searchTable is where the Postgres-backed index keeps its documents. It is
// separate from the notes table: notes is the source of truth, and the index is
// derived from it and rebuilt by the reconcile job.
const searchTable = "note_documents"

func main() {
	os.Exit(run())
}

// run is main with a return value, so every failure path can close what it
// opened. os.Exit in main skips deferred functions.
func run() int {
	logger := keellog.New(keellog.Options{})
	slog.SetDefault(logger)

	var cfg Config
	if err := config.Load(&cfg); err != nil {
		logger.Error("load configuration", "error", err)
		return 1
	}

	cfg.Env = strings.TrimSpace(cfg.Env)
	if cfg.Env != "development" {
		// Cloud Logging reads a "severity" field and ignores slog's "level",
		// so without this every entry is DEFAULT severity and a severity-based
		// alert matches nothing.
		logger = keellog.New(keellog.Options{Cloud: true})
		slog.SetDefault(logger)
	} else {
		// Debug in development only: the mail sender logs verification and
		// reset links at Debug, which is what makes the signup flow walkable
		// from the terminal. Those links are credentials, so production stays
		// at Info where they are never written.
		logger = keellog.New(keellog.Options{Level: slog.LevelDebug})
		slog.SetDefault(logger)
	}

	for _, field := range config.Redacted(&cfg) {
		logger.Info("configuration", "name", field.Name, "value", field.Value)
	}

	// Cancelled on SIGINT or SIGTERM. Everything below stops when it is, which
	// keeps the signal handling here rather than buried in a library.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pg.Open(ctx, pg.Options{URL: cfg.DatabaseURL, Logger: logger})
	if err != nil {
		logger.Error("open database", "error", err)
		return 1
	}
	defer pool.Close()

	if cfg.MigrateOnStart {
		if err := applyMigrations(ctx, pool, logger); err != nil {
			logger.Error("apply migrations", "error", err)
			return 1
		}
	}

	index := search.NewPostgresIndex(search.PostgresIndexOptions{
		Pool: pool,
		Config: search.PostgresConfig{
			Table:            searchTable,
			SearchableFields: []string{"title", "body"},
			TextSearchConfig: "english",
		},
		Logger: logger,
	})
	// Creates the table and its index if they are absent, and does nothing
	// otherwise, so it is safe on every start.
	if err := index.EnsureSchema(ctx); err != nil {
		logger.Error("prepare search index", "error", err)
		return 1
	}

	notes := NewNotes(pool)
	bus := events.NewInMemoryBus(events.InMemoryBusOptions{Logger: logger})
	// In development the verification and reset links are logged, so the
	// signup flow can be walked through from the terminal. Outside
	// development only the fact of the send is logged: the link is a
	// credential, and credentials do not belong in production logs.
	sender := mail.NewLogSender(mail.LogSenderOptions{Logger: logger, LogBodies: cfg.Env == "development"})

	authSvc, err := buildAuthService(ctx, cfg, logger, pool, sender)
	if err != nil {
		logger.Error("wire auth", "error", err)
		return 1
	}

	// One subscriber, for the side effect that may be missed. The in-memory bus
	// is at-most-once, which is the right fit for a notification and the wrong
	// fit for anything the API's own responses depend on.
	go func() {
		err := bus.Subscribe(ctx, topicNoteCreated, notifyOnNoteCreated(sender))
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("note.created subscriber stopped", "error", err)
		}
	}()

	if cfg.ReconcileInterval > 0 {
		scheduler := jobs.NewScheduler(jobs.SchedulerOptions{Logger: logger})
		err := scheduler.Register(jobs.Entry{
			Name:     "reconcile-search-index",
			Interval: cfg.ReconcileInterval,
			Func:     reconcileSearchIndex(notes, index),
		})
		if err != nil {
			logger.Error("register reconcile job", "error", err)
			return 1
		}
		go scheduler.Run(ctx)
	}

	api := &API{notes: notes, index: index, publisher: bus, auth: authSvc}
	srv := httpx.NewServer(httpx.ServerOptions{
		Addr:            cfg.Addr(),
		Handler:         newRouter(cfg, logger, api, authSvc, pool, index),
		ShutdownTimeout: cfg.ShutdownTimeout,
		Logger:          logger,
	})

	if err := srv.ListenAndServe(ctx); err != nil {
		logger.Error("server stopped", "error", err)
		return 1
	}
	logger.Info("stopped cleanly")
	return 0
}

func newRouter(cfg Config, logger *slog.Logger, api *API, authSvc *auth.Service, pool *pgxpool.Pool, index search.Index) chi.Router {
	r := httpx.NewRouter(httpx.RouterOptions{
		Logger: logger,
		RealIP: middleware.RealIPOptions{TrustedProxies: cfg.TrustedProxies},
		RequestLog: middleware.RequestLogOptions{
			// The health endpoints are polled continuously and would bury
			// everything else.
			Skip: func(r *http.Request) bool {
				return r.URL.Path == "/healthz" || r.URL.Path == "/readyz"
			},
			SlowRequest: time.Second,
		},
	})

	if len(cfg.CORSOrigins) > 0 {
		r.Use(middleware.CORS(middleware.CORSOptions{AllowedOrigins: cfg.CORSOrigins}))
	}

	r.Mount("/", httpx.Health(httpx.HealthOptions{
		Logger:   logger,
		CacheTTL: cfg.ReadinessCacheTTL,
		Checks: map[string]httpx.Check{
			"database": pg.HealthCheck(pool),
			"search":   index.Health,
		},
		// The errors name hosts and databases, and this endpoint is reachable
		// from wherever the service is.
		ExposeCheckErrors: false,
	}))

	// The auth package owns its routes; they live under /auth so the example
	// stays one service with two concerns rather than two services. chi's
	// Mount does not strip the prefix, so StripPrefix does — the auth
	// package's own doc comment calls for exactly this.
	r.Mount("/auth", http.StripPrefix("/auth", authSvc.Router()))

	api.Routes(r)
	return r
}

// applyMigrations runs the embedded migrations, tolerating checksum drift
// because a branch under development edits its own migrations constantly. A
// deployment that wants drift to stop the rollout drops the errors.Is check.
//
// The auth package's tables go first: everything the example stores about an
// account assumes they exist. Both runs share the ledger table, so each file
// is still applied exactly once.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	if err := applyMigrationFS(ctx, pool, logger, authpg.MigrationsFS, "migrations"); err != nil {
		return err
	}
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	return applyMigrationFS(ctx, pool, logger, sub, "")
}

func applyMigrationFS(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, fsys fs.FS, dir string) error {
	result, err := migrate.Run(ctx, pool, migrate.Options{FS: fsys, Dir: dir, Logger: logger})
	if err != nil && !errors.Is(err, migrate.ErrChecksumDrift) {
		return err
	}
	if len(result.Changed) > 0 {
		logger.Warn("migrations changed since they were applied",
			"migrations", result.Changed,
			"note", "the new content will not run; use migrate.Replay to find out whether it would apply")
	}
	return nil
}

// notifyOnNoteCreated is the subscriber. It sends through whatever Sender was
// configured, which here writes to the log.
func notifyOnNoteCreated(sender mail.Sender) events.Handler {
	return func(ctx context.Context, data []byte) error {
		var note Note
		if err := json.Unmarshal(data, &note); err != nil {
			return err
		}
		return sender.Send(ctx, "owner@example.com", "note-created", map[string]any{
			"Title": note.Title,
			"ID":    note.ID,
		})
	}
}

// reconcileSearchIndex rebuilds the index from the notes table and removes
// anything the table no longer has. It is what repairs an index left stale by a
// failed write, which is why the API can treat a failed index removal as a
// warning rather than a failed request.
func reconcileSearchIndex(notes *Notes, index search.Index) jobs.Func {
	return func(ctx context.Context) (jobs.Outcome, error) {
		all, err := notes.All(ctx)
		if err != nil {
			// Fatal, not a failure count: the authoritative set could not be
			// read, so pruning now would delete documents whose rows exist.
			// "Could not measure" is a different claim from "measured, fine".
			return jobs.Outcome{Fatal: true}, err
		}

		outcome := jobs.Outcome{Attempted: len(all)}
		if len(all) > 0 {
			docs := make([]search.Document, 0, len(all))
			for _, note := range all {
				docs = append(docs, noteDocument(note))
			}
			if err := index.IndexDocuments(ctx, docs); err != nil {
				outcome.Failed = len(all)
				return outcome, err
			}
			outcome.Succeeded = len(all)
		}

		keep := make(map[string]struct{}, len(all))
		for _, note := range all {
			keep[note.ID] = struct{}{}
		}
		if _, err := index.PruneStale(ctx, keep); err != nil {
			return outcome, err
		}
		return outcome, nil
	}
}
