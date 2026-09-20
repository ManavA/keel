package pg_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/pg"
)

func TestSoftDeleteColumnConvention(t *testing.T) {
	assert.Equal(t, "deleted_at", pg.DeletedAtColumn,
		"the column name is the convention services share, so it must not drift")
}

func TestSoftDeleteAddColumn(t *testing.T) {
	ddl, err := pg.AddColumn("listings")
	require.NoError(t, err)
	assert.Equal(t,
		"alter table listings add column if not exists deleted_at timestamptz", ddl)

	qualified, err := pg.AddColumn("public.listings")
	require.NoError(t, err)
	assert.Contains(t, qualified, "public.listings")

	for _, bad := range []string{"", "listings; drop table listings --", `listings"`, "123abc"} {
		_, err := pg.AddColumn(bad)
		assert.Error(t, err, "table names are concatenated into SQL: %q must be refused", bad)
	}
}

func TestSoftDeleteScopeClause(t *testing.T) {
	// The zero value excludes deleted rows, so a Scope nobody sets still
	// cannot leak them.
	clause, err := pg.Scope(0).Clause("")
	require.NoError(t, err)
	assert.Equal(t, "deleted_at is null", clause)

	clause, err = pg.LiveOnly.Clause("")
	require.NoError(t, err)
	assert.Equal(t, "deleted_at is null", clause)

	clause, err = pg.LiveOnly.Clause("archived_at")
	require.NoError(t, err)
	assert.Equal(t, "archived_at is null", clause)

	clause, err = pg.LiveOnly.Clause("listings.deleted_at")
	require.NoError(t, err)
	assert.Equal(t, "listings.deleted_at is null", clause)

	clause, err = pg.WithDeleted.Clause("")
	require.NoError(t, err)
	assert.Empty(t, clause, "including deleted rows is an explicit scope, never a predicate")

	_, err = pg.LiveOnly.Clause("deleted_at; drop table listings --")
	assert.Error(t, err, "column names are concatenated into SQL")

	_, err = pg.Scope(7).Clause("")
	assert.Error(t, err, "an unknown scope must fail rather than guess a visibility")
}

func TestSoftDeleteScopeWhere(t *testing.T) {
	// Paging integration: the scope predicate ANDs with whatever filter the
	// listing already has, e.g. a Keyset.Where seek clause.
	got, err := pg.LiveOnly.Where("", "(created_at, id) > ($1, $2)")
	require.NoError(t, err)
	assert.Equal(t, "deleted_at is null and (created_at, id) > ($1, $2)", got)

	got, err = pg.LiveOnly.Where("", "")
	require.NoError(t, err)
	assert.Equal(t, "deleted_at is null", got)

	got, err = pg.WithDeleted.Where("", "(created_at, id) > ($1, $2)")
	require.NoError(t, err)
	assert.Equal(t, "(created_at, id) > ($1, $2)", got,
		"the explicit scope passes the filter through untouched")

	got, err = pg.WithDeleted.Where("", "")
	require.NoError(t, err)
	assert.Empty(t, got)

	_, err = pg.LiveOnly.Where("deleted_at; drop table listings --", "")
	assert.Error(t, err)
}

// softDeleteTable makes a table without the soft-delete column, so the test
// can prove the migration DDL applies to a populated table.
func softDeleteTable(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	name := fmt.Sprintf("softdel_%d", time.Now().UnixNano())
	_, err := pool.Exec(context.Background(),
		fmt.Sprintf("create table %s (id int primary key, name text not null)", name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "drop table if exists "+name)
	})
	return name
}

func listNames(t *testing.T, pool *pgxpool.Pool, table, where string) []string {
	t.Helper()
	q := "select name from " + table
	if where != "" {
		q += " where " + where
	}
	q += " order by id"
	rows, err := pool.Query(context.Background(), q)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

// TestSoftDeleteDefaultScopeExcludesDeleted is the acceptance check for the
// issue: live and soft-deleted rows go in, and the default listing reads back
// only the live ones.
func TestSoftDeleteDefaultScopeExcludesDeleted(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	table := softDeleteTable(t, pool)

	// Rows predate the column, as in a real migration run against a live table.
	_, err := pool.Exec(ctx, "insert into "+table+" (id, name) values (1, 'kept'), (2, 'doomed')")
	require.NoError(t, err)

	ddl, err := pg.AddColumn(table)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, ddl)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, ddl)
	require.NoError(t, err, "the DDL must be safe to replay: migrations re-run every file")

	_, err = pool.Exec(ctx, "update "+table+" set deleted_at = now() where id = 2")
	require.NoError(t, err)

	def, err := pg.LiveOnly.Where("", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"kept"}, listNames(t, pool, table, def),
		"default listing must exclude the soft-deleted row")

	var zero pg.Scope
	def, err = zero.Where("", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"kept"}, listNames(t, pool, table, def),
		"the zero Scope must exclude deleted rows too: exclusion is the default")

	all, err := pg.WithDeleted.Where("", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"kept", "doomed"}, listNames(t, pool, table, all),
		"deleted rows come back only through the explicit scope")
}

// TestSoftDeleteScopeWithKeysetFilter proves the paging integration end to
// end: a seek clause from Keyset.Where composed with the default scope still
// excludes deleted rows.
func TestSoftDeleteScopeWithKeysetFilter(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	table := softDeleteTable(t, pool)

	ddl, err := pg.AddColumn(table)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, ddl)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "insert into "+table+
		" (id, name, deleted_at) values (1, 'one', null), (2, 'two', now()), (3, 'three', null)")
	require.NoError(t, err)

	seek, args, err := pg.Keyset{
		Sort:  []pg.SortKey{{Column: "id"}},
		After: []any{1},
	}.Where(1)
	require.NoError(t, err)

	where, err := pg.LiveOnly.Where(pg.DeletedAtColumn, seek)
	require.NoError(t, err)

	q := "select name from " + table + " where " + where + " order by id"
	rows, err := pool.Query(ctx, q, args...)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"three"}, names,
		"row 2 is past the cursor but deleted: the scope must still hide it")
}
