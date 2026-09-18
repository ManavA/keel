// Package migrate applies SQL migrations exactly once each and keeps a ledger
// of what ran.
//
// Without a ledger, the usual implementation runs every file on every
// invocation and treats each SQL error as "possibly already applied", exiting 0
// as long as something succeeded. That makes an idempotent re-run and a
// migration with a real bug produce identical output, and the second one skips
// a schema change silently.
//
// So: only pending files run, each inside one transaction together with its own
// ledger row, and any error stops the run naming the file.
//
// Write migrations to be safe to re-apply anyway. The ledger is per database,
// and a new database, a restored one or a dropped ledger table replays
// everything.
//
// Run migrations from one place. There is no advisory lock yet (issue #6), so
// two instances deploying at once can both reach the same pending file. The
// outcome is safe — one transaction wins and the other fails on the ledger's
// primary key or on the DDL, stopping the run and naming the file — but in a
// rolling deploy that shows up as one replica failing while another succeeds,
// which costs an investigation.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ManavA/keel/pg"
)

// DefaultTable is the ledger table.
const DefaultTable = "schema_migrations"

// UpSuffix and DownSuffix are the file names this package reads. A migration
// with no down file is fine and common; one is only needed for a change you
// intend to be able to reverse.
const (
	UpSuffix   = ".up.sql"
	DownSuffix = ".down.sql"
)

// Options configures Run.
type Options struct {
	// FS holds the migration files. os.DirFS("migrations") for a directory,
	// or an embed.FS so the binary carries its own.
	FS fs.FS

	// Dir is the path inside FS. Empty means the root.
	Dir string

	// Table is the ledger table, default DefaultTable.
	Table string

	// Baseline is the last migration already applied to every existing database
	// before this package was introduced. On a database that has a schema but
	// no ledger, everything up to and including Baseline is recorded as applied
	// rather than re-run.
	//
	// Pin it to a file name rather than to whatever is on disk, so a migration
	// added later sorts after it and cannot be skipped by the bootstrap. It
	// describes history and does not move.
	//
	// Empty means no bootstrap, which is correct only if every file is
	// idempotent.
	Baseline string

	// ExistsQuery decides whether a database already has a schema, for the
	// bootstrap above. It must return a single boolean. The default asks
	// whether any non-system table exists.
	ExistsQuery string

	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Result reports what a run did.
type Result struct {
	// Applied names the migrations this run executed, in order.
	Applied []string

	// Skipped names the migrations the ledger already had.
	Skipped []string

	// Changed names migrations whose file no longer matches the checksum
	// recorded when they were applied. Run returns ErrChecksumDrift when this
	// is non-empty, after applying everything pending.
	Changed []string
}

// ErrChecksumDrift reports that a migration file no longer matches the checksum
// recorded when it was applied. Run returns it after applying everything
// pending, so a deploy step written as `if _, err := migrate.Run(...); err != nil`
// stops instead of running the application against a schema its own migrations
// no longer describe.
//
// Drift is normal on a branch that is still being edited. A caller that expects
// it checks for this error and carries on:
//
//	result, err := migrate.Run(ctx, pool, opts)
//	if err != nil && !errors.Is(err, migrate.ErrChecksumDrift) {
//		return err
//	}
//
// Replay is the check that says whether the edited file would actually apply.
var ErrChecksumDrift = errors.New("migrate: a migration changed after it was applied")

// migrationName matches a migration file and captures its sortable prefix.
var migrationName = regexp.MustCompile(`^(\d+)[_-]`)

// Run applies every pending migration.
func Run(ctx context.Context, db pg.Beginner, opts Options) (Result, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	table := opts.Table
	if table == "" {
		table = DefaultTable
	}
	if !tableName.MatchString(table) {
		return Result{}, fmt.Errorf("migrate: %q is not a valid table name", table)
	}

	files, err := Load(opts.FS, opts.Dir)
	if err != nil {
		return Result{}, err
	}
	if len(files) == 0 {
		// Almost always a wrong path or a missing embed pattern. Reporting
		// "0 applied" would be a silent success.
		return Result{}, fmt.Errorf("migrate: no %s files found in %q", UpSuffix, path.Join(opts.Dir, "."))
	}

	if err := ensureLedger(ctx, db, table); err != nil {
		return Result{}, err
	}
	if err := bootstrap(ctx, db, table, files, opts, logger); err != nil {
		return Result{}, err
	}

	applied, err := appliedChecksums(ctx, db, table)
	if err != nil {
		return Result{}, err
	}

	var result Result
	for _, f := range files {
		recorded, already := applied[f.Name]
		if already {
			result.Skipped = append(result.Skipped, f.Name)
			if recorded != "" && recorded != f.Checksum {
				result.Changed = append(result.Changed, f.Name)
				logger.WarnContext(ctx, "migration changed since it was applied",
					"migration", f.Name,
					"ledger_checksum", short(recorded),
					"file_checksum", short(f.Checksum),
				)
			}
			continue
		}

		logger.InfoContext(ctx, "applying migration", "migration", f.Name)
		if err := apply(ctx, db, table, f); err != nil {
			return result, fmt.Errorf("migrate: %s: %w "+
				"(nothing from this file was committed, and later migrations were not attempted "+
				"because they may depend on it)", f.Name, err)
		}
		result.Applied = append(result.Applied, f.Name)
	}

	logger.InfoContext(ctx, "migrations complete",
		"applied", len(result.Applied),
		"already_applied", len(result.Skipped),
	)
	if len(result.Changed) > 0 {
		return result, fmt.Errorf("%w: %s (the ledger already names %s, so the new content will not run; "+
			"use Replay to find out whether it would apply)",
			ErrChecksumDrift, strings.Join(result.Changed, ", "),
			plural(len(result.Changed), "this file", "these files"))
	}
	return result, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// apply runs one migration and records it, in a single transaction, so a
// half-applied migration is impossible. Running the file and the ledger insert
// separately leaves a window in which the schema has changed and nothing says
// so, and the next run applies it again.
//
// Postgres cannot run every statement inside a transaction; CREATE INDEX
// CONCURRENTLY and ALTER TYPE ... ADD VALUE are the common ones. A migration
// needing those has to go in its own file and run outside this path.
func apply(ctx context.Context, db pg.Beginner, table string, f File) error {
	return pg.InTx(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, f.SQL); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			fmt.Sprintf("insert into %s (filename, checksum) values ($1, $2)", table),
			f.Name, f.Checksum)
		return err
	})
}

