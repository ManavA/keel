// Package pg is a Postgres-backed [jobs.HistoryStore], for run history that
// survives a restart.
//
// Runs are append-only: Record inserts one row per finished run, and List
// reads the newest first. There is no update or delete, so concurrent entries
// recording at once cannot conflict with each other.
package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/jobs"
)

// Table is the name of the table jobs/pg/migrations creates.
const Table = "job_runs"

// defaultLimit bounds List when the caller passes a non-positive limit, so a
// forgotten argument cannot load the whole table into memory.
const defaultLimit = 100

// conn is the subset of *pgxpool.Pool this package needs, so a test can fake
// it without a live database.
type conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store is a [jobs.HistoryStore] backed by Postgres. The zero value is not
// usable; build one with [New].
type Store struct {
	db conn
}

// New builds a Store over db, which must already have the schema from
// jobs/pg/migrations applied.
func New(db conn) *Store {
	return &Store{db: db}
}

const recordSQL = `
insert into ` + Table + ` (name, started_at, finished_at, attempted, succeeded, failed, fatal, status, exit_code)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

// Record implements [jobs.HistoryStore.Record].
func (s *Store) Record(ctx context.Context, rec jobs.RunRecord) error {
	if _, err := s.db.Exec(ctx, recordSQL,
		rec.Name, rec.StartedAt, rec.FinishedAt,
		rec.Attempted, rec.Succeeded, rec.Failed, rec.Fatal,
		rec.Status, rec.ExitCode,
	); err != nil {
		return fmt.Errorf("jobs/pg: record run of %q: %w", rec.Name, err)
	}
	return nil
}

const listAllSQL = `
select name, started_at, finished_at, attempted, succeeded, failed, fatal, status, exit_code
from ` + Table + `
order by id desc
limit $1`

const listNamedSQL = `
select name, started_at, finished_at, attempted, succeeded, failed, fatal, status, exit_code
from ` + Table + `
where name = $1
order by id desc
limit $2`

// List implements [jobs.HistoryStore.List]: newest runs first, filtered by
// name when one is given.
func (s *Store) List(ctx context.Context, name string, limit int) ([]jobs.RunRecord, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	var (
		rows pgx.Rows
		err  error
	)
	if name == "" {
		rows, err = s.db.Query(ctx, listAllSQL, limit)
	} else {
		rows, err = s.db.Query(ctx, listNamedSQL, name, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("jobs/pg: list runs: %w", err)
	}
	defer rows.Close()

	var out []jobs.RunRecord
	for rows.Next() {
		var rec jobs.RunRecord
		if err := rows.Scan(&rec.Name, &rec.StartedAt, &rec.FinishedAt,
			&rec.Attempted, &rec.Succeeded, &rec.Failed, &rec.Fatal,
			&rec.Status, &rec.ExitCode,
		); err != nil {
			return nil, fmt.Errorf("jobs/pg: scan run: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs/pg: list runs: %w", err)
	}
	return out, nil
}
