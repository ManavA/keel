package pg_test

import (
	"context"
	"fmt"
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
	policypg "github.com/ManavA/keel/policy/pg"
)

// bareSchemaPool is a pool on a new, empty schema, for the tests that apply the
// migrations themselves.
func bareSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Shared(t)
	ctx := context.Background()

	schema := fmt.Sprintf("policy_pg_%d", schemaCounter.Add(1))
	admin, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL, MaxConns: 2})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	admin.Close()

	pool, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL + "&search_path=" + schema, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrationsFS_HoldsTheUpAndDownFiles(t *testing.T) {
	names, err := fs.Glob(policypg.MigrationsFS, "migrations/*.sql")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"migrations/001_policy_decisions.down.sql",
		"migrations/001_policy_decisions.up.sql",
	}, names)
}

func TestMigrations_CreateTheTableTheDesignDescribes(t *testing.T) {
	pool := bareSchemaPool(t)
	ctx := t.Context()

	res, err := migrate.Run(ctx, pool, migrate.Options{FS: policypg.MigrationsFS, Dir: "migrations"})
	require.NoError(t, err)
	assert.Equal(t, []string{"001_policy_decisions.up.sql"}, res.Applied)

	t.Run("the columns, in order, with their types", func(t *testing.T) {
		rows, err := pool.Query(ctx, `
			select column_name, data_type, is_nullable
			from information_schema.columns
			where table_schema = current_schema() and table_name = $1
			order by ordinal_position`, policypg.Table)
		require.NoError(t, err)
		defer rows.Close()
		var got []string
		for rows.Next() {
			var name, typ, nullable string
			require.NoError(t, rows.Scan(&name, &typ, &nullable))
			got = append(got, fmt.Sprintf("%s %s null=%s", name, typ, nullable))
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, []string{
			"id bigint null=NO",
			"decided_at timestamp with time zone null=NO",
			"kind text null=NO",
			"target text null=NO",
			"attrs jsonb null=NO",
			"effect text null=NO",
			"rule text null=NO",
			"rule_index integer null=NO",
			"matched jsonb null=NO",
			"uncertain jsonb null=NO",
			"policy_version text null=NO",
		}, got)
	})

	t.Run("both indexes", func(t *testing.T) {
		var names []string
		rows, err := pool.Query(ctx, `select indexname from pg_indexes where schemaname = current_schema() and tablename = $1 and indexname not like '%_pkey' order by indexname`, policypg.Table)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var n string
			require.NoError(t, rows.Scan(&n))
			names = append(names, n)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, []string{"policy_decisions_decided_idx", "policy_decisions_rule_idx"}, names)
	})

	t.Run("each index is in the order of the listing", func(t *testing.T) {
		// The time index is the order of the listing; the rule index is the same
		// order within a rule, so that a filter on a rare rule reads only its own
		// rows and does not sort them.
		for name, want := range map[string]string{
			"policy_decisions_decided_idx": "(decided_at DESC, id DESC)",
			"policy_decisions_rule_idx":    "(rule, decided_at DESC, id DESC)",
		} {
			var def string
			require.NoError(t, pool.QueryRow(ctx, `select indexdef from pg_indexes where schemaname = current_schema() and indexname = $1`, name).Scan(&def))
			assert.Contains(t, def, want, name)
		}
	})

	t.Run("the columns that are left out take their defaults", func(t *testing.T) {
		_, err := pool.Exec(ctx, `insert into `+policypg.Table+` (decided_at, kind, effect, rule, rule_index) values ($1, 'k', 'allow', 'r', 0)`, base)
		require.NoError(t, err)
		var target, attrs, matched, uncertain, version string
		require.NoError(t, pool.QueryRow(ctx, `select target, attrs::text, matched::text, uncertain::text, policy_version from `+policypg.Table).
			Scan(&target, &attrs, &matched, &uncertain, &version))
		assert.Equal(t, []string{"", "{}", "[]", "[]", ""}, []string{target, attrs, matched, uncertain, version})
	})

	t.Run("an effect other than the three is refused by the table", func(t *testing.T) {
		for _, effect := range []string{"approve", "ALLOW", ""} {
			_, err := pool.Exec(ctx, `insert into `+policypg.Table+` (decided_at, kind, effect, rule, rule_index) values ($1, 'k', $2, 'r', 0)`, base, effect)
			var pgErr *pgconn.PgError
			require.ErrorAsf(t, err, &pgErr, "effect %q", effect)
			assert.Equal(t, "23514", pgErr.Code, "effect %q", effect)
		}
	})
}

func TestMigrations_SurviveReplay(t *testing.T) {
	pool := bareSchemaPool(t)

	_, err := migrate.Replay(t.Context(), pool, migrate.ReplayOptions{
		Current:    policypg.MigrationsFS,
		CurrentDir: "migrations",
	})
	require.NoError(t, err)
}

func TestMigrations_DownDropsTheTableAndBothIndexes(t *testing.T) {
	pool := bareSchemaPool(t)
	ctx := t.Context()

	_, err := migrate.Run(ctx, pool, migrate.Options{FS: policypg.MigrationsFS, Dir: "migrations"})
	require.NoError(t, err)
	down, err := fs.ReadFile(policypg.MigrationsFS, "migrations/001_policy_decisions.down.sql")
	require.NoError(t, err)

	remaining := func() (table *string, indexes int) {
		require.NoError(t, pool.QueryRow(ctx, `select to_regclass($1)::text`, policypg.Table).Scan(&table))
		require.NoError(t, pool.QueryRow(ctx, `select count(*) from pg_indexes where schemaname = current_schema() and tablename = $1`, policypg.Table).Scan(&indexes))
		return table, indexes
	}
	table, indexes := remaining()
	require.NotNil(t, table)
	require.Equal(t, 3, indexes, "the primary key and the two indexes")

	_, err = pool.Exec(ctx, string(down))
	require.NoError(t, err)
	table, indexes = remaining()
	assert.Nil(t, table)
	assert.Zero(t, indexes)

	// And it can be run twice, as every migration here must be.
	_, err = pool.Exec(ctx, string(down))
	require.NoError(t, err)
}
