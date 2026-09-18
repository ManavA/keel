package media

import (
	"context"
	"fmt"
)

// Lister selects the next batch of incomplete items to (re)try, and reports
// the queue's total depth. Depth is separate from the per-run success and
// failure counts: a run can report success=N failed=0 on every execution
// while the backlog stays the same size, if new incomplete items are added
// at the same rate the run processes them. Depth is the only signal that
// distinguishes that case from an empty queue.
type Lister interface {
	Pending(ctx context.Context, limit int) ([]Item, error)
	BacklogDepth(ctx context.Context) (int, error)
}

// FallbackOptions configures RunFallback.
type FallbackOptions struct {
	Options
	// Limit caps how many pending items one sweep pulls from the Lister.
	// <= 0 defaults to 200.
	Limit int
}

func (o FallbackOptions) withDefaults() FallbackOptions {
	o.Options = o.Options.withDefaults()
	if o.Limit <= 0 {
		o.Limit = 200
	}
	return o
}

// FallbackReport is one sweep's outcome. BacklogBefore/After are -1 when the
// Lister could not report depth, which is itself worth surfacing rather than
// silently treated as zero.
type FallbackReport struct {
	Attempted, Succeeded, Skipped, Dropped, Failed int
	BacklogBefore, BacklogAfter                    int
}

// RunFallback runs one sweep over whatever the Lister currently reports as
// pending. It is meant to be scheduled less often, and with a smaller
// Limit, than whatever primary job drains the same queue; RunFallback
// retries items the primary job did not get to or could not complete.
//
// RunFallback calls Run internally with a Lister-selected batch, rather
// than reimplementing retry and idempotence logic. A separate
// implementation for the fallback path could disagree with Run about what
// "done" means for an item.
func RunFallback(ctx context.Context, lister Lister, fetch Fetcher, derive Deriver, st Putter, rec Recorder, opts FallbackOptions) (FallbackReport, error) {
	opts = opts.withDefaults()

	backlogBefore, err := lister.BacklogDepth(ctx)
	if err != nil {
		return FallbackReport{}, fmt.Errorf("media: backlog depth: %w", err)
	}

	items, err := lister.Pending(ctx, opts.Limit)
	if err != nil {
		return FallbackReport{}, fmt.Errorf("media: list pending: %w", err)
	}

	results := Run(ctx, items, fetch, derive, st, rec, opts.Options)

	report := FallbackReport{Attempted: len(items), BacklogBefore: backlogBefore, BacklogAfter: -1}
	for _, r := range results {
		switch {
		case r.Err != nil:
			report.Failed++
		case r.Dropped:
			report.Dropped++
		case r.Skipped:
			report.Skipped++
		default:
			report.Succeeded++
		}
	}

	if after, err := lister.BacklogDepth(ctx); err == nil {
		report.BacklogAfter = after
	} else {
		opts.Logger.Warn("media: backlog depth after sweep failed", "error", err)
	}

	return report, nil
}
