// Command agentdemo is a durable, governed agent service built out of keel,
// to be read rather than deployed.
//
// Two agents review a batch of documents. The coordinator lists the batch,
// delegates each document to a reviewer, sends one digest and tries to delete
// the originals; the reviewer reads one document and saves a one-line summary.
// The rules in policy.json allow the reading and the summaries, ask a person
// before the digest goes out, and block the deletions.
//
// What it shows: a run that survives its process being killed and is carried
// on by the next one, tool effects that happen once however often that
// happens, an approval a run waits on across restarts, a blocked action on the
// record with the rule that blocked it, and a budget on what the model costs.
//
// It needs Postgres and nothing else: with no API key the model is scripted,
// and the same run happens the same way every time.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/httpapi"
	agentpg "github.com/ManavA/keel/agent/pg"
	"github.com/ManavA/keel/app"
	"github.com/ManavA/keel/config"
	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/llm"
	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/policy"
	policypg "github.com/ManavA/keel/policy/pg"
)

// migrationFiles ships with the binary, so there is never a question of which
// migrations a given build carries. Its file names must stay unique across
// the other sources below: they share one ledger.
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

	// Redacted replaces the operator token and the API keys, and keeps the
	// database URL's host without its password.
	for _, field := range config.Redacted(&cfg) {
		logger.Info("configuration", "name", field.Name, "value", field.Value)
	}

	// Cancelled on SIGINT or SIGTERM. The server and the worker below both
	// stop when it is, which keeps the signal handling here rather than buried
	// in a library.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := newService(ctx, cfg, logger)
	if err != nil {
		logger.Error("build service", "error", err)
		return 1
	}
	defer svc.app.Close()

	logger.Info("model", "provider", cfg.Provider(),
		"coordinator", svc.names.coordinator, "reviewer", svc.names.reviewer)
	svc.logUnfinished(ctx, logger)

	// app has no lifecycle for the worker, so it runs beside the server: Work
	// claims runs until ctx is cancelled, then lets each execution finish the
	// step it is in and gives its run back for the next process.
	worked := make(chan error, 1)
	go func() { worked <- svc.engine.Work(ctx) }()

	code := 0
	if err := svc.app.Run(ctx); err != nil {
		logger.Error("server stopped", "error", err)
		code = 1
		// The server failed with the signal context still live; the worker
		// must not be left running behind a process that is exiting.
		stop()
	}

	// The pool is closed by the deferred Close, after the worker has made its
	// last writes or has had as long as Work can take to make them.
	select {
	case err := <-worked:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("worker stopped", "error", err)
			code = 1
		}
	case <-time.After(workerWait(cfg)):
		logger.Warn("worker did not stop in time; its runs resume when their leases lapse")
	}
	spend := svc.meter.Totals()
	logger.Info("model spend", "calls", spend.Calls, "tokens_used", spend.Tokens, "cost_micros", spend.CostMicros)
	if code == 0 {
		logger.Info("stopped cleanly")
	}
	return code
}

// service is the example wired: the app that serves it and the engine that
// executes its runs. The tests build it with newService, as run does.
type service struct {
	app    *app.App
	engine *agent.Engine
	names  modelNames
	// meter counts what the process has spent on the model.
	meter *llm.Metered
}

