// Package pg is the Postgres plumbing a service needs before its first query: a
// configured pgx pool, a transaction helper, and paging that does not silently
// ignore the caller.
//
// It is not a query builder and not an ORM. Repositories are yours.
package pg

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options configures Open. Only URL is required.
type Options struct {
	// URL is a Postgres connection string. Settings in the URL are applied
	// first and the fields below override them, so a deployment can tune the
	// pool through the environment without a code change.
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
	cfg.MaxConnLifetime = orDuration(opts.MaxConnLifetime, time.Hour)
	cfg.MaxConnIdleTime = orDuration(opts.MaxConnIdleTime, 30*time.Minute)
	cfg.ConnConfig.ConnectTimeout = orDuration(opts.ConnectTimeout, 10*time.Second)
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

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}
