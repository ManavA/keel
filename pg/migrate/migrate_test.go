package migrate_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/log"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// freshSchema gives each test its own schema on the shared database, so that
// migrations can create tables with fixed names without colliding.
func freshSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Shared(t)
	ctx := context.Background()

	schema := fmt.Sprintf("t%d", len(t.Name())*1_000_000+int(hash(t.Name())%1_000_000))
	admin, err := pg.Open(ctx, pg.Options{URL: db.URL})
	require.NoError(t, err)
	defer admin.Close()

	_, err = admin.Exec(ctx, "drop schema if exists "+schema+" cascade")
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)

	pool, err := pg.Open(ctx, pg.Options{
		URL: db.URL + "&search_path=" + schema,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pg.Open(context.Background(), pg.Options{URL: db.URL})
		if err == nil {
			_, _ = cleanup.Exec(context.Background(), "drop schema if exists "+schema+" cascade")
			cleanup.Close()
		}
	})
	return pool
}

func hash(s string) uint32 {
	var h uint32 = 2166136261
	for i := range len(s) {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func files(entries map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range entries {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

var baseMigrations = map[string]string{
	"001_users.up.sql": `create table if not exists users (
		id bigserial primary key,
		email text not null unique
	);`,
	"001_users.down.sql": `drop table if exists users;`,
	"002_users_name.up.sql": `alter table users add column if not exists name text;
		update users set name = 'unknown' where name is null;`,
	"002_users_name.down.sql": `alter table users drop column if exists name;`,
}

func TestRunAppliesEverythingOnce(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	result, err := migrate.Run(ctx, pool, migrate.Options{FS: files(baseMigrations)})
	require.NoError(t, err)
	assert.Equal(t, []string{"001_users.up.sql", "002_users_name.up.sql"}, result.Applied)
	assert.Empty(t, result.Skipped)

	var columns int
	require.NoError(t, pool.QueryRow(ctx,
		`select count(*) from information_schema.columns
		 where table_name = 'users' and table_schema = current_schema()`).Scan(&columns))
	assert.Equal(t, 3, columns)
}

func TestRunIsIdempotent(t *testing.T) {
	// The job that runs these re-runs every file on every deploy, so a second
	// run must apply nothing and change nothing.
	pool := freshSchema(t)
	ctx := context.Background()

	_, err := migrate.Run(ctx, pool, migrate.Options{FS: files(baseMigrations)})
	require.NoError(t, err)

	second, err := migrate.Run(ctx, pool, migrate.Options{FS: files(baseMigrations)})
	require.NoError(t, err)
	assert.Empty(t, second.Applied)
	assert.Len(t, second.Skipped, 2)
}

func TestRunStopsAtTheFirstFailure(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	broken := map[string]string{
		"001_ok.up.sql":    `create table if not exists a (id int primary key);`,
		"002_bad.up.sql":   `create table b (id int references missing_table(id));`,
		"003_later.up.sql": `create table if not exists c (id int primary key);`,
	}

	result, err := migrate.Run(ctx, pool, migrate.Options{FS: files(broken)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "002_bad.up.sql")
	assert.Equal(t, []string{"001_ok.up.sql"}, result.Applied)

	// Nothing from the failing file, and the later file was not attempted: it
	// may depend on what 002 was supposed to create.
	assert.False(t, tableExists(t, pool, "b"))
	assert.False(t, tableExists(t, pool, "c"))
}

func TestRunLeavesNothingBehindFromAFailedFile(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	half := map[string]string{
		"001_half.up.sql": `create table first_half (id int primary key);
			create table second_half (id int references missing_table(id));`,
	}

	_, err := migrate.Run(ctx, pool, migrate.Options{FS: files(half)})
	require.Error(t, err)
	assert.False(t, tableExists(t, pool, "first_half"),
		"the file and its ledger row share one transaction, so a half-applied migration is impossible")
}

func TestRunReportsAChangedFile(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	_, err := migrate.Run(ctx, pool, migrate.Options{FS: files(baseMigrations)})
	require.NoError(t, err)

	edited := map[string]string{}
	for name, content := range baseMigrations {
		edited[name] = content
	}
	edited["002_users_name.up.sql"] += "\n-- edited after it was applied"

	result, err := migrate.Run(ctx, pool, migrate.Options{FS: files(edited)})
	require.ErrorIs(t, err, migrate.ErrChecksumDrift)
	assert.Equal(t, []string{"002_users_name.up.sql"}, result.Changed)
	assert.Empty(t, result.Applied, "the ledger keys on the name, so the new content does not run")
}

func TestRunBaseline(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	// A database that already has a schema and no ledger.
	_, err := pool.Exec(ctx, `create table users (id bigserial primary key, email text not null unique)`)
	require.NoError(t, err)

	result, err := migrate.Run(ctx, pool, migrate.Options{
		FS:       files(baseMigrations),
		Baseline: "001_users.up.sql",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"001_users.up.sql"}, result.Skipped)
	assert.Equal(t, []string{"002_users_name.up.sql"}, result.Applied,
		"a migration added after the baseline sorts after it and must still run")
}

func TestRunBaselineDoesNothingToAnEmptyDatabase(t *testing.T) {
	pool := freshSchema(t)

	result, err := migrate.Run(context.Background(), pool, migrate.Options{
		FS:       files(baseMigrations),
		Baseline: "001_users.up.sql",
	})
	require.NoError(t, err)
	assert.Len(t, result.Applied, 2)
}

func TestRunRejectsAnEmptyDirectory(t *testing.T) {
	pool := freshSchema(t)
	_, err := migrate.Run(context.Background(), pool, migrate.Options{FS: fstest.MapFS{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no .up.sql files")
}

func TestRunRejectsABadTableName(t *testing.T) {
	pool := freshSchema(t)
	_, err := migrate.Run(context.Background(), pool, migrate.Options{
		FS:    files(baseMigrations),
		Table: "ledger; drop table users",
	})
	require.Error(t, err)
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(context.Background(),
		`select exists (select 1 from information_schema.tables
		 where table_schema = current_schema() and table_name = $1)`, name).Scan(&exists))
	return exists
}

func TestRunWritesNoLedgerRowForAFailedMigration(t *testing.T) {
	// The file and its ledger row share one transaction. If the file's error
	// were swallowed inside that transaction, the row would commit and the
	// migration would be recorded as applied without having run.
	pool := freshSchema(t)
	ctx := context.Background()

	_, err := migrate.Run(ctx, pool, migrate.Options{FS: files(map[string]string{
		"001_bad.up.sql": `create table b (id int references missing_table(id));`,
	})})
	require.Error(t, err)

	var rows int
	require.NoError(t, pool.QueryRow(ctx,
		"select count(*) from schema_migrations where filename = '001_bad.up.sql'").Scan(&rows))
	assert.Zero(t, rows, "a migration that failed must not be recorded as applied")
}

func TestRunReportsChecksumDriftAsAnError(t *testing.T) {
	// The deploy-time call site is `if _, err := migrate.Run(...); err != nil`,
	// and it has to be able to tell drift from a clean run.
	pool := freshSchema(t)
	ctx := context.Background()

	_, err := migrate.Run(ctx, pool, migrate.Options{FS: files(baseMigrations)})
	require.NoError(t, err)

	edited := map[string]string{}
	for name, content := range baseMigrations {
		edited[name] = content
	}
	edited["002_users_name.up.sql"] += "\n-- edited after it was applied"

	result, err := migrate.Run(ctx, pool, migrate.Options{FS: files(edited)})
	require.Error(t, err)
	assert.ErrorIs(t, err, migrate.ErrChecksumDrift)
	assert.Contains(t, err.Error(), "002_users_name.up.sql")
	assert.Equal(t, []string{"002_users_name.up.sql"}, result.Changed,
		"the result is still filled in, so a caller that tolerates drift can see what drifted")
}

func TestLoadRefusesAMixOfPrefixedAndUnprefixedFiles(t *testing.T) {
	// An unprefixed file sorts wherever the alphabet puts it among the
	// numbered ones, which is the same hazard as a mixed prefix width.
	_, err := migrate.Load(files(map[string]string{
		"001_a.up.sql": "select 1;",
		"init.up.sql":  "select 2;",
	}), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "init.up.sql")

	// The control: all-unprefixed is a consistent, if unusual, convention.
	_, err = migrate.Load(files(map[string]string{
		"a.up.sql": "select 1;",
		"b.up.sql": "select 2;",
	}), "")
	assert.NoError(t, err)
}

func TestRunRefusesAQualifiedLedgerTableName(t *testing.T) {
	pool := freshSchema(t)
	_, err := migrate.Run(context.Background(), pool, migrate.Options{
		FS:    files(baseMigrations),
		Table: "public.schema_migrations",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid table name")
}
