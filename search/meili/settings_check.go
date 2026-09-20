package meili

import (
	"context"
	"fmt"

	"github.com/ManavA/keel/jobs"
)

// DriftCheck returns the scheduled-job form of the settings-drift check: it
// compares the live index settings against [Config] on every run and fails
// loudly on drift. Wire it to a [jobs.Scheduler] entry:
//
//	scheduler.Register(jobs.Entry{
//		Name:     "search-settings-drift",
//		Interval: time.Hour,
//		Func:     searcher.DriftCheck(),
//	})
//
// A clean index reports Attempted 1, Succeeded 1. A drifted index returns an
// error carrying the drift description, so the run exits non-zero and logs at
// Error — a scheduler that only retries non-zero exits retries exactly this
// case. An index whose settings could not be read returns an error with
// nothing attempted: unmeasured is not clean. See [Searcher.Health] for what
// ctx can and cannot do here.
func (s *Searcher) DriftCheck() jobs.Func {
	return func(ctx context.Context) (jobs.Outcome, error) {
		report, err := s.CheckSettings(ctx)
		if err != nil {
			// Fatal, not a failure count: nothing was measured, so there is
			// nothing to count — "could not check" is not "checked, clean".
			return jobs.Outcome{Fatal: true}, fmt.Errorf("check search index settings: %w", err)
		}
		if !report.OK() {
			return jobs.Outcome{Attempted: 1, Failed: 1},
				fmt.Errorf("search index settings drifted: %s", report.Describe())
		}
		return jobs.Outcome{Attempted: 1, Succeeded: 1}, nil
	}
}

// RequireSettings is the startup gate for the settings-drift check: it
// compares the live index settings against [Config] and refuses to start on
// drift, returning an error that names the drifted setting and its
// user-visible consequence. An index whose settings could not be read is also
// refused — "could not check" must never pass for "checked, and fine".
//
// Call this after the settings have had a chance to settle. [Searcher.SetupIndex]
// applies settings asynchronously, so gating immediately after it in the same
// process reports drift that is still enqueued; prefer
// [Searcher.SetupIndexAndVerify] there, which waits and then checks. This gate
// is for the moment the process is willing to serve on the index as it stands.
// To repair drift, see the Runbook section in the package doc.
// See [Searcher.Health] for what ctx can and cannot do here.
func (s *Searcher) RequireSettings(ctx context.Context) error {
	report, err := s.CheckSettings(ctx)
	if err != nil {
		return fmt.Errorf("search index settings gate refused: %s: %w", report.Describe(), err)
	}
	if !report.OK() {
		return fmt.Errorf("search index settings gate refused: %s", report.Describe())
	}
	return nil
}
