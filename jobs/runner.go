package jobs

import (
	"context"
	"log/slog"
	"time"
)

// Func is one job's body. It reports its own Outcome; a non-nil error means
// the job could not even measure its own work, which Run treats as Fatal.
type Func func(ctx context.Context) (Outcome, error)

// RunnerOptions configures a [Runner]. The zero value works: no timeout is
// applied, and logging falls back to [slog.Default].
type RunnerOptions struct {
	// Timeout bounds one run of Func, via context.WithTimeout. Zero means
	// no deadline is added beyond whatever the caller's context already
	// carries.
	Timeout time.Duration
	// Logger receives the run's terminal line. Nil falls back to
	// slog.Default(); Run never calls slog.SetDefault.
	Logger *slog.Logger
}

// Runner runs one job to completion and turns its Outcome into a log line
// and an exit code, via [Complete].
type Runner struct {
	opts RunnerOptions
}

// NewRunner builds a Runner from opts.
func NewRunner(opts RunnerOptions) Runner {
	return Runner{opts: opts}
}

// Run executes fn, bounded by opts.Timeout when it is non-zero, and returns
// the process exit code the run earned. name identifies the job in the
// terminal log line ("<name> complete" / "<name> failed").
func (r Runner) Run(ctx context.Context, name string, fn Func) int {
	if r.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.opts.Timeout)
		defer cancel()
	}

	outcome, err := fn(ctx)
	if err != nil {
		// A job that returns an error could not even measure its own work —
		// distinct from one that measured and found failures, but no less
		// fatal, so it takes the same terminal status.
		outcome.Fatal = true
		return Complete(r.opts.Logger, name+" complete", name+" failed", outcome, "error", err)
	}
	return Complete(r.opts.Logger, name+" complete", name+" failed", outcome)
}
