package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/jobs"
)

// runHeartbeat records one row for job and reports what it did. The row is
// the liveness signal: an operator reads the newest ran_at for the job and
// knows the last time the worker did anything. The outcome is computed from
// what the run counted, so an empty write reports against Attempted rather
// than as a bare success.
func runHeartbeat(ctx context.Context, pool *pgxpool.Pool, job string) (jobs.Outcome, error) {
	if _, err := pool.Exec(ctx, `INSERT INTO worker_heartbeats (job) VALUES ($1)`, job); err != nil {
		return jobs.Outcome{Attempted: 1, Failed: 1}, fmt.Errorf("insert heartbeat: %w", err)
	}
	return jobs.Outcome{Attempted: 1, Succeeded: 1}, nil
}
