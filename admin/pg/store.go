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

	"github.com/ManavA/keel/admin"
)

// AdminStore is a Postgres-backed admin.AdminStore.
type AdminStore struct {
	pool *pgxpool.Pool
}

// NewAdminStore wraps an already-open pool. The caller owns the pool's
// lifecycle (build it with keel's pg.Open, close it on shutdown).
func NewAdminStore(pool *pgxpool.Pool) *AdminStore {
	return &AdminStore{pool: pool}
}

const adminColumns = `id, email, name, role, password_hash, session_epoch, last_login_at, created_at`

func scanAdmin(row pgx.Row) (*admin.Admin, error) {
	var a admin.Admin
	var lastLoginAt *time.Time
	if err := row.Scan(&a.ID, &a.Email, &a.Name, &a.Role, &a.PasswordHash, &a.SessionEpoch, &lastLoginAt, &a.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, admin.ErrAdminNotFound
		}
		return nil, fmt.Errorf("adminpg: scan admin: %w", err)
	}
	if lastLoginAt != nil {
		a.LastLoginAt = *lastLoginAt
	}
	return &a, nil
}

// GetByEmail implements admin.AdminStore. The comparison is
// case-insensitive (see the citext column type in this package's
// migrations).
func (s *AdminStore) GetByEmail(ctx context.Context, email string) (*admin.Admin, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+adminColumns+` FROM admin_users WHERE email = $1`, email)
	return scanAdmin(row)
}

// GetByID implements admin.AdminStore.
func (s *AdminStore) GetByID(ctx context.Context, id string) (*admin.Admin, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+adminColumns+` FROM admin_users WHERE id = $1`, id)
	return scanAdmin(row)
}

// Create implements admin.AdminStore, generating a new random id.
func (s *AdminStore) Create(ctx context.Context, a *admin.Admin) error {
	id := uuid.NewString()
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO admin_users (id, email, name, role, password_hash)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`,
		id, a.Email, a.Name, a.Role, a.PasswordHash,
	).Scan(&createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return admin.ErrDuplicateEmail
		}
		return fmt.Errorf("adminpg: create admin: %w", err)
	}
	a.ID = id
	a.CreatedAt = createdAt
	return nil
}

// UpdateLastLogin implements admin.AdminStore.
func (s *AdminStore) UpdateLastLogin(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE admin_users SET last_login_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("adminpg: update last login: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return admin.ErrAdminNotFound
	}
	return nil
}

// List implements admin.AdminStore, ordered by email.
func (s *AdminStore) List(ctx context.Context) ([]admin.Admin, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+adminColumns+` FROM admin_users ORDER BY email ASC`)
	if err != nil {
		return nil, fmt.Errorf("adminpg: list admins: %w", err)
	}
	defer rows.Close()
	var out []admin.Admin
	for rows.Next() {
		a, err := scanAdmin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("adminpg: list admins: %w", err)
	}
	return out, nil
}

// RevokeSessions implements admin.AdminStore.
func (s *AdminStore) RevokeSessions(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE admin_users SET session_epoch = session_epoch + 1 WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("adminpg: revoke sessions: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return admin.ErrAdminNotFound
	}
	return nil
}
