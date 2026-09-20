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
// reaches, an app holding the pool, migrations, routes, checks and jobs,
// password auth with verification, reset and DB sessions mounted next to the
// API, an HTTP API with keyset paging and generic errors, liveness and
// readiness that check the right things, a background job whose result is
// computed from what it counted, and a shutdown that finishes the requests
// already in flight.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ManavA/keel/app"
	authpg "github.com/ManavA/keel/auth/pg"
	"github.com/ManavA/keel/config"
	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/jobs"
	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/mail"
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
	if cfg.Cloud() {
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

	// The API and the index hold what the handlers and the job need, filled
	// in after Open: the pool only exists then, while Checks and Jobs are
	// registered on the options before it. The closures below read these
	// variables when they run — requests and job ticks happen after the
	// wiring — never when they are registered.
	//
	// Routes are registered after Open too, on the app's router: api.Routes
	// requires the auth service for its session middleware, and the auth
	// service needs the pool, which only exists after Open.
	api := &API{}
	var index search.Index
	var notes *Notes

	opts := app.Options{
		Logger:            logger,
		Addr:              cfg.Addr(),
		DatabaseURL:       cfg.DatabaseURL,
		TrustedProxies:    cfg.TrustedProxies,
		CORSOrigins:       cfg.CORSOrigins,
		ShutdownTimeout:   cfg.ShutdownTimeout,
		ReadinessCacheTTL: cfg.ReadinessCacheTTL,
		Checks: map[string]httpx.Check{
			"search": func(ctx context.Context) error { return index.Health(ctx) },
		},
	}
	if cfg.MigrateOnStart {
		// The auth package's tables go first: everything the example stores
		// about an account assumes they exist. Both sources share the ledger
		// table, so each file is still applied exactly once.
		opts.Migrations = []app.MigrationSource{
			{FS: authpg.MigrationsFS, Dir: "migrations"},
			{FS: migrationFiles, Dir: "migrations"},
		}
	}
	if cfg.ReconcileInterval > 0 {
		opts.Jobs = []jobs.Entry{{
			Name:     "reconcile-search-index",
			Interval: cfg.ReconcileInterval,
			Func: func(ctx context.Context) (jobs.Outcome, error) {
				return reconcileSearchIndex(notes, index)(ctx)
			},
		}}
	}

	a := app.New(opts)
	if err := a.Open(ctx); err != nil {
		logger.Error("open app", "error", err)
		return 1
	}
	defer a.Close()

	pool := a.Pool()
	pgIndex := search.NewPostgresIndex(search.PostgresIndexOptions{
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
	if err := pgIndex.EnsureSchema(ctx); err != nil {
		logger.Error("prepare search index", "error", err)
		return 1
	}
	index = pgIndex

	notes = NewNotes(pool)
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

	api.notes = notes
	api.index = index
	api.publisher = bus
	api.auth = authSvc
	api.Routes(a.Router())
	// Templates parse once, before serving: a page or block that does not
	// parse stops the process here rather than failing its first request.
	tmpl, err := ParseTemplates()
	if err != nil {
		logger.Error("parse UI templates", "error", err)
		return 1
	}
	api.tmpl = tmpl
	api.NotesUIRoutes(a.Router())
	// The auth package owns its routes; they live under /auth so the example
	// stays one service with two concerns rather than two services. chi's
	// Mount does not strip the prefix, so StripPrefix does — the auth
	// package's own doc comment calls for exactly this.
	//
	// The form handlers reuse the same router instance in-process, so their
	// validation, status codes and rate limit are the JSON endpoints'
	// themselves rather than a second implementation beside them.
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
	// The landing shell and the static assets need no database, so they hang
	// off their own value rather than the API.
	NewUI(tmpl).Routes(a.Router())

	// One subscriber, for the side effect that may be missed. The in-memory bus
	// is at-most-once, which is the right fit for a notification and the wrong
	// fit for anything the API's own responses depend on.
	go func() {
		err := bus.Subscribe(ctx, topicNoteCreated, notifyOnNoteCreated(sender))
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("note.created subscriber stopped", "error", err)
		}
	}()

	if err := a.Run(ctx); err != nil {
		logger.Error("server stopped", "error", err)
		return 1
	}
	logger.Info("stopped cleanly")
	return 0
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