func ensureLedger(ctx context.Context, db pg.Beginner, table string) error {
	return pg.InTx(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, fmt.Sprintf(`
			create table if not exists %s (
				filename   text primary key,
				checksum   text,
				applied_at timestamptz not null default now()
			)`, table))
		if err != nil {
			return fmt.Errorf("migrate: create ledger %s: %w", table, err)
		}
		return nil
	})
}

// bootstrap records the migrations up to Baseline as applied, on a database
// that already has a schema but no ledger.
func bootstrap(ctx context.Context, db pg.Beginner, table string, files []File, opts Options, logger *slog.Logger) error {
	if opts.Baseline == "" {
		return nil
	}

	return pg.InTx(ctx, db, func(tx pgx.Tx) error {
		var ledgerRows int
		if err := tx.QueryRow(ctx, fmt.Sprintf("select count(*) from %s", table)).Scan(&ledgerRows); err != nil {
			return fmt.Errorf("migrate: read ledger: %w", err)
		}
		if ledgerRows > 0 {
			return nil
		}

		existsQuery := opts.ExistsQuery
		if existsQuery == "" {
			existsQuery = `select exists (
				select 1 from information_schema.tables
				where table_schema = current_schema() and table_name <> $1
			)`
		}
		var hasSchema bool
		if err := tx.QueryRow(ctx, existsQuery, table).Scan(&hasSchema); err != nil {
			return fmt.Errorf("migrate: check for an existing schema: %w", err)
		}
		if !hasSchema {
			logger.InfoContext(ctx, "empty database: applying every migration from the beginning")
			return nil
		}

		logger.InfoContext(ctx, "existing schema with no ledger: recording migrations as applied",
			"through", opts.Baseline)
		for _, f := range files {
			if f.Name > opts.Baseline {
				continue
			}
			_, err := tx.Exec(ctx, fmt.Sprintf(
				"insert into %s (filename, checksum) values ($1, $2) on conflict (filename) do nothing", table),
				f.Name, f.Checksum)
			if err != nil {
				return fmt.Errorf("migrate: baseline %s: %w", f.Name, err)
			}
		}
		return nil
	})
}

func appliedChecksums(ctx context.Context, db pg.Beginner, table string) (map[string]string, error) {
	applied := map[string]string{}
	err := pg.InTxOptions(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, fmt.Sprintf("select filename, coalesce(checksum, '') from %s", table))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var name, sum string
			if err := rows.Scan(&name, &sum); err != nil {
				return err
			}
			applied[name] = sum
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: read ledger: %w", err)
	}
	return applied, nil
}

// File is one migration on disk.
type File struct {
	Name     string
	SQL      string
	Checksum string
}

// Load reads the .up.sql files under dir, in the order they must be applied.
//
// Ordering is lexical over the whole file name, hence the zero-padded numeric
// prefix convention. 9_x.up.sql sorts after 10_y.up.sql, so Load refuses a
// prefix whose width differs from the others.
func Load(fsys fs.FS, dir string) ([]File, error) {
	if fsys == nil {
		return nil, errors.New("migrate: Options.FS is nil")
	}
	if dir == "" {
		dir = "."
	}

	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read %q: %w", dir, err)
	}

	var (
		files      []File
		width      int
		prefixed   string
		unprefixed string
	)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, UpSuffix) {
			continue
		}

		if m := migrationName.FindStringSubmatch(name); m != nil {
			prefixed = name
			if width == 0 {
				width = len(m[1])
			} else if len(m[1]) != width {
				return nil, fmt.Errorf("migrate: %q has a %d-digit prefix where the others have %d; "+
					"pad them all to the same width or they will not sort into the order you meant",
					name, len(m[1]), width)
			}
		} else {
			unprefixed = name
		}

		content, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("migrate: read %q: %w", name, err)
		}
		sum := sha256.Sum256(content)
		files = append(files, File{
			Name:     name,
			SQL:      string(content),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	// A file with no numeric prefix sorts by its bare name, which puts it
	// wherever the alphabet happens to put it among the numbered ones.
	if prefixed != "" && unprefixed != "" {
		return nil, fmt.Errorf("migrate: %q has no numeric prefix while %q does; "+
			"give every migration a zero-padded prefix or the run order is whatever the alphabet decides",
			unprefixed, prefixed)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// DownName is the down file matching an up file, whether or not it exists.
func DownName(upName string) string {
	return strings.TrimSuffix(upName, UpSuffix) + DownSuffix
}

// tableName is what a ledger table may be called. Unqualified on purpose: a
// schema-qualified name would have to be quoted correctly in five statements,
// and the search_path is the right place to choose a schema. Named differently
// from pg.identifier, which permits one dot for a table-qualified column.
var tableName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func short(checksum string) string {
	if len(checksum) <= 12 {
		return checksum
	}
	return checksum[:12]
}
