// Command worker is a jobs-only service built out of keel, to be read rather
// than deployed. It serves no HTTP: a scheduler runs a heartbeat job on an
// interval, and each run writes a row so an operator can see the last time
// the worker did anything.
//
// It needs Postgres and nothing else.
package main

import (
	"context"
	"embed"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ManavA/keel/config"
	"github.com/ManavA/keel/jobs"
	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
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

	// Cancelled on SIGINT or SIGTERM. The scheduler below stops when it is,
	// which keeps the signal handling here rather than buried in a library.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// pg.Open pings, so a wrong password or an unreachable host stops the
	// process here rather than on the first job tick.
	pool, err := pg.Open(ctx, pg.Options{URL: cfg.DatabaseURL, Logger: logger})
	if err != nil {
		logger.Error("open pool", "error", err)
		return 1
	}
	defer pool.Close()

	if cfg.MigrateOnStart {
		if _, err := migrate.Run(ctx, pool, migrate.Options{FS: migrationFiles, Dir: "migrations"}); err != nil {
			logger.Error("apply migrations", "error", err)
			return 1
		}
	}

	scheduler := jobs.NewScheduler(jobs.SchedulerOptions{Logger: logger})
	if err := scheduler.Register(jobs.Entry{
		Name:     cfg.HeartbeatJob,
		Interval: cfg.HeartbeatInterval,
		Func: func(ctx context.Context) (jobs.Outcome, error) {
			return runHeartbeat(ctx, pool, cfg.HeartbeatJob)
		},
	}); err != nil {
		logger.Error("register jobs", "error", err)
		return 1
	}

	// Run blocks until the signal context is cancelled, running the entry
	// once immediately and then every interval. A cancelled context is the
	// normal stop, not a failure.
	logger.Info("worker running", "job", cfg.HeartbeatJob, "interval", cfg.HeartbeatInterval.String())
	scheduler.Run(ctx)
	logger.Info("stopped cleanly")
	return 0
}
