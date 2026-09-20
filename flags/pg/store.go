// Package pg is a Postgres-backed [flags.Store], for a deployment that runs
// more than one instance and needs flag definitions to be visible to all of
// them.
package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/flags"
)

// Table is the name of the table flags/pg/migrations creates.
const Table = "feature_flags"

// conn is the subset of *pgxpool.Pool this package needs, so a test can
// fake it without a live database.
type conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is a [flags.Store] backed by Postgres. The zero value is not
// usable; build one with [New].
type Store struct {
	db conn
}

// New builds a Store over db, which must already have the schema from
// flags/pg/migrations applied.
func New(db conn) *Store {
	return &Store{db: db}
}

const columns = `key, enabled, percentage, allowlist`

// Get implements [flags.Store.Get].
func (s *Store) Get(ctx context.Context, key string) (flags.Flag, error) {
	var f flags.Flag
	err := s.db.QueryRow(ctx, `select `+columns+` from `+Table+` where key = $1`, key).
		Scan(&f.Key, &f.Enabled, &f.Percentage, &f.Allow)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return flags.Flag{}, flags.ErrNotFound
	case err != nil:
		return flags.Flag{}, fmt.Errorf("flags/pg: get %q: %w", key, err)
	}
	return f, nil
}

// List implements [flags.Store.List], ordered by key.
func (s *Store) List(ctx context.Context) ([]flags.Flag, error) {
	rows, err := s.db.Query(ctx, `select `+columns+` from `+Table+` order by key`)
	if err != nil {
		return nil, fmt.Errorf("flags/pg: list: %w", err)
	}
	defer rows.Close()

	var out []flags.Flag
	for rows.Next() {
		var f flags.Flag
		if err := rows.Scan(&f.Key, &f.Enabled, &f.Percentage, &f.Allow); err != nil {
			return nil, fmt.Errorf("flags/pg: list scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("flags/pg: list rows: %w", err)
	}
	return out, nil
}

const upsertSQL = `
insert into ` + Table + ` (key, enabled, percentage, allowlist, updated_at)
values ($1, $2, $3, $4, now())
on conflict (key) do update set
	enabled    = excluded.enabled,
	percentage = excluded.percentage,
	allowlist  = excluded.allowlist,
	updated_at = now()`

// Upsert implements [flags.Store.Upsert].
func (s *Store) Upsert(ctx context.Context, f flags.Flag) error {
	if err := f.Validate(); err != nil {
		return err
	}
	allow := f.Allow
	if allow == nil {
		// pgx binds a nil slice to SQL NULL, which the column's NOT NULL
		// constraint rejects; no allowlist is an empty one, not absent data.
		allow = []string{}
	}
	if _, err := s.db.Exec(ctx, upsertSQL, f.Key, f.Enabled, f.Percentage, allow); err != nil {
		return fmt.Errorf("flags/pg: upsert %q: %w", f.Key, err)
	}
	return nil
}
