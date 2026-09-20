// Package pg is a Postgres-backed [notifyprefs.Store], for a deployment
// that runs more than one instance and needs one user's preferences to read
// the same everywhere.
//
// One row holds one user's opt-outs as a JSONB object of
// {category: {channel: true}}. Security and transactional pairs are stripped
// before persisting — the same rule [notifyprefs.AllowedBy] enforces on
// read — so a row only ever holds suppressible pairs, and a reader that
// evaluates the JSON directly cannot suppress what the Go check would send.
package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/notifyprefs"
)

// errUserIDRequired reports a Store call with no user to look up.
var errUserIDRequired = errors.New("notifyprefs: user id is required")

// Table is the name of the table notifyprefs/pg/migrations creates.
const Table = "notification_preferences"

// conn is the subset of *pgxpool.Pool this package needs, so a test can
// fake it without a live database.
type conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is a [notifyprefs.Store] backed by Postgres. The zero value is not
// usable; build one with [New].
type Store struct {
	db conn
}

// New builds a Store over db, which must already have the schema from
// notifyprefs/pg/migrations applied.
func New(db conn) *Store {
	return &Store{db: db}
}

// Get implements [notifyprefs.Store.Get]. A user with no row gets the zero
// Preferences, which under [notifyprefs.AllowedBy] sends everything.
func (s *Store) Get(ctx context.Context, userID string) (notifyprefs.Preferences, error) {
	if userID == "" {
		return notifyprefs.Preferences{}, fmt.Errorf("notifyprefs/pg: get: %w", errUserIDRequired)
	}
	var raw []byte
	err := s.db.QueryRow(ctx, `select opt_outs from `+Table+` where user_id = $1`, userID).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notifyprefs.Preferences{}, nil
		}
		return notifyprefs.Preferences{}, fmt.Errorf("notifyprefs/pg: get %q: %w", userID, err)
	}
	var prefs notifyprefs.Preferences
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &prefs); err != nil {
			return notifyprefs.Preferences{}, fmt.Errorf("notifyprefs/pg: decode preferences for %q: %w", userID, err)
		}
	}
	return prefs, nil
}

const upsertSQL = `
insert into ` + Table + ` (user_id, opt_outs, updated_at)
values ($1, $2, now())
on conflict (user_id) do update set
	opt_outs   = excluded.opt_outs,
	updated_at = now()`

// Set implements [notifyprefs.Store.Set]. It replaces the user's row, so a
// category the new value no longer names is re-subscribed rather than
// lingering from an earlier write.
func (s *Store) Set(ctx context.Context, userID string, prefs notifyprefs.Preferences) error {
	if userID == "" {
		return fmt.Errorf("notifyprefs/pg: set: %w", errUserIDRequired)
	}
	raw, err := json.Marshal(prefs.Normalized())
	if err != nil {
		return fmt.Errorf("notifyprefs/pg: encode preferences for %q: %w", userID, err)
	}
	if _, err := s.db.Exec(ctx, upsertSQL, userID, raw); err != nil {
		return fmt.Errorf("notifyprefs/pg: set %q: %w", userID, err)
	}
	return nil
}

// Allowed implements [notifyprefs.Store.Allowed].
func (s *Store) Allowed(ctx context.Context, userID string, category notifyprefs.Category, channel notifyprefs.Channel) (bool, error) {
	if userID == "" {
		return false, fmt.Errorf("notifyprefs/pg: allowed: %w", errUserIDRequired)
	}
	prefs, err := s.Get(ctx, userID)
	if err != nil {
		return false, err
	}
	return notifyprefs.AllowedBy(prefs, category, channel), nil
}
