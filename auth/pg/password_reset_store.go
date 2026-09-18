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

// PasswordResetStore is a Postgres-backed auth.PasswordResetStore.
type PasswordResetStore struct {
	pool *pgxpool.Pool
}

// NewPasswordResetStore wraps an already-open pool.
func NewPasswordResetStore(pool *pgxpool.Pool) *PasswordResetStore {
	return &PasswordResetStore{pool: pool}
}

// Create implements auth.PasswordResetStore.
//
// A token_hash collision (astronomically unlikely: it is a SHA-256 digest
// of 32 random bytes) fails the insert rather than overwriting the
// existing row — see VerificationStore.Create's identical comment for why
// ON CONFLICT DO UPDATE was the wrong tool here.
func (s *PasswordResetStore) Create(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO auth_password_reset_tokens (token_hash, user_id, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (token_hash) DO NOTHING`,
		tokenHash, userID, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("authpg: create password reset token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("authpg: create password reset token: token_hash collision, another token already uses this hash")
	}
	return nil
}

// ConsumeValid implements auth.PasswordResetStore: validates and
// invalidates the token in one statement, so two concurrent requests
// presenting the same token cannot both succeed.
func (s *PasswordResetStore) ConsumeValid(ctx context.Context, tokenHash string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		UPDATE auth_password_reset_tokens
		SET consumed = TRUE
		WHERE token_hash = $1 AND consumed = FALSE AND expires_at > now()
		RETURNING user_id`,
		tokenHash,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", auth.ErrPasswordResetTokenInvalid
	}
	if err != nil {
		return "", fmt.Errorf("authpg: consume password reset token: %w", err)
	}
	return userID, nil
}

// InvalidateForUser implements auth.PasswordResetStore.
func (s *PasswordResetStore) InvalidateForUser(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE auth_password_reset_tokens SET consumed = TRUE WHERE user_id = $1 AND consumed = FALSE`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("authpg: invalidate password reset tokens: %w", err)
	}
	return nil
}
