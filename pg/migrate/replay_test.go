package migrate_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/pg/migrate"
)

func TestReplayPasses(t *testing.T) {
	pool := freshSchema(t)

	previous := files(map[string]string{
		"001_users.up.sql":   `create table if not exists users (id bigserial primary key);`,
		"001_users.down.sql": `drop table if exists users;`,
	})
	current := files(map[string]string{
		"001_users.up.sql":   `create table if not exists users (id bigserial primary key);`,
		"001_users.down.sql": `drop table if exists users;`,
		"002_email.up.sql":   `alter table users add column if not exists email text;`,
		"002_email.down.sql": `alter table users drop column if exists email;`,
	})

	result, err := migrate.Replay(context.Background(), pool, migrate.ReplayOptions{
		Previous: previous,
		Current:  current,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"002_email.up.sql"}, result.ChangedOrAdded)
}

func TestReplayCatchesAMigrationEditedAfterItWasApplied(t *testing.T) {
	// The failure this exists for. Run skips a file the ledger already names,
	// so the edited content never runs on a database that applied the earlier
	// version, and the application runs against a schema the file does not
	// describe.
	pool := freshSchema(t)

	previous := files(map[string]string{
		"001_notes.up.sql": `create table if not exists notes (id bigserial primary key);
			create index if not exists notes_idx on notes (id);`,
	})
	current := files(map[string]string{
		// Same file name, edited: the index name is already taken and the
		// column it now references was never created by the earlier version.
		"001_notes.up.sql": `create table if not exists notes (id bigserial primary key);
			create index notes_idx on notes (author_id);`,
	})

	_, err := migrate.Replay(context.Background(), pool, migrate.ReplayOptions{
		Previous: previous,
		Current:  current,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "001_notes.up.sql")
	assert.Contains(t, err.Error(), "step 2")
}

func TestReplayCatchesANonIdempotentFile(t *testing.T) {
	pool := freshSchema(t)

	previous := fstest.MapFS{}
	current := files(map[string]string{
		// No IF NOT EXISTS: fine once, an error the second time.
		"001_tags.up.sql": `create table tags (id bigserial primary key);`,
	})

	_, err := migrate.Replay(context.Background(), pool, migrate.ReplayOptions{
		Previous: previous,
		Current:  current,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "step 3")
}

func TestReplayCatchesABrokenDownFile(t *testing.T) {
	pool := freshSchema(t)

	current := files(map[string]string{
		"001_tags.up.sql":   `create table if not exists tags (id bigserial primary key);`,
		"001_tags.down.sql": `drop table tags_with_the_wrong_name;`,
	})

	_, err := migrate.Replay(context.Background(), pool, migrate.ReplayOptions{
		Previous: fstest.MapFS{},
		Current:  current,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "step 4")
}

func TestReplayRefusesADeletedMigration(t *testing.T) {
	pool := freshSchema(t)

	previous := files(map[string]string{
		"001_a.up.sql": `create table if not exists a (id int primary key);`,
		"002_b.up.sql": `create table if not exists b (id int primary key);`,
	})
	current := files(map[string]string{
		"001_a.up.sql": `create table if not exists a (id int primary key);`,
	})

	_, err := migrate.Replay(context.Background(), pool, migrate.ReplayOptions{
		Previous: previous,
		Current:  current,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "002_b.up.sql")
}

func TestClassify(t *testing.T) {
	load := func(t *testing.T, entries map[string]string) []migrate.File {
		t.Helper()
		f, err := migrate.Load(files(entries), "")
		require.NoError(t, err)
		return f
	}

	t.Run("a renumbered file is a rename, not a deletion", func(t *testing.T) {
		previous := load(t, map[string]string{"034_x.up.sql": "select 1;"})
		current := load(t, map[string]string{"037_x.up.sql": "select 1;"})

		result, deleted := migrate.Classify(previous, current)
		assert.Empty(t, deleted)
		assert.Equal(t, map[string]string{"034_x.up.sql": "037_x.up.sql"}, result.Renamed)
	})

	t.Run("a deletion is not absorbed by an unrelated sibling", func(t *testing.T) {
		// Matching on the suffix alone lets 035_a, which was already there,
		// claim to be the renamed 031_a and hide a real deletion.
		previous := load(t, map[string]string{
			"031_a.up.sql": "select 1;",
			"035_a.up.sql": "select 2;",
		})
		current := load(t, map[string]string{"035_a.up.sql": "select 2;"})

		result, deleted := migrate.Classify(previous, current)
		assert.Equal(t, []string{"031_a.up.sql"}, deleted)
		assert.Empty(t, result.Renamed)
	})

	t.Run("a renumbered file that also changed is still a rename", func(t *testing.T) {
		previous := load(t, map[string]string{"034_x.up.sql": "select 1;"})
		current := load(t, map[string]string{"037_x.up.sql": "select 2;"})

		result, deleted := migrate.Classify(previous, current)
		assert.Empty(t, deleted)
		assert.Equal(t, "037_x.up.sql", result.Renamed["034_x.up.sql"])
	})

	t.Run("an identical candidate is preferred over an edited one", func(t *testing.T) {
		previous := load(t, map[string]string{"010_x.up.sql": "select 1;"})
		current := load(t, map[string]string{
			"020_x.up.sql": "select 999;",
			"030_x.up.sql": "select 1;",
		})

		result, _ := migrate.Classify(previous, current)
		assert.Equal(t, "030_x.up.sql", result.Renamed["010_x.up.sql"])
	})

	t.Run("changed and added files are reported", func(t *testing.T) {
		previous := load(t, map[string]string{
			"001_a.up.sql": "select 1;",
			"002_b.up.sql": "select 2;",
		})
		current := load(t, map[string]string{
			"001_a.up.sql": "select 1;",
			"002_b.up.sql": "select 22;",
			"003_c.up.sql": "select 3;",
		})

		result, deleted := migrate.Classify(previous, current)
		assert.Empty(t, deleted)
		assert.Equal(t, []string{"002_b.up.sql", "003_c.up.sql"}, result.ChangedOrAdded)
	})
}

func TestLoad(t *testing.T) {
	t.Run("ordered by name, down files ignored", func(t *testing.T) {
		loaded, err := migrate.Load(files(map[string]string{
			"002_b.up.sql":   "select 2;",
			"001_a.up.sql":   "select 1;",
			"001_a.down.sql": "select 0;",
			"README.md":      "not a migration",
		}), "")
		require.NoError(t, err)
		require.Len(t, loaded, 2)
		assert.Equal(t, "001_a.up.sql", loaded[0].Name)
		assert.Equal(t, "002_b.up.sql", loaded[1].Name)
		assert.NotEqual(t, loaded[0].Checksum, loaded[1].Checksum)
	})

	t.Run("mixed prefix widths are refused", func(t *testing.T) {
		// 9_x sorts after 10_y, so an unpadded prefix silently reorders the run.
		_, err := migrate.Load(files(map[string]string{
			"10_y.up.sql": "select 1;",
			"9_x.up.sql":  "select 2;",
		}), "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pad")
	})

	t.Run("a nil filesystem", func(t *testing.T) {
		_, err := migrate.Load(nil, "")
		assert.Error(t, err)
	})
}

func TestDownName(t *testing.T) {
	assert.Equal(t, "001_x.down.sql", migrate.DownName("001_x.up.sql"))
}
