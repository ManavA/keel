package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Tick is one pass: it lapses overdue approvals, claims runnable runs, up
// to Concurrency of them, executes each until it ends or parks, and
// returns when all have.
//
// The runs are claimed first and then executed at once, so a run made
// runnable by the pass, a child it started or a parent it woke, waits for
// the next. The error is the pass's own: the store refusing to lapse
// approvals, when nothing is claimed, or to claim, when what was already
// claimed is still executed. An execution that fails is in the Report, as
// Yielded when the run was given back, and in the log.
func (e *Engine) Tick(ctx context.Context) (Report, error) {
	var report Report
	if _, err := e.store.ExpireApprovals(ctx, e.clock.Now()); err != nil {
		return report, fmt.Errorf("agent: tick: lapse overdue approvals: %w", err)
	}

	var (
		claimed []Run
		failure error
	)
	for len(claimed) < e.opts.Concurrency {
		run, err := e.claim(ctx, "")
		if err != nil {
			failure = fmt.Errorf("agent: tick: claim a run: %w", err)
			break
		}
		if run == nil {
			break
		}
		claimed = append(claimed, *run)
	}
	report.Claimed = len(claimed)

	endings := make([]ending, len(claimed))
	var executions sync.WaitGroup
	for i, run := range claimed {
		executions.Go(func() { endings[i], _ = e.execute(ctx, run) })
	}
	executions.Wait()

	for _, end := range endings {
		switch end {
		case endedCompleted:
			report.Completed++
		case endedFailed:
			report.Failed++
		case endedCancelled:
			report.Cancelled++
		case endedParked:
			report.Parked++
		case endedYielded:
			report.Yielded++
		}
	}
	return report, failure
}

// Work keeps up to Concurrency executions going until ctx is cancelled:
// it claims a run whenever a slot is free and looks again after
// PollInterval when there is none. Then it waits up to DrainTimeout for
// steps in flight and returns ctx.Err().
//
// It also lapses overdue approvals, once as it starts and then every
// PollInterval, as Tick does on every pass.
//
// Once ctx is cancelled nothing more is claimed. Each execution finishes
// the action it is in the middle of, records it, and gives its run back with
// no failure counted. An action still in flight DrainTimeout later is cut
// off. A model call that then returns is not recorded and its run is given
// back all the same; a tool may be running still, so nothing is written for
// its run and the lease is left to lapse. Work waits for those last writes,
// and for no longer than a few seconds past DrainTimeout: an execution stuck
// in a model that ignores its context is left behind.
func (e *Engine) Work(ctx context.Context) error {
	lapsing := make(chan struct{})
	go func() {
		defer close(lapsing)
		e.lapse(ctx)
	}()

	slots := make(chan struct{}, e.opts.Concurrency)
	var executions sync.WaitGroup
	for ctx.Err() == nil {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		run, err := e.claim(ctx, "")
		if run == nil {
			<-slots
			if err != nil && ctx.Err() == nil {
				e.log.WarnContext(ctx, "agent: work: no run could be claimed", "error", err)
			}
			wait(ctx, e.opts.PollInterval)
			continue
		}
		executions.Go(func() {
			defer func() { <-slots }()
			// How the execution ended is in the journal and the log.
			_, _ = e.execute(ctx, *run)
		})
	}

	finished := make(chan struct{})
	go func() {
		executions.Wait()
		close(finished)
	}()
	limit := time.NewTimer(e.opts.DrainTimeout + lastWriteTimeout)
	defer limit.Stop()
	select {
	case <-finished:
	case <-limit.C:
		e.log.WarnContext(ctx, "agent: work: returning with executions that have not ended",
			"drain", e.opts.DrainTimeout)
	}
	<-lapsing
	return ctx.Err()
}

// lapse lapses overdue approvals every PollInterval until ctx ends. An
// approval that lapses makes its run runnable, and a worker then claims it.
func (e *Engine) lapse(ctx context.Context) {
	every := time.NewTicker(e.opts.PollInterval)
	defer every.Stop()
	for {
		if _, err := e.store.ExpireApprovals(ctx, e.clock.Now()); err != nil && ctx.Err() == nil {
			e.log.WarnContext(ctx, "agent: work: overdue approvals could not be lapsed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-every.C:
		}
	}
}

// wait blocks for d, or until ctx ends.
func wait(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
