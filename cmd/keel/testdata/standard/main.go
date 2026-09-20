// Command fullstack is the standard stack built out of keel, to be read
// rather than deployed.
//
// Where examples/minimal is the smallest starting point — Postgres only,
// events in-process — this example wires what a real service adds next: the
// local auth source over Postgres, an admin service with its own sessions,
// an outbox relay delivering domain events, and pg-backed idempotency on
// the write route. It still needs Postgres and nothing else: the relay
// publishes onto the in-process bus, so the whole thing comes up under a
// compose file holding an app and a database.
//
// What it shows: the app lifecycle with mounts, migrations from five
// sources sharing one ledger, a write that commits its row and its event
// in one transaction, a relay job publishing what the transaction left
// behind, a retried POST replaying its first response, an operator login,
// and readiness that checks each table the service depends on.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/admin"
	adminpg "github.com/ManavA/keel/admin/pg"
	"github.com/ManavA/keel/app"
	authpg "github.com/ManavA/keel/auth/pg"
	"github.com/ManavA/keel/config"
	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/httpx"
	idempg "github.com/ManavA/keel/idempotency/pg"
	"github.com/ManavA/keel/jobs"
	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/outbox"
	outboxpg "github.com/ManavA/keel/outbox/pg"
)

// migrationFiles ships with the binary, so there is never a question of which
// migrations a given build carries. Its file names must stay unique across
// every other source below: they share one ledger, so a collision reads as
// already applied, or as checksum drift when the contents differ.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

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
	}

	for _, field := range config.Redacted(&cfg) {
		logger.Info("configuration", "name", field.Name, "value", field.Value)
	}

	// Cancelled on SIGINT or SIGTERM. Everything below stops when it is, which
	// keeps the signal handling here rather than buried in a library.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The pool and the relay only exist after Open, while Checks and Jobs are
	// registered on the options before it. The closures below read these
	// variables when they run — probes and job ticks happen after the
	// wiring — never when they are registered.
	var pool *pgxpool.Pool
	var relay *outbox.Relay

	opts := app.Options{
		Logger:            logger,
		Addr:              cfg.Addr(),
		DatabaseURL:       cfg.DatabaseURL,
		ShutdownTimeout:   cfg.ShutdownTimeout,
		ReadinessCacheTTL: cfg.ReadinessCacheTTL,
		Checks: map[string]httpx.Check{
			"outbox":      func(ctx context.Context) error { return checkOutbox(ctx, pool) },
			"idempotency": func(ctx context.Context) error { return checkIdempotency(ctx, pool) },
		},
	}
	if cfg.MigrateOnStart {
		// The package-owned tables go first: everything the example stores
		// assumes they exist. All five sources share the ledger table, so
		// each file is still applied exactly once.
		opts.Migrations = []app.MigrationSource{
			{FS: authpg.MigrationsFS, Dir: "migrations"},
			{FS: adminpg.MigrationsFS, Dir: "migrations"},
			{FS: outboxpg.MigrationsFS, Dir: "migrations"},
			{FS: idempg.MigrationsFS, Dir: "migrations"},
			{FS: migrationFiles, Dir: "migrations"},
		}
	}
	if cfg.RelayInterval > 0 {
		opts.Jobs = []jobs.Entry{{
			Name:     "outbox-relay",
			Interval: cfg.RelayInterval,
			Func: func(ctx context.Context) (jobs.Outcome, error) {
				return tickRelay(ctx, relay)
			},
		}}
	}

	a := app.New(opts)
	if err := a.Open(ctx); err != nil {
		logger.Error("open app", "error", err)
		return 1
	}
	defer a.Close()

	pool = a.Pool()

	authSvc, err := buildAuthService(logger, pool, strings.TrimSpace(cfg.SiteURL))
	if err != nil {
		logger.Error("wire auth", "error", err)
		return 1
	}

	adminSvc, err := buildAdminService(logger, pool, strings.TrimSpace(cfg.AdminSecret))
	if err != nil {
		logger.Error("wire admin", "error", err)
		return 1
	}
	if email, password := strings.TrimSpace(cfg.AdminSeedEmail), strings.TrimSpace(cfg.AdminSeedPassword); email != "" && password != "" {
		if _, err := admin.Seed(ctx, adminpg.NewAdminStore(pool), email, password, "Owner", "admin"); err != nil {
			if errors.Is(err, admin.ErrDuplicateEmail) {
				logger.Info("admin already seeded, leaving it alone")
			} else {
				logger.Error("seed admin", "error", err)
				return 1
			}
		} else {
			logger.Info("seeded first admin", "email", email)
		}
	}

	bus := events.NewInMemoryBus(events.InMemoryBusOptions{Logger: logger})
	relay, err = outbox.NewRelay(pool, outbox.Options{Publisher: bus, Logger: logger})
	if err != nil {
		logger.Error("wire outbox relay", "error", err)
		return 1
	}

	api := &API{notes: NewNotes(pool), auth: authSvc, idem: idempg.New(pool)}
	api.Routes(a.Router())

	// The auth and admin packages own their routes; they live under /auth
	// and /admin so the example stays one service with three concerns
	// rather than three services. chi's Mount does not strip the prefix, so
	// StripPrefix does.
	a.Router().Mount("/auth", http.StripPrefix("/auth", authSvc.Router()))
	a.Router().Mount("/admin", http.StripPrefix("/admin", adminSvc.Router()))

	// One subscriber, for the side effect that may be missed. The relay is
	// at-least-once, so this handler must tolerate seeing an event twice;
	// logging does.
	go func() {
		err := bus.Subscribe(ctx, topicNoteCreated, func(_ context.Context, data []byte) error {
			var env outbox.Envelope
			if err := json.Unmarshal(data, &env); err != nil {
				return err
			}
			logger.Info("note.created delivered", "outbox_id", env.ID, "bytes", len(env.Payload))
			return nil
		})
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

// tickRelay runs one publish cycle and reports what it landed. A failed row
// stays unpublished for the next tick rather than failing the run: the
// relay backs those rows off on its own.
func tickRelay(ctx context.Context, relay *outbox.Relay) (jobs.Outcome, error) {
	published, err := relay.Tick(ctx)
	if err != nil {
		// Fatal, not a failure count: the unpublished set could not be read,
		// so "nothing to do" would be a guess rather than a measurement.
		return jobs.Outcome{Fatal: true}, err
	}
	return jobs.Outcome{Attempted: published, Succeeded: published}, nil
}

// checkOutbox and checkIdempotency prove the relay's and the middleware's
// tables are reachable, not just the database server. A migration the app
// forgot to apply answers here instead of at the first write.
func checkOutbox(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("no pool")
	}
	var n int64
	if err := pool.QueryRow(ctx, "select count(*) from outbox_events").Scan(&n); err != nil {
		return fmt.Errorf("outbox events unreachable: %w", err)
	}
	return nil
}

func checkIdempotency(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("no pool")
	}
	var n int64
	if err := pool.QueryRow(ctx, "select count(*) from idempotency_keys").Scan(&n); err != nil {
		return fmt.Errorf("idempotency keys unreachable: %w", err)
	}
	return nil
}
