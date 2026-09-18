package migrate_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
)

// Replay is only as useful as the directories it is pointed at. The rest of the
// tests in this package build their own fixtures, so a real migrations
// directory anywhere in the module could fail Replay without any test noticing.
//
// This discovers every migrations directory in the module and replays each one
// against a fresh schema. The control below proves it can fail.

// findModuleRoot walks up from the working directory to the directory holding
// go.mod.
func findModuleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.mod above %s", dir)
		dir = parent
	}
}

// discoverMigrationDirs returns every directory named "migrations" under root
// that holds at least one .up.sql file, as paths relative to root.
func discoverMigrationDirs(t *testing.T, root string) []string {
	t.Helper()

	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		switch d.Name() {
		case ".git", "node_modules", "vendor":
			return filepath.SkipDir
		}
		if d.Name() != "migrations" {
			return nil
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), migrate.UpSuffix) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				dirs = append(dirs, rel)
				return filepath.SkipDir
			}
		}
		return nil
	})
	require.NoError(t, err)
	return dirs
}

// replayInFreshSchema runs Replay over fsys in a schema of its own, so two
// directories that both create a "users" table do not collide.
func replayInFreshSchema(ctx context.Context, dbURL, schema string, fsys fs.FS) error {
	admin, err := pg.Open(ctx, pg.Options{URL: dbURL})
	if err != nil {
		return err
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx, "drop schema if exists "+schema+" cascade"); err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, "create schema "+schema); err != nil {
		return err
	}
	defer func() {
		_, _ = admin.Exec(context.WithoutCancel(ctx), "drop schema if exists "+schema+" cascade")
	}()

	pool, err := pg.Open(ctx, pg.Options{URL: dbURL + "&search_path=" + schema})
	if err != nil {
		return err
	}
	defer pool.Close()

	// Previous is nil: the empty set. Replay then applies the current set to an
	// empty schema, applies it again to check idempotence, and takes every file
	// down and up, which is where a down that cannot run shows up.
	_, err = migrate.Replay(ctx, pool, migrate.ReplayOptions{Current: fsys})
	return err
}

func TestRepositoryMigrationsReplay(t *testing.T) {
	db := testdb.Shared(t)
	ctx := context.Background()

	root := findModuleRoot(t)
	dirs := discoverMigrationDirs(t, root)

	// A discovery that finds nothing would pass silently and keep passing after
	// somebody moved the migrations. The example ships one, so there is always
	// at least that.
	require.NotEmpty(t, dirs, "no migrations directories found under %s", root)
	t.Logf("replaying %d migrations director(y|ies): %v", len(dirs), dirs)

	for _, dir := range dirs {
		t.Run(dir, func(t *testing.T) {
			schema := "replay_" + strings.NewReplacer("/", "_", "-", "_", ".", "_").Replace(dir)
			err := replayInFreshSchema(ctx, db.URL, schema, os.DirFS(filepath.Join(root, dir)))
			assert.NoError(t, err, "migrations in %s do not replay cleanly", dir)
		})
	}
}

// brokenDown is the failure this check exists for: a down file that drops a
// table another object depends on, without CASCADE. The up applies, the
// re-apply is clean, and only the down/up round trip fails.
var brokenDown = fstest.MapFS{
	"001_widgets.up.sql": &fstest.MapFile{Data: []byte(`
		CREATE TABLE IF NOT EXISTS widgets (id INT PRIMARY KEY);
		CREATE OR REPLACE VIEW widget_ids AS SELECT id FROM widgets;
	`)},
	"001_widgets.down.sql": &fstest.MapFile{Data: []byte(`DROP TABLE IF EXISTS widgets;`)},
}

// fixedDown is the same migration with the down that works.
var fixedDown = fstest.MapFS{
	"001_widgets.up.sql":   brokenDown["001_widgets.up.sql"],
	"001_widgets.down.sql": &fstest.MapFile{Data: []byte(`DROP TABLE IF EXISTS widgets CASCADE;`)},
}

func TestRepositoryMigrationsCheckCanFail(t *testing.T) {
	// The control. Without it, a discovery that silently stopped walking, or a
	// Replay call that stopped checking down files, would leave this gate green
	// forever.
	db := testdb.Shared(t)
	ctx := context.Background()

	err := replayInFreshSchema(ctx, db.URL, "replay_control_broken", brokenDown)
	require.Error(t, err, "a down file missing CASCADE must fail the check")
	assert.Contains(t, err.Error(), "001_widgets.down.sql")
	assert.Contains(t, err.Error(), "step 4", "the failing step must be named")

	// And the same migration with CASCADE passes, so the check is reacting to
	// the defect rather than to the fixture.
	assert.NoError(t, replayInFreshSchema(ctx, db.URL, "replay_control_fixed", fixedDown))
}

func TestDiscoverMigrationDirs(t *testing.T) {
	root := findModuleRoot(t)
	dirs := discoverMigrationDirs(t, root)

	assert.Contains(t, dirs, filepath.Join("examples", "minimal", "migrations"),
		"the example's migrations must be discovered, or the walk is not reaching the whole module")

	for _, dir := range dirs {
		assert.NotContains(t, dir, ".git")
		assert.True(t, strings.HasSuffix(dir, "migrations"), "discovered %q", dir)
	}
}
