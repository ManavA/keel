// Command webhook is a minimal webhook receiver built out of keel, to be read
// rather than deployed. It verifies each delivery's HMAC signature, stores
// the delivery, and answers 202. Anything unsigned, or without a topic, is
// refused before it reaches the database.
//
// It needs Postgres and nothing else.
package main

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/app"
	"github.com/ManavA/keel/config"
	"github.com/ManavA/keel/httpx"
	keellog "github.com/ManavA/keel/log"
)

// migrationFiles ships with the binary, so there is never a question of which
// migrations a given build carries.
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

	// The receiver is filled in after Open: it needs the pool, which only
	// exists then. The closure below reads the variable when it runs —
	// readiness probes happen after the wiring — never when registered.
	var pool *pgxpool.Pool

	opts := app.Options{
		Logger:            logger,
		Addr:              cfg.Addr(),
		DatabaseURL:       cfg.DatabaseURL,
		TrustedProxies:    cfg.TrustedProxies,
		ShutdownTimeout:   cfg.ShutdownTimeout,
		ReadinessCacheTTL: cfg.ReadinessCacheTTL,
		Checks: map[string]httpx.Check{
			"database": func(ctx context.Context) error {
				if pool == nil {
					return errors.New("pool is not open")
				}
				return pool.Ping(ctx)
			},
		},
	}
	if cfg.MigrateOnStart {
		opts.Migrations = []app.MigrationSource{
			{FS: migrationFiles, Dir: "migrations"},
		}
	}

	a := app.New(opts)
	if err := a.Open(ctx); err != nil {
		logger.Error("open app", "error", err)
		return 1
	}
	defer a.Close()

	pool = a.Pool()
	NewReceiver(pool, cfg.WebhookSecret).Routes(a.Router())

	if err := a.Run(ctx); err != nil {
		logger.Error("server stopped", "error", err)
		return 1
	}
	logger.Info("stopped cleanly")
	return 0
}