// newService opens the app, builds the model, the rules and the engine,
// registers the two agents and mounts the routes. The caller closes the app.
func newService(ctx context.Context, cfg Config, logger *slog.Logger) (_ *service, err error) {
	// The pool only exists after Open, while Checks is registered before it.
	// The closure reads the variable when a probe runs, never when it is
	// registered.
	var pool *pgxpool.Pool

	opts := cfg.Options()
	opts.Logger = logger
	// The run event stream is one long response, which the default 30 second
	// request timeout would cancel, so the router is built without one, as
	// httpx.RouterOptions says. Each send on a stream has its own write
	// deadline, and startBatch takes a deadline of its own.
	opts.RequestTimeout = -1
	opts.Checks = map[string]httpx.Check{
		"agent_journal": func(ctx context.Context) error { return checkJournal(ctx, pool) },
	}
	if cfg.MigrateOnStart {
		// The package-owned tables go first. All three sources share the
		// ledger table, so each file is still applied exactly once.
		opts.Migrations = []app.MigrationSource{
			{FS: agentpg.MigrationsFS, Dir: "migrations"},
			{FS: policypg.MigrationsFS, Dir: "migrations"},
			{FS: migrationFiles, Dir: "migrations"},
		}
	}

	a := app.New(opts)
	if err := a.Open(ctx); err != nil {
		return nil, fmt.Errorf("open app: %w", err)
	}
	defer func() {
		if err != nil {
			a.Close()
		}
	}()
	pool = a.Pool()

	provider, err := buildProvider(cfg, logger)
	if err != nil {
		return nil, err
	}
	model, meter, err := buildModel(provider, cfg, logger)
	if err != nil {
		return nil, err
	}

	// Every decision is written to policy_decisions before the engine acts on
	// it, so the log holds what was allowed, asked and blocked, with the rule.
	rules, err := loadPolicy()
	if err != nil {
		return nil, err
	}
	decider, err := policy.NewDecider(rules, policy.Options{Recorder: policypg.New(pool), Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}

	// Events are a hint for whoever wants one; nothing here subscribes, and
	// the HTTP stream reads the journal itself.
	bus := events.NewInMemoryBus(events.InMemoryBusOptions{Logger: logger})

	engine, err := agent.New(agent.Options{
		Store:        agentpg.New(pool),
		Model:        model,
		Guard:        app.AgentGuard(decider),
		Events:       bus,
		Logger:       logger,
		LeaseTTL:     cfg.LeaseTTL,
		PollInterval: cfg.PollInterval,
		DrainTimeout: drainTimeout(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	names := namesFor(cfg)
	for _, def := range definitions(NewDocuments(pool), names, cfg) {
		if err := engine.Register(def); err != nil {
			return nil, fmt.Errorf("register %s: %w", def.Name, err)
		}
	}

	api, err := httpapi.New(httpapi.Options{Runs: engine, Actor: operatorActor, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("http surface: %w", err)
	}

	// Everything but the health checks is behind the operator token: the
	// journal holds whatever the tools handled, and an approval is a decision.
	a.Router().Group(func(r chi.Router) {
		r.Use(requireOperator(strings.TrimSpace(cfg.OperatorToken)))
		r.With(withDeadline(30*time.Second)).Post("/api/batches", startBatch(engine))
		r.Mount("/agent", api.Routes())
	})

	return &service{app: a, engine: engine, names: names, meter: meter}, nil
}

// logUnfinished says, at startup, which runs this process found under way and
// how far each had got, so that a restart after a kill shows what it resumed.
// The worker takes a runnable one over once the dead process's lease lapses.
func (s *service) logUnfinished(ctx context.Context, logger *slog.Logger) {
	for _, status := range []agent.Status{agent.StatusRunnable, agent.StatusWaiting} {
		runs, err := s.engine.ListRuns(ctx, agent.RunFilter{Status: status, Limit: 200})
		if err != nil {
			logger.Warn("list unfinished runs", "error", err)
			return
		}
		for _, run := range runs {
			steps, err := s.engine.Timeline(ctx, run.ID)
			if err != nil {
				logger.Warn("read timeline", "run", run.ID, "error", err)
				continue
			}
			if status == agent.StatusWaiting {
				logger.Info("run is still waiting", "run", run.ID, "agent", run.Agent, "for", run.Reason, "steps", len(steps))
				continue
			}
			logger.Info("resuming run", "run", run.ID, "agent", run.Agent, "at_step", len(steps), "last_owner", run.LeaseOwner)
		}
	}
}

// checkJournal proves the engine's tables are reachable, not just the database
// server. A migration that was not applied answers here instead of at the
// first run.
func checkJournal(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("no pool")
	}
	var n int64
	if err := pool.QueryRow(ctx, "select count(*) from "+agentpg.RunsTable).Scan(&n); err != nil {
		return fmt.Errorf("agent runs unreachable: %w", err)
	}
	return nil
}

// workerLastWrites is how long Work may take past DrainTimeout to make the
// last writes of the executions it drained, as its documentation gives it.
const workerLastWrites = 5 * time.Second

// drainTimeout is how long a step in flight is given to finish once the
// process is told to stop: half the shutdown timeout, as the server is given.
func drainTimeout(cfg Config) time.Duration { return cfg.ShutdownTimeout / 2 }

// workerWait is how long run waits for Work to return once the server has
// stopped: as long as Work can take, and never less than the shutdown
// timeout, so that a run whose step was cut off has been given back.
func workerWait(cfg Config) time.Duration {
	return max(cfg.ShutdownTimeout, drainTimeout(cfg)+workerLastWrites)
}
