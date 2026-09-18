package jobs

import (
	"context"
	"fmt"
	"strconv"
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
// are combined with [namespacedKey], which is collision-free for any pair
// of strings, including ones containing whatever byte a simpler
// concatenation might have picked as a separator; job may not be empty.
//
// A failed Done check is returned as an error rather than treated as "not
// done". Treating it as "not done" would re-run fn whenever the guard's
// store is briefly unreachable, which is unsafe for a fn whose side effect
// (an email send, a charge) is not itself idempotent.
func Idempotent(ctx context.Context, g Guard, job, key string, fn func(ctx context.Context) error) error {
	if job == "" {
		return fmt.Errorf("jobs: Idempotent requires a non-empty job name")
	}
	namespaced := namespacedKey(job, key)

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

// namespacedKey combines job and key into a single string with no
// collisions: two different (job, key) pairs never produce the same
// result, regardless of what bytes job or key contain. It length-prefixes
// job rather than joining with a separator character, because any fixed
// separator can itself appear inside job or key — a plain job+"\x00"+key
// join collides for job="a\x00b", key="c" against job="a", key="b\x00c",
// both of which produce "a\x00b\x00c".
func namespacedKey(job, key string) string {
	return strconv.Itoa(len(job)) + ":" + job + key
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
