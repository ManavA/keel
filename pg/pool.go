// Package pg is the Postgres plumbing a service needs before its first query: a
// configured pgx pool, a transaction helper, and paging that does not silently
// ignore the caller.
//
// It stops at the pool, transactions, and paging. Queries and repositories
// belong to the service.
package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/metrics"
)

// Options configures Open. Only URL is required.
type Options struct {
	// URL is a Postgres connection string, and must not be empty: libpq's
	// defaults would otherwise send the service at a local Unix socket and
	// report a failure that names neither the configuration nor the host.
	//
	// Settings in the URL are honoured, and the fields below override them
	// where set, so a deployment can tune the pool through the environment
	// without a code change.
	URL string

	// MaxConns defaults to pgx's own default, the greater of 4 and the number of
	// CPUs. Size it against the database's connection limit divided by the
	// number of instances you will run.
	MaxConns int32

	// MinConns keeps this many connections open. Zero is usually right; set it
	// when connection setup is slow enough to show up in request latency.
	MinConns int32

	// MaxConnLifetime retires a connection after this long, default 1 hour, so
	// a failover, credential rotation or resize takes effect without a restart.
	MaxConnLifetime time.Duration

	// MaxConnIdleTime closes an idle connection after this long, default 30
	// minutes.
	MaxConnIdleTime time.Duration

	// ConnectTimeout bounds one connection attempt, default 10 seconds.
	ConnectTimeout time.Duration

	// AfterConnect runs on every new connection — a SET, a prepared statement,
	// a type registration.
	AfterConnect func(context.Context, *pgx.Conn) error

	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Open parses the URL, builds a pool and runs a query to prove it works.
//
// pgxpool.New is lazy: it returns a usable pool having contacted nothing, so a
// service with a wrong password or an unreachable host starts cleanly and fails
// on its first request instead of at startup.
func Open(ctx context.Context, opts Options) (*pgxpool.Pool, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(opts.URL) == "" {
		return nil, errors.New("pg: Options.URL is empty")
	}

	cfg, err := pgxpool.ParseConfig(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("pg: parse connection string: %w", err)
	}

	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	// Each of these is applied only when it was asked for, and defaulted only
	// when neither the caller nor the URL set it. Assigning unconditionally
	// would overwrite pool_max_conn_lifetime, pool_max_conn_idle_time and
	// connect_timeout from the connection string, silently, so a deployment
	// tuning them through the environment would see no effect.
	cfg.MaxConnLifetime = pick(opts.MaxConnLifetime, cfg.MaxConnLifetime, time.Hour)
	cfg.MaxConnIdleTime = pick(opts.MaxConnIdleTime, cfg.MaxConnIdleTime, 30*time.Minute)
	cfg.ConnConfig.ConnectTimeout = pick(opts.ConnectTimeout, cfg.ConnConfig.ConnectTimeout, 10*time.Second)
	if opts.AfterConnect != nil {
		cfg.AfterConnect = opts.AfterConnect
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: create pool: %w", err)
	}

	verifyCtx, cancel := context.WithTimeout(ctx, cfg.ConnConfig.ConnectTimeout)
	defer cancel()
	if err := ping(verifyCtx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "database connected",
		slog.String("host", cfg.ConnConfig.Host),
		slog.String("database", cfg.ConnConfig.Database),
		slog.Int("max_conns", int(cfg.MaxConns)),
	)
	return pool, nil
}

// Acquire takes a connection from the pool and records how long the
// caller waited for one. A nil m records nothing. The wait is recorded
// whether acquisition succeeded or not: a pool that answers slowly and
// then fails still kept the caller waiting.
func Acquire(ctx context.Context, pool *pgxpool.Pool, m *metrics.Metrics) (*pgxpool.Conn, error) {
	start := time.Now()
	conn, err := pool.Acquire(ctx)
	m.ObservePoolAcquire(ctx, time.Since(start))
	return conn, err
}

// HealthCheck returns a readiness check for this pool, shaped to fit
// httpx.Check without either package importing the other.
func HealthCheck(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		if pool == nil {
			return fmt.Errorf("pg: no pool")
		}
		return ping(ctx, pool)
	}
}

// ping runs a real query rather than pgxpool.Ping, which sends an empty
// statement and succeeds against a server that is connected but refusing work —
// read-only after a failover, or out of disk. A select costs the same.
func ping(ctx context.Context, pool *pgxpool.Pool) error {
	var one int
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		return fmt.Errorf("pg: database unreachable: %w", err)
	}
	if one != 1 {
		return fmt.Errorf("pg: database answered %d to select 1", one)
	}
	return nil
}

// pick returns the first of the caller's value, the connection string's value
// and the default that is set.
//
// The MaxConnLifetime and MaxConnIdleTime defaults below duplicate pgxpool's
// own (one hour and thirty minutes), so for those two the third argument is
// unreachable today: ParseConfig has already filled the field in. They are
// written out anyway so this package's defaults are stated rather than
// inherited, and TestOpenDefaultsMatchPgxpool fails if the two ever diverge.
// ConnectTimeout is different: pgx leaves it at zero, meaning no timeout, so
// the default there does apply.
func pick(fromOptions, fromURL, def time.Duration) time.Duration {
	switch {
	case fromOptions > 0:
		return fromOptions
	case fromURL > 0:
		return fromURL
	default:
		return def
	}
}
