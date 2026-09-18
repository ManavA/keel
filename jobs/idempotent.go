package jobs

import (
	"context"
	"fmt"
)

// Guard remembers which idempotency keys have already completed
// successfully. It is two methods rather than a lock: a lock prevents two
// runs from overlapping, but not two runs in sequence, such as a retried
// scheduler tick or a redelivered webhook — a more common cause of
// duplicate work than concurrent overlap.
type Guard interface {
	// Done reports whether key has already completed. An error means the
	// check itself failed (the store was unreachable), which callers must
	// treat as "unknown", never as "not done" — see [Idempotent].
	Done(ctx context.Context, key string) (bool, error)
	// MarkDone records that key has completed. Called only after fn
	// succeeds.
	MarkDone(ctx context.Context, key string) error
}

// Idempotent runs fn unless g reports the (job, key) pair already done, and
// marks it done after fn succeeds.
//
// job namespaces key. Without it, two unrelated callers that both reach for
// the same natural key — a calendar date, a batch id — collide: the second
// caller's Done check reports true, and it never runs at all. job and key
// are joined with a separator that cannot appear in either half naturally,
// but a caller supplying job or key values from untrusted input should not
// rely on that; construct g directly with a manually chosen key in that
// case.
//
// A failed Done check is returned as an error rather than treated as "not
// done". Treating it as "not done" would re-run fn whenever the guard's
// store is briefly unreachable, which is unsafe for a fn whose side effect
// (an email send, a charge) is not itself idempotent.
func Idempotent(ctx context.Context, g Guard, job, key string, fn func(ctx context.Context) error) error {
	namespaced := job + "\x00" + key

	done, err := g.Done(ctx, namespaced)
	if err != nil {
		return fmt.Errorf("check idempotency key %q for job %q: %w", key, job, err)
	}
	if done {
		return nil
	}
	if err := fn(ctx); err != nil {
		return err
	}
	if err := g.MarkDone(ctx, namespaced); err != nil {
		// fn already ran and succeeded; only the bookkeeping failed. The
		// error message says so, so a retry is not misdiagnosed as fn
		// itself failing.
		return fmt.Errorf("mark idempotency key %q for job %q done (side effect already ran): %w", key, job, err)
	}
	return nil
}

// MemoryGuard is an in-memory [Guard] for tests and single-process jobs. It
// does not persist across restarts, so it is not a substitute for a Guard
// backed by real storage in anything that runs as more than one process.
type MemoryGuard struct {
	done map[string]bool
}

// NewMemoryGuard builds an empty MemoryGuard.
func NewMemoryGuard() *MemoryGuard {
	return &MemoryGuard{done: make(map[string]bool)}
}

// Done reports whether key was previously marked done. It never errors.
func (g *MemoryGuard) Done(_ context.Context, key string) (bool, error) {
	return g.done[key], nil
}

// MarkDone records key as done. It never errors.
func (g *MemoryGuard) MarkDone(_ context.Context, key string) error {
	g.done[key] = true
	return nil
}
