package pg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/auth"
)

// SessionStore is a Postgres-backed auth.SessionStore. It stores a SHA-256
// hash of each token, never the token itself — see auth.SessionStore's doc
// comment for why — so a stolen database dump does not hand out live bearer
// credentials the way a stolen token would.
//
// It enforces auth.SessionLimits the same way the in-memory store does: a
// session idle longer than IdleTimeout stops validating (each successful
// validation moves last_seen_at forward), and no session validates past
// AbsoluteLifetime after its creation. Both windows are disabled by default;
// pass them with NewSessionStoreWithLimits.
type SessionStore struct {
	pool   *pgxpool.Pool
	limits auth.SessionLimits
}

// NewSessionStore wraps an already-open pool.
func NewSessionStore(pool *pgxpool.Pool) *SessionStore {
	return &SessionStore{pool: pool}
}

// NewSessionStoreWithLimits wraps an already-open pool and enforces limits on
// validation: a session idle longer than IdleTimeout stops validating, and no
// session validates past AbsoluteLifetime after creation, however active. A
// non-positive value for either disables that window.
func NewSessionStoreWithLimits(pool *pgxpool.Pool, limits auth.SessionLimits) *SessionStore {
	return &SessionStore{pool: pool, limits: limits}
}

func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create implements auth.SessionStore. It refuses a non-positive ttl — see
// MemorySessionStore.Create's identical guard for why.
func (s *SessionStore) Create(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("authpg: session ttl must be positive, got %s", ttl)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authpg: generate session token: %w", err)
	}
	token := hex.EncodeToString(raw)
	now := time.Now()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO auth_sessions (token_hash, user_id, expires_at, created_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5)`,
		hashSessionToken(token), userID, now.Add(ttl), now, now,
	)
	if err != nil {
		return "", fmt.Errorf("authpg: create session: %w", err)
	}
	return token, nil
}

// Validate implements auth.SessionStore. A successful validation moves
// last_seen_at forward, sliding the idle window; a session past its ttl,
// idle past IdleTimeout, or older than AbsoluteLifetime reports
// auth.ErrSessionNotFound.
func (s *SessionStore) Validate(ctx context.Context, token string) (string, error) {
	var (
		userID     string
		expiresAt  time.Time
		createdAt  time.Time
		lastSeenAt time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT user_id, expires_at, created_at, last_seen_at FROM auth_sessions
		WHERE token_hash = $1`,
		hashSessionToken(token),
	).Scan(&userID, &expiresAt, &createdAt, &lastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", auth.ErrSessionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("authpg: validate session: %w", err)
	}
	now := time.Now()
	if now.After(expiresAt) ||
		(s.limits.IdleTimeout > 0 && now.After(lastSeenAt.Add(s.limits.IdleTimeout))) ||
		(s.limits.AbsoluteLifetime > 0 && now.After(createdAt.Add(s.limits.AbsoluteLifetime))) {
		return "", auth.ErrSessionNotFound
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE auth_sessions SET last_seen_at = $1 WHERE token_hash = $2`,
		now, hashSessionToken(token),
	); err != nil {
		return "", fmt.Errorf("authpg: record session activity: %w", err)
	}
	return userID, nil
}

// Revoke implements auth.SessionStore. Revoking an unknown token affects
// zero rows and is not an error, matching the interface's contract.
func (s *SessionStore) Revoke(ctx context.Context, token string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM auth_sessions WHERE token_hash = $1`, hashSessionToken(token)); err != nil {
		return fmt.Errorf("authpg: revoke session: %w", err)
	}
	return nil
}

// RevokeAllForUser implements auth.SessionStore.
func (s *SessionStore) RevokeAllForUser(ctx context.Context, userID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM auth_sessions WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("authpg: revoke all sessions: %w", err)
	}
	return nil
}
