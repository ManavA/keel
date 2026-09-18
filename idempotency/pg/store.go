// Package pg is a Postgres-backed [idempotency.Store], for a deployment
// that runs more than one instance and needs a claim to be visible to all
// of them.
//
// A claim and a reclaim of an expired key are both done with one
// INSERT ... ON CONFLICT statement rather than a read followed by a write,
// so two instances racing to claim the same key cannot both succeed:
// Postgres serializes the two statements against the same row, and only
// one of them satisfies the conflict's WHERE clause.
//
// expires_at is the claim's lease deadline until the claim completes, and
// the end of the replay window after. The reclaim checks only expires_at, so
// a claim that is never completed or released becomes reclaimable when its
// lease passes. Each claim writes a fresh claim_id, and Complete and Release
// match on it, so a request that outlived its lease cannot touch the row of
// the request that reclaimed the key.
package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/idempotency"
)

// Table is the name of the table idempotency/pg/migrations creates.
const Table = "idempotency_keys"

// conn is the subset of *pgxpool.Pool this package needs, so a test can
// fake it without a live database.
type conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is a [idempotency.Store] backed by Postgres. The zero value is not
// usable; build one with [New].
type Store struct {
	db conn
}

// New builds a Store over db, which must already have the schema from
// idempotency/pg/migrations applied.
func New(db conn) *Store {
	return &Store{db: db}
}

const claimSQL = `
insert into ` + Table + ` (key, request_hash, claim_id, done, status, header, body, expires_at, created_at)
values ($1, $2, $3, false, 0, '{}'::jsonb, ''::bytea, $4, now())
on conflict (key) do update set
	request_hash = excluded.request_hash,
	claim_id     = excluded.claim_id,
	done         = false,
	status       = 0,
	header       = '{}'::jsonb,
	body         = ''::bytea,
	expires_at   = excluded.expires_at,
	created_at   = now()
where ` + Table + `.expires_at < now()
returning key`

const selectSQL = `
select request_hash, done, status, header, body, expires_at
from ` + Table + `
where key = $1`

// Claim implements [idempotency.Store.Claim].
func (s *Store) Claim(ctx context.Context, key, requestHash string, lease time.Duration) (string, *idempotency.Record, error) {
	claimID := idempotency.NewClaimID()
	var claimedKey string
	err := s.db.QueryRow(ctx, claimSQL, key, requestHash, claimID, time.Now().Add(lease)).Scan(&claimedKey)
	switch {
	case err == nil:
		return claimID, nil, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", nil, fmt.Errorf("idempotency/pg: claim %q: %w", key, err)
	}

	// The insert found a row it was not allowed to touch: either an
	// in-progress claim, or a completed one that has not expired yet.
	var (
		storedHash string
		done       bool
		status     int
		header     []byte
		body       []byte
		expiresAt  time.Time
	)
	row := s.db.QueryRow(ctx, selectSQL, key)
	if err := row.Scan(&storedHash, &done, &status, &header, &body, &expiresAt); err != nil {
		return "", nil, fmt.Errorf("idempotency/pg: read %q: %w", key, err)
	}
	if !done {
		return "", nil, idempotency.ErrInProgress
	}

	hdr := http.Header{}
	if len(header) > 0 {
		var raw map[string][]string
		if err := json.Unmarshal(header, &raw); err != nil {
			return "", nil, fmt.Errorf("idempotency/pg: decode stored header for %q: %w", key, err)
		}
		hdr = http.Header(raw)
	}
	return "", &idempotency.Record{
		RequestHash: storedHash,
		Status:      status,
		Header:      hdr,
		Body:        body,
		ExpiresAt:   expiresAt,
	}, nil
}

const completeSQL = `
update ` + Table + `
set done = true, status = $3, header = $4, body = $5, expires_at = $6
where key = $1 and claim_id = $2 and done = false`

// Complete implements [idempotency.Store.Complete].
func (s *Store) Complete(ctx context.Context, key, claimID string, rec idempotency.Record) error {
	header, err := json.Marshal(map[string][]string(rec.Header))
	if err != nil {
		return fmt.Errorf("idempotency/pg: encode header for %q: %w", key, err)
	}
	body := rec.Body
	if body == nil {
		// pgx binds a nil []byte to SQL NULL, which the column's NOT NULL
		// constraint rejects; an empty response body is not absent data.
		body = []byte{}
	}

	tag, err := s.db.Exec(ctx, completeSQL, key, claimID, rec.Status, header, body, rec.ExpiresAt)
	if err != nil {
		return fmt.Errorf("idempotency/pg: complete %q: %w", key, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("idempotency/pg: complete %q: %w", key, idempotency.ErrClaimLost)
	}
	return nil
}

const releaseSQL = `delete from ` + Table + ` where key = $1 and claim_id = $2 and done = false`

// Release implements [idempotency.Store.Release].
func (s *Store) Release(ctx context.Context, key, claimID string) error {
	if _, err := s.db.Exec(ctx, releaseSQL, key, claimID); err != nil {
		return fmt.Errorf("idempotency/pg: release %q: %w", key, err)
	}
	return nil
}
