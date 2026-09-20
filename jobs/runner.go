package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/ManavA/keel/metrics"
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
	// Metrics receives one count per run under the run's status word.
	// Nil records nothing.
	Metrics *metrics.Metrics
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
// the process exit code that corresponds to the run's outcome. name identifies the job in the
// terminal log line ("<name> complete" / "<name> failed").
func (r Runner) Run(ctx context.Context, name string, fn Func) int {
	_, exit := r.RunOutcome(ctx, name, fn)
	return exit
}

// RunOutcome executes fn like [Runner.Run] and also returns the Outcome the
// exit code was computed from, for callers — the Scheduler's run history —
// that need the counts as well as the code.
func (r Runner) RunOutcome(ctx context.Context, name string, fn Func) (Outcome, int) {
	if r.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.opts.Timeout)
		defer cancel()
	}

	outcome, err := runRecovered(ctx, fn)
	if err != nil {
		// A job that returns an error could not even measure its own work —
		// distinct from one that measured and found failures, but no less
		// fatal, so it takes the same terminal status.
		outcome.Fatal = true
		r.opts.Metrics.ObserveJob(ctx, name, outcome.Status())
		return outcome, Complete(r.opts.Logger, name+" complete", name+" failed", outcome, "error", err)
	}
	r.opts.Metrics.ObserveJob(ctx, name, outcome.Status())
	return outcome, Complete(r.opts.Logger, name+" complete", name+" failed", outcome)
}

// runRecovered calls fn and converts a panic into an error instead of
// letting it propagate. A single job entry panicking must not end the
// calling process: for Scheduler, one entry's process would take down every
// other entry sharing that process, which is the failure mode an external
// scheduler service would not have.
func runRecovered(ctx context.Context, fn Func) (outcome Outcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return fn(ctx)
}
