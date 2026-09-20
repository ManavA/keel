package pg

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/admin"
)

// auditConn is the subset of *pgxpool.Pool this audit store needs. Both a
// pool and a pgx.Tx satisfy it, so a handler that changes domain state
// inside a transaction builds its AuditStore over the tx and the audit row
// commits or rolls back with the change it records.
type auditConn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AuditStore is a Postgres-backed admin.AuditStore.
type AuditStore struct {
	db auditConn
}

var _ admin.AuditStore = (*AuditStore)(nil)

// NewAuditStore builds an AuditStore over db, which must already have the
// schema from this package's migrations applied. Pass a pgx.Tx instead of
// the pool to record entries in the caller's transaction.
func NewAuditStore(db auditConn) *AuditStore {
	return &AuditStore{db: db}
}

// Append implements admin.AuditStore, setting the entry's ID and CreatedAt.
func (s *AuditStore) Append(ctx context.Context, e *admin.AuditEntry) error {
	err := s.db.QueryRow(ctx, `
		INSERT INTO admin_audit (actor, action, target, outcome)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`,
		e.Actor, e.Action, e.Target, e.Outcome,
	).Scan(&e.ID, &e.CreatedAt)
	if err != nil {
		return fmt.Errorf("adminpg: append audit entry: %w", err)
	}
	return nil
}

// List implements admin.AuditStore, oldest first and narrowed by filter. A
// non-positive limit means the default page of 50.
func (s *AuditStore) List(ctx context.Context, filter admin.AuditFilter, limit, offset int) ([]admin.AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	query := `
		SELECT id, actor, action, target, outcome, created_at
		FROM admin_audit`
	var conds []string
	var args []any
	if filter.Actor != "" {
		args = append(args, filter.Actor)
		conds = append(conds, `actor = $`+strconv.Itoa(len(args)))
	}
	if filter.Action != "" {
		args = append(args, filter.Action)
		conds = append(conds, `action = $`+strconv.Itoa(len(args)))
	}
	if filter.Outcome != "" {
		args = append(args, filter.Outcome)
		conds = append(conds, `outcome = $`+strconv.Itoa(len(args)))
	}
	if len(conds) > 0 {
		query += "\nWHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, offset)
	query += `
		ORDER BY id ASC
		LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("adminpg: list audit entries: %w", err)
	}
	defer rows.Close()
	var out []admin.AuditEntry
	for rows.Next() {
		var e admin.AuditEntry
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &e.Outcome, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("adminpg: scan audit entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("adminpg: list audit entries: %w", err)
	}
	return out, nil
}
