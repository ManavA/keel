package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ManavA/keel/pg"
)

// ReplayOptions configures Replay.
type ReplayOptions struct {
	// Previous is the migration set as it was at an earlier revision, which is
	// the state a long-lived development or staging database may be in.
	Previous    fs.FS
	PreviousDir string

	// Current is the migration set now.
	Current    fs.FS
	CurrentDir string

	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// ReplayResult reports what Replay compared and ran.
type ReplayResult struct {
	// ChangedOrAdded names files that differ from, or are absent at, Previous.
	ChangedOrAdded []string

	// Renamed pairs a file at Previous with the file at Current that appears to
	// be the same migration renumbered.
	Renamed map[string]string
}

// Replay applies the previous revision's migrations to an empty database, then
// the current revision's on top, then the current revision's again, then takes
// each changed or added file down and up.
//
// The ledger keys on the file name, so a migration edited while unmerged never
// runs its new content on a database that applied the old content: the name
// matches, the file is skipped, and the application runs against a schema the
// file does not describe. This reproduces that on a throwaway database.
//
// Every statement goes straight to the database rather than through Run, which
// would skip the files under test.
//
// It does not catch a changed migration that applies cleanly and leaves a
// different schema. `add column if not exists x text` at the previous revision
// and `... x integer` now is a no-op the second time and the column stays text.
// Detecting that needs a schema dump comparison.
func Replay(ctx context.Context, db pg.Beginner, opts ReplayOptions) (ReplayResult, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	previous, err := Load(opts.Previous, opts.PreviousDir)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("migrate: previous revision: %w", err)
	}
	current, err := Load(opts.Current, opts.CurrentDir)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("migrate: current revision: %w", err)
	}
	if len(current) == 0 {
		return ReplayResult{}, fmt.Errorf("migrate: the current revision has no %s files", UpSuffix)
	}

	result, deleted := Classify(previous, current)
	if len(deleted) > 0 {
		// Nothing later in this function would mention a file that no longer
		// exists, so a deletion has to be caught here.
		return result, fmt.Errorf("migrate: %s present at the previous revision and deleted since; "+
			"a database that already applied it cannot be reproduced", strings.Join(deleted, ", "))
	}
	for from, to := range result.Renamed {
		logger.WarnContext(ctx, "migration renumbered", "from", from, "to", to)
	}

	for _, f := range previous {
		if err := execFile(ctx, db, f); err != nil {
			return result, fmt.Errorf("migrate: replay step 1 (previous revision): %w", err)
		}
	}
	for _, f := range current {
		if err := execFile(ctx, db, f); err != nil {
			return result, fmt.Errorf("migrate: replay step 2 (current revision over the previous schema): %w", err)
		}
	}
	for _, f := range current {
		if err := execFile(ctx, db, f); err != nil {
			return result, fmt.Errorf("migrate: replay step 3 (re-applying the current revision): %w", err)
		}
	}

	byName := map[string]File{}
	for _, f := range current {
		byName[f.Name] = f
	}
	downs, err := loadDowns(opts.Current, opts.CurrentDir)
	if err != nil {
		return result, err
	}
	for _, name := range result.ChangedOrAdded {
		down, ok := downs[DownName(name)]
		if !ok {
			continue
		}
		if err := execFile(ctx, db, down); err != nil {
			return result, fmt.Errorf("migrate: replay step 4 (down): %w", err)
		}
		if err := execFile(ctx, db, byName[name]); err != nil {
			return result, fmt.Errorf("migrate: replay step 4 (up again): %w", err)
		}
	}

	logger.InfoContext(ctx, "migrations replay cleanly",
		"previous_files", len(previous),
		"current_files", len(current),
		"changed_or_added", len(result.ChangedOrAdded),
	)
	return result, nil
}

// execFile runs one migration outside the ledger, in its own transaction.
func execFile(ctx context.Context, db pg.Beginner, f File) error {
	err := pg.InTx(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, f.SQL)
		return err
	})
	if err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	return nil
}

func loadDowns(fsys fs.FS, dir string) (map[string]File, error) {
	if dir == "" {
		dir = "."
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read %q: %w", dir, err)
	}

	downs := map[string]File{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), DownSuffix) {
			continue
		}
		content, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrate: read %q: %w", e.Name(), err)
		}
		downs[e.Name()] = File{Name: e.Name(), SQL: string(content)}
	}
	return downs, nil
}

var numericPrefix = regexp.MustCompile(`^\d+[_-]`)

// Classify compares two migration sets and reports which files changed or were
// added, and which were deleted.
//
// A file that disappears between revisions is usually a renumber, not a
// deletion: two branches take the same number and one of them moves. A previous
// file with no match now is treated as renamed when a file that is itself new
// carries the same name after the numeric prefix. The partner has to be new, or
// an unrelated file sharing a suffix can absorb a real deletion.
func Classify(previous, current []File) (ReplayResult, []string) {
	result := ReplayResult{Renamed: map[string]string{}}

	previousByName := map[string]File{}
	for _, f := range previous {
		previousByName[f.Name] = f
	}
	newAtCurrent := map[string]File{}
	for _, f := range current {
		if _, existed := previousByName[f.Name]; !existed {
			newAtCurrent[f.Name] = f
		}
		if before, existed := previousByName[f.Name]; !existed || before.Checksum != f.Checksum {
			result.ChangedOrAdded = append(result.ChangedOrAdded, f.Name)
		}
	}
	slices.Sort(result.ChangedOrAdded)

	currentNames := map[string]bool{}
	for _, f := range current {
		currentNames[f.Name] = true
	}

	var deleted []string
	for _, before := range previous {
		if currentNames[before.Name] {
			continue
		}
		if partner := renamePartner(before, newAtCurrent); partner != "" {
			result.Renamed[before.Name] = partner
			delete(newAtCurrent, partner)
			continue
		}
		deleted = append(deleted, before.Name)
	}
	slices.Sort(deleted)
	return result, deleted
}

// renamePartner finds a file that is new at the current revision and carries
// the same name once the numeric prefix is removed.
func renamePartner(before File, newAtCurrent map[string]File) string {
	want := numericPrefix.ReplaceAllString(before.Name, "")
	names := make([]string, 0, len(newAtCurrent))
	for name := range newAtCurrent {
		names = append(names, name)
	}
	slices.Sort(names) // deterministic when two candidates match

	// An identical file first, so a pure renumber does not lose its partner to a
	// renumbered-and-edited candidate.
	for _, name := range names {
		if numericPrefix.ReplaceAllString(name, "") == want &&
			newAtCurrent[name].Checksum == before.Checksum {
			return name
		}
	}
	for _, name := range names {
		if numericPrefix.ReplaceAllString(name, "") == want {
			return name
		}
	}
	return ""
}
