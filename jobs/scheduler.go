package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Entry is one job on a fixed interval.
type Entry struct {
	// Name identifies the entry in log lines and in the error returned by
	// [Scheduler.Register] for a duplicate name.
	Name string
	// Interval is how often Func runs. Must be positive.
	Interval time.Duration
	// Func is the job body, run once per tick.
	Func Func
	// History receives one [RunRecord] per finished run. Nil records
	// nothing: an entry without a store runs exactly as before.
	History HistoryStore
}

// SchedulerOptions configures a [Scheduler]. The zero value works: logging
// falls back to [slog.Default].
type SchedulerOptions struct {
	// Logger receives each entry's terminal line. Nil falls back to
	// slog.Default(); Scheduler never calls slog.SetDefault.
	Logger *slog.Logger
}

// Scheduler runs a fixed set of [Entry] values, each on its own ticker,
// inside a single process. No external scheduler service is required.
//
// This is not a cron expression parser. Every entry runs on a plain fixed
// interval, which covers most background job schedules ("every 30
// minutes"). A caller that needs calendar-aware scheduling (specific times
// of day, DST-aware intervals) should use a dedicated cron library and use
// this package only for [Outcome] and [Complete].
type Scheduler struct {
	opts    SchedulerOptions
	entries []Entry
	seen    map[string]bool
}

// NewScheduler builds an empty Scheduler.
func NewScheduler(opts SchedulerOptions) *Scheduler {
	return &Scheduler{opts: opts, seen: make(map[string]bool)}
}

// Register adds e to the scheduler. It returns an error if e.Interval is
// not positive, or if e.Name duplicates an already-registered entry: two
// entries with the same name would produce log lines that cannot be told
// apart.
func (s *Scheduler) Register(e Entry) error {
	if e.Interval <= 0 {
		return fmt.Errorf("register %q: interval must be positive, got %s", e.Name, e.Interval)
	}
	if s.seen[e.Name] {
		return fmt.Errorf("register %q: an entry with this name is already registered", e.Name)
	}
	s.seen[e.Name] = true
	s.entries = append(s.entries, e)
	return nil
}

// Run starts every registered entry on its own ticker and blocks until ctx
// is canceled. Each entry runs once immediately and then every Interval;
// entries run concurrently with each other, and a slow or hung entry never
// delays another entry's ticks.
func (s *Scheduler) Run(ctx context.Context) {
	runner := NewRunner(RunnerOptions{Logger: s.opts.Logger})

	done := make(chan struct{})
	for _, e := range s.entries {
		go func(e Entry) {
			// Runner.Run already recovers a panic inside e.Func. This
			// second recover is defense in depth against a panic in
			// runLoop itself (the ticker logic, not the job body): without
			// it, that goroutine would never send on done, and Run below
			// would block forever instead of returning.
			defer func() {
				if p := recover(); p != nil {
					logger := s.opts.Logger
					if logger == nil {
						logger = slog.Default()
					}
					logger.Error("scheduler entry loop panicked", "entry", e.Name, "panic", p)
				}
				done <- struct{}{}
			}()
			s.runLoop(ctx, runner, e)
		}(e)
	}
	for range s.entries {
		<-done
	}
}

// runLoop runs one entry immediately and then on every tick of its own
// ticker, until ctx is canceled.
func (s *Scheduler) runLoop(ctx context.Context, runner Runner, e Entry) {
	s.runOnce(ctx, runner, e)

	ticker := time.NewTicker(e.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx, runner, e)
		}
	}
}

// runOnce runs one tick of e and records it when the entry carries a
// [HistoryStore]. A recording failure is logged, not returned: the job
// already ran, and failing the process over its history would turn the
// record into the run's most dangerous side effect.
func (s *Scheduler) runOnce(ctx context.Context, runner Runner, e Entry) {
	if e.History == nil {
		runner.Run(ctx, e.Name, e.Func)
		return
	}
	started := time.Now()
	outcome, _ := runner.RunOutcome(ctx, e.Name, e.Func)
	if err := e.History.Record(ctx, NewRunRecord(e.Name, started, time.Now(), outcome)); err != nil {
		logger := s.opts.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("scheduler history record failed", "entry", e.Name, "error", err)
	}
}
