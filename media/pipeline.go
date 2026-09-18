package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// ErrPermanent marks a source a Fetcher's origin will never serve again: a
// 404 from a media host that has withdrawn the asset, as opposed to a
// timeout or a 5xx. Run treats it as Dropped rather than Failed, so a
// withdrawn source is not requeued on later runs.
var ErrPermanent = errors.New("media: source permanently unavailable")

// Fetcher downloads a source. Implementations should retry transient
// failures internally and wrap ErrPermanent around an answer that will never
// change.
type Fetcher interface {
	Fetch(ctx context.Context, sourceURL string) ([]byte, error)
}

// Deriver turns fetched source bytes into the variants to store.
type Deriver func(ctx context.Context, body []byte) ([]DerivedVariant, error)

// DerivedVariant is one variant a Deriver produced, ready to store.
type DerivedVariant struct {
	// Name identifies the variant for Recorder bookkeeping (e.g. "large").
	Name string
	// Key is the full Store key to write Body under.
	Key         string
	Body        []byte
	ContentType string
}

// Putter is the write side of Store — all Run needs to land a variant. Any
// Store satisfies it for free; declaring it separately keeps a caller who
// only wants to run the pipeline against a stub from having to fake Get,
// Delete and URL as well.
type Putter interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
}

// Recorder is the durable record of one Item's progress: which variants
// have already been stored, so a re-run does not redo finished work, and
// which sources are permanently gone, so they are not retried. A caller
// implements Recorder against whatever storage it already has: a Postgres
// table, a KV store, a map in a test.
type Recorder interface {
	// Done reports the variant names already stored for key, from a
	// previous run.
	Done(ctx context.Context, key string) (map[string]bool, error)
	// RecordVariant marks one variant stored for key.
	RecordVariant(ctx context.Context, key, variantName string) error
	// RecordDropped marks key's source as permanently unavailable.
	RecordDropped(ctx context.Context, key string) error
}

// Item is one unit of pipeline work.
type Item struct {
	// Key identifies this item to the Recorder and typically prefixes the
	// Store keys its variants are written under.
	Key       string
	SourceURL string
	// ExpectedVariants are the variant names this item should end up with.
	// An item where Recorder.Done already reports every one of these is
	// skipped without a fetch — this is what makes a re-run idempotent
	// instead of merely retry-safe.
	ExpectedVariants []string
}

// Result is one Item's outcome. Exactly one of Skipped, Dropped, or Err is
// meaningful; Stored is set alongside any of them and lists whatever did
// land this run even if the item did not fully complete.
type Result struct {
	Item    Item
	Stored  []string
	Skipped bool
	Dropped bool
	Err     error
}

// Options configures Run and RunFallback. The zero value is a working
// default: concurrency 8, logging through slog.Default().
type Options struct {
	Concurrency int
	// Logger receives warnings for problems that do not fail the item outright
	// (for example, a Recorder write failing after the Store write it was
	// recording already succeeded). Falls back to slog.Default() when nil;
	// Options never calls slog.SetDefault — that is the caller binary's
	// decision, not this package's.
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Concurrency <= 0 {
		o.Concurrency = 8
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Run processes items concurrently, bounded by opts.Concurrency. It never
// returns an error itself — one item's failure is reported in its own
// Result, not as a call failure, so one bad source cannot abort a whole
// batch.
func Run(ctx context.Context, items []Item, fetch Fetcher, derive Deriver, st Putter, rec Recorder, opts Options) []Result {
	opts = opts.withDefaults()

	results := make([]Result, len(items))
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup

	for i, item := range items {
		wg.Add(1)
		go func(i int, item Item) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = Result{Item: item, Err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			results[i] = process(ctx, item, fetch, derive, st, rec, opts)
		}(i, item)
	}
	wg.Wait()
	return results
}

func process(ctx context.Context, item Item, fetch Fetcher, derive Deriver, st Putter, rec Recorder, opts Options) Result {
	done, err := rec.Done(ctx, item.Key)
	if err != nil {
		return Result{Item: item, Err: fmt.Errorf("check done: %w", err)}
	}
	if allDone(item.ExpectedVariants, done) {
		return Result{Item: item, Skipped: true}
	}

	body, err := fetch.Fetch(ctx, item.SourceURL)
	switch {
	case errors.Is(err, ErrPermanent):
		if rerr := rec.RecordDropped(ctx, item.Key); rerr != nil {
			opts.Logger.Warn("media: record dropped failed", "key", item.Key, "error", rerr)
		}
		return Result{Item: item, Dropped: true}
	case err != nil:
		return Result{Item: item, Err: fmt.Errorf("fetch: %w", err)}
	}

	variants, err := derive(ctx, body)
	if err != nil {
		return Result{Item: item, Err: fmt.Errorf("derive: %w", err)}
	}

	var stored []string
	for _, v := range variants {
		if done[v.Name] {
			stored = append(stored, v.Name)
			continue
		}
		if err := st.Put(ctx, v.Key, v.Body, v.ContentType); err != nil {
			return Result{Item: item, Stored: stored, Err: fmt.Errorf("store %s: %w", v.Key, err)}
		}
		// The variant is durably stored even if this write fails; a failed
		// record here means the NEXT run repeats a Put that is a harmless
		// overwrite, not a data loss — so it is a warning, not an item error.
		if err := rec.RecordVariant(ctx, item.Key, v.Name); err != nil {
			opts.Logger.Warn("media: record variant failed", "key", item.Key, "variant", v.Name, "error", err)
		}
		stored = append(stored, v.Name)
	}

	return Result{Item: item, Stored: stored}
}

func allDone(expected []string, done map[string]bool) bool {
	if len(expected) == 0 {
		return false
	}
	for _, name := range expected {
		if !done[name] {
			return false
		}
	}
	return true
}
