package pg

import (
	"fmt"
	"strings"
)

// DeletedAtColumn is the shared soft-delete column: NULL means live, and a
// timestamp means deleted at that time.
//
// The column is always nullable with no default, so adding it to a populated
// table is a metadata-only change: Postgres rewrites no rows, and reads and
// writes proceed while the migration runs. AddColumn renders that DDL.
const DeletedAtColumn = "deleted_at"

// Scope decides whether a listing sees soft-deleted rows. The zero value is
// LiveOnly, so a Scope nobody sets still excludes deleted rows; only an
// explicit WithDeleted lets them through.
type Scope int

const (
	// LiveOnly lists rows whose soft-delete column is NULL. It is the zero
	// value, so exclusion is the default.
	LiveOnly Scope = iota
	// WithDeleted lists every row, including soft-deleted ones.
	WithDeleted
)

// AddColumn returns migration-safe DDL adding the soft-delete column to table:
// nullable with no default, and IF NOT EXISTS so a replayed migration does
// not fail. table may be schema-qualified; anything else that is not a plain
// identifier is refused, since the name is concatenated into SQL.
func AddColumn(table string) (string, error) {
	if !identifier.MatchString(table) {
		return "", fmt.Errorf("pg: %q is not a valid table name", table)
	}
	return fmt.Sprintf("alter table %s add column if not exists %s timestamptz",
		table, DeletedAtColumn), nil
}

// Clause returns the WHERE predicate for the scope: "<column> IS NULL" for
// LiveOnly, and empty for WithDeleted, which reads everything and needs no
// predicate. An empty column defaults to DeletedAtColumn; a qualified name
// such as listings.deleted_at is allowed, for queries that join. A name that
// is not a plain identifier is refused, since it is concatenated into SQL.
func (s Scope) Clause(column string) (string, error) {
	switch s {
	case WithDeleted:
		return "", nil
	case LiveOnly:
		if column == "" {
			column = DeletedAtColumn
		}
		if !identifier.MatchString(column) {
			return "", fmt.Errorf("pg: %q is not a valid column name", column)
		}
		return column + " is null", nil
	default:
		return "", fmt.Errorf("pg: unknown soft-delete scope %d", int(s))
	}
}

// Where ANDs the scope predicate with filter, which is whatever WHERE the
// listing already has — for example a Keyset.Where seek clause. Either side
// may be empty, and the result is always a valid WHERE body: the scope alone,
// the filter alone, both joined, or empty when WithDeleted meets no filter.
func (s Scope) Where(column, filter string) (string, error) {
	clause, err := s.Clause(column)
	if err != nil {
		return "", err
	}
	filter = strings.TrimSpace(filter)
	switch {
	case clause == "":
		return filter, nil
	case filter == "":
		return clause, nil
	default:
		return clause + " and " + filter, nil
	}
}
