package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/auth"
)

// UserStore is a Postgres-backed auth.UserStore.
type UserStore struct {
	pool *pgxpool.Pool
}

// NewUserStore wraps an already-open pool. The caller owns the pool's
// lifecycle (build it with keel's pg.Open, close it on shutdown).
func NewUserStore(pool *pgxpool.Pool) *UserStore {
	return &UserStore{pool: pool}
}

const userColumns = `id, email, name, password_hash, external_uid, email_verified, created_at`

func scanUser(row pgx.Row) (*auth.User, error) {
	var u auth.User
	var externalUID *string
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &externalUID, &u.EmailVerified, &u.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, auth.ErrUserNotFound
		}
		return nil, fmt.Errorf("authpg: scan user: %w", err)
	}
	if externalUID != nil {
		u.ExternalUID = *externalUID
	}
	return &u, nil
}

// GetByEmail implements auth.UserStore. The comparison is case-insensitive
// (see the citext column type in this package's migrations).
func (s *UserStore) GetByEmail(ctx context.Context, email string) (*auth.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM auth_users WHERE email = $1`, email)
	return scanUser(row)
}

// GetByID implements auth.UserStore.
func (s *UserStore) GetByID(ctx context.Context, id string) (*auth.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM auth_users WHERE id = $1`, id)
	return scanUser(row)
}

// GetByExternalUID implements auth.UserStore.
func (s *UserStore) GetByExternalUID(ctx context.Context, externalUID string) (*auth.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM auth_users WHERE external_uid = $1`, externalUID)
	return scanUser(row)
}

// Create implements auth.UserStore, generating a new random id.
func (s *UserStore) Create(ctx context.Context, u *auth.User) error {
	id := uuid.NewString()
	var externalUID *string
	if u.ExternalUID != "" {
		externalUID = &u.ExternalUID
	}
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO auth_users (id, email, name, password_hash, external_uid, email_verified)
		VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6)
		RETURNING created_at`,
		id, u.Email, u.Name, u.PasswordHash, externalUID, u.EmailVerified,
	).Scan(&createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return auth.ErrDuplicateEmail
		}
		return fmt.Errorf("authpg: create user: %w", err)
	}
	u.ID = id
	u.CreatedAt = createdAt
	return nil
}

// SetEmailVerified implements auth.UserStore.
func (s *UserStore) SetEmailVerified(ctx context.Context, id string) error {
	return s.exec1(ctx, `UPDATE auth_users SET email_verified = TRUE WHERE id = $1`, id)
}

// SetPasswordHash implements auth.UserStore.
func (s *UserStore) SetPasswordHash(ctx context.Context, id, passwordHash string) error {
	return s.exec1(ctx, `UPDATE auth_users SET password_hash = $2 WHERE id = $1`, id, passwordHash)
}

// LinkExternalUID implements auth.UserStore.
func (s *UserStore) LinkExternalUID(ctx context.Context, id, externalUID string) error {
	return s.exec1(ctx, `UPDATE auth_users SET external_uid = $2 WHERE id = $1`, id, externalUID)
}

// Delete implements auth.UserStore.
func (s *UserStore) Delete(ctx context.Context, id string) error {
	return s.exec1(ctx, `DELETE FROM auth_users WHERE id = $1`, id)
}

// exec1 runs a statement expected to affect exactly one row and translates
// "affected zero rows" into auth.ErrUserNotFound, the same answer the
// in-memory store gives for an id that does not exist.
func (s *UserStore) exec1(ctx context.Context, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("authpg: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return auth.ErrUserNotFound
	}
	return nil
}
