// Package reconcile runs mail delivery reconciliation as a scheduled job:
// it polls recent sends, asks the provider for each message's status, and
// records confirmed, bounced and complained outcomes.
//
// Use it when a service sends mail and must learn about silent delivery
// failures from the provider rather than from user complaints. Register
// [Job.Run] directly as a jobs entry; it already matches [jobs.Func], so the
// run's counts surface through [jobs.Outcome] with no further wiring.
//
// An empty check run — no sends polled, or none the provider had a verdict
// for — reports [jobs.StatusDidNothing], never success. A broken send-log
// query and a quiet period look identical from here, so both take the same
// non-zero exit rather than reading as clean.
package reconcile

import (
	"context"
	"fmt"

	"github.com/ManavA/keel/jobs"
)

// Status is a provider's verdict on one sent message. Unknown means the
// provider has no verdict yet; the send stays unrecorded for a later run.
type Status string

const (
	// StatusConfirmed means the provider reports the message delivered.
	StatusConfirmed Status = "confirmed"
	// StatusBounced means the provider reports the message undeliverable.
	StatusBounced Status = "bounced"
	// StatusComplained means the provider reports a spam complaint.
	StatusComplained Status = "complained"
	// StatusUnknown means the provider has no verdict yet; the send stays
	// unrecorded for a later run.
	StatusUnknown Status = "unknown"
)

// Send is one recent send the job should check. ID is the store's own key,
// used when recording the outcome; MessageID is the provider's key, used
// when asking for the status.
type Send struct {
	ID        string
	MessageID string
	Recipient string
}

// Store polls recent sends and records their outcomes. It is declared here,
// where it is used, so any backend — Postgres, memory, the local filesystem
// — can back the job without importing this package's provider types.
type Store interface {
	RecentSends(ctx context.Context, limit int) ([]Send, error)
	RecordOutcome(ctx context.Context, id string, status Status) error
}

// Provider reports one message's delivery status. It answers only for the
// message asked about; bulk archive lookups belong to deliverycheck.
type Provider interface {
	Status(ctx context.Context, messageID string) (Status, error)
}

// Options configures a [Job]. Store and Provider are required; everything
// else has a default.
type Options struct {
	Store Store
	// Provider reports each message's delivery status.
	Provider Provider
	// Limit caps how many sends one run checks, default 100.
	Limit int
}

// defaultLimit caps one run's sends when Options.Limit is not positive.
const defaultLimit = 100

// Job reconciles one batch of recent sends per Run. It holds no state
// between runs beyond what Store persists, so a single Job is safe for
// concurrent use across scheduler entries.
type Job struct {
	store    Store
	provider Provider
	limit    int
}

// New builds a Job from opts, refusing one whose Store or Provider is
// missing: a job that cannot poll or cannot ask would measure nothing and
// report did-nothing every run, which reads as a broken pipeline rather
// than as a misconfiguration.
func New(opts Options) (*Job, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("reconcile: Options.Store is required")
	}
	if opts.Provider == nil {
		return nil, fmt.Errorf("reconcile: Options.Provider is required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	return &Job{store: opts.Store, provider: opts.Provider, limit: limit}, nil
}

// Run checks one batch of recent sends and reports what it recorded. A
// bounce or a complaint counts as recorded work, not as a failure: the job
// did what it came to do when the outcome is on record. Only a send the job
// could not check — a provider error, a record write that failed, or a send
// logged with no message id at all — counts as failed, and only a send list
// the job could not read fails the run.
func (j *Job) Run(ctx context.Context) (jobs.Outcome, error) {
	sends, err := j.store.RecentSends(ctx, j.limit)
	if err != nil {
		// Fatal, not a failure count: without the authoritative send set,
		// "measured, and fine" is a claim this run cannot make.
		return jobs.Outcome{Fatal: true}, fmt.Errorf("reconcile: list recent sends: %w", err)
	}

	outcome := jobs.Outcome{Attempted: len(sends)}
	if len(sends) == 0 {
		// The check itself is the unit of work attempted: it ran and learned
		// nothing, which is did-nothing rather than success or idle.
		outcome.Attempted = 1
		return outcome, nil
	}

	for _, s := range sends {
		if s.MessageID == "" {
			outcome.Failed++
			continue
		}
		status, err := j.provider.Status(ctx, s.MessageID)
		if err != nil {
			outcome.Failed++
			continue
		}
		if status == StatusUnknown {
			continue
		}
		if err := j.store.RecordOutcome(ctx, s.ID, status); err != nil {
			outcome.Failed++
			continue
		}
		outcome.Succeeded++
	}
	return outcome, nil
}
