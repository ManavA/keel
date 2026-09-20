package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/auth"
)

// AttemptStore is a Postgres-backed auth.AttemptStore: the login-attempt
// audit trail behind the per-account throttle. Rows reference no user id on
// purpose — attempts against unregistered addresses are recorded too — so
// there is no foreign key to auth_users and deleting an account leaves its
// attempt history in place.
type AttemptStore struct {
	pool *pgxpool.Pool
}

// NewAttemptStore wraps an already-open pool.
func NewAttemptStore(pool *pgxpool.Pool) *AttemptStore {
	return &AttemptStore{pool: pool}
}

// Record implements auth.AttemptStore. A zero At defaults to now, the same
// as MemoryAttemptStore.
func (s *AttemptStore) Record(ctx context.Context, attempt auth.LoginAttempt) error {
	at := attempt.At
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO auth_login_attempts (email, success, ip, attempted_at)
		VALUES ($1, $2, $3, $4)`,
		attempt.Email, attempt.Success, attempt.IP, at,
	)
	if err != nil {
		return fmt.Errorf("authpg: record login attempt: %w", err)
	}
	return nil
}

// FailuresSince implements auth.AttemptStore.
func (s *AttemptStore) FailuresSince(ctx context.Context, email string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM auth_login_attempts
		WHERE email = $1 AND success = FALSE AND attempted_at >= $2`,
		email, since,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("authpg: count recent login failures: %w", err)
	}
	return n, nil
}

// Recent implements auth.AttemptStore.
func (s *AttemptStore) Recent(ctx context.Context, email string, limit int) ([]auth.LoginAttempt, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT email, success, ip, attempted_at FROM auth_login_attempts
		WHERE email = $1
		ORDER BY attempted_at DESC, id DESC
		LIMIT $2`,
		email, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("authpg: list recent login attempts: %w", err)
	}
	defer rows.Close()

	var out []auth.LoginAttempt
	for rows.Next() {
		var a auth.LoginAttempt
		if err := rows.Scan(&a.Email, &a.Success, &a.IP, &a.At); err != nil {
			return nil, fmt.Errorf("authpg: scan login attempt: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authpg: list recent login attempts: %w", err)
	}
	return out, nil
}
