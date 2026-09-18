package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/auth"
)

// VerificationStore is a Postgres-backed auth.VerificationStore.
type VerificationStore struct {
	pool *pgxpool.Pool
}

// NewVerificationStore wraps an already-open pool.
func NewVerificationStore(pool *pgxpool.Pool) *VerificationStore {
	return &VerificationStore{pool: pool}
}

// Create implements auth.VerificationStore.
//
// A token_hash collision (astronomically unlikely: it is a SHA-256 digest
// of 32 random bytes) fails the insert rather than overwriting the
// existing row. An earlier version used ON CONFLICT DO UPDATE, which would
// have reassigned a colliding row to a NEW user_id and reset its consumed
// flag to false — un-consuming a token that had already been spent, or
// handing a live token to a user it was never issued to. DO NOTHING
// leaves the existing row alone, and this checks that a row actually was
// inserted rather than reporting success for one that was not.
func (s *VerificationStore) Create(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO auth_verification_tokens (token_hash, user_id, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (token_hash) DO NOTHING`,
		tokenHash, userID, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("authpg: create verification token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("authpg: create verification token: token_hash collision, another token already uses this hash")
	}
	return nil
}

// ConsumeValid implements auth.VerificationStore: it validates and
// invalidates the token in one statement, so two concurrent requests
// presenting the same token cannot both succeed.
func (s *VerificationStore) ConsumeValid(ctx context.Context, tokenHash string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		UPDATE auth_verification_tokens
		SET consumed = TRUE
		WHERE token_hash = $1 AND consumed = FALSE AND expires_at > now()
		RETURNING user_id`,
		tokenHash,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", auth.ErrVerificationTokenInvalid
	}
	if err != nil {
		return "", fmt.Errorf("authpg: consume verification token: %w", err)
	}
	return userID, nil
}

// InvalidateForUser implements auth.VerificationStore.
func (s *VerificationStore) InvalidateForUser(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE auth_verification_tokens SET consumed = TRUE WHERE user_id = $1 AND consumed = FALSE`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("authpg: invalidate verification tokens: %w", err)
	}
	return nil
}
