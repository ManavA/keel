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
type SessionStore struct {
	pool *pgxpool.Pool
}

// NewSessionStore wraps an already-open pool.
func NewSessionStore(pool *pgxpool.Pool) *SessionStore {
	return &SessionStore{pool: pool}
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

	_, err := s.pool.Exec(ctx, `
		INSERT INTO auth_sessions (token_hash, user_id, expires_at)
		VALUES ($1, $2, $3)`,
		hashSessionToken(token), userID, time.Now().Add(ttl),
	)
	if err != nil {
		return "", fmt.Errorf("authpg: create session: %w", err)
	}
	return token, nil
}

// Validate implements auth.SessionStore.
func (s *SessionStore) Validate(ctx context.Context, token string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		SELECT user_id FROM auth_sessions
		WHERE token_hash = $1 AND expires_at > now()`,
		hashSessionToken(token),
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", auth.ErrSessionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("authpg: validate session: %w", err)
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
