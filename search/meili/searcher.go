package meili

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/meilisearch/meilisearch-go"

	"github.com/ManavA/keel/search"
)

var _ search.Index = (*Searcher)(nil)

// Options configures a [Searcher]. Host, APIKey and Config.UID are
// required; everything else has a working zero value.
type Options struct {
	// Host is the Meilisearch instance URL, e.g. "http://localhost:7700".
	Host string
	// APIKey authenticates against Host.
	APIKey string
	// Config declares the index's settings. See [SetupIndex].
	Config Config
	// Logger receives this Searcher's log lines. Nil falls back to
	// slog.Default(); this package never calls slog.SetDefault.
	Logger *slog.Logger
}

// Searcher indexes and searches documents in one Meilisearch index.
type Searcher struct {
	client clientAPI
	index  indexAPI
	cfg    Config
	logger *slog.Logger
}

// NewSearcher builds a Searcher connected to opts.Host.
func NewSearcher(opts Options) *Searcher {
	client := meilisearch.NewClient(meilisearch.ClientConfig{
		Host:   opts.Host,
		APIKey: opts.APIKey,
	})
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Searcher{
		client: client,
		index:  client.Index(opts.Config.UID),
		cfg:    opts.Config,
		logger: logger,
	}
}

// newSearcherWithFakes builds a Searcher over fakes, for tests that exercise
// the query builder and setup logic without a live Meilisearch instance.
func newSearcherWithFakes(client clientAPI, index indexAPI, cfg Config) *Searcher {
	return &Searcher{client: client, index: index, cfg: cfg, logger: slog.Default()}
}

// Health reports whether Meilisearch is reachable.
//
// meilisearch-go@v0.26.1's Health call takes no context, so ctx cannot
// abort a request already in flight; Health only honors ctx if it is
// already canceled or past its deadline before the call starts. The same
// limitation applies to every method on Searcher except the wait phase
// inside awaitTask, which does receive a real context (see awaitTask).
func (s *Searcher) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.client.Health()
	return err
}

// SetupIndex applies s.cfg to the index: creates it if it does not exist,
// and configures searchable, filterable and sortable attributes, ranking
// rules, synonyms, and the result caps.
//
// SetupIndex does NOT wait for these settings to actually apply — see the
// package doc for why, and use [SetupIndexAndVerify] where that guarantee
// is worth blocking for. See [Searcher.Health] for what ctx can and cannot
// do here.
func (s *Searcher) SetupIndex(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// DELIBERATE discard. On every call after the first this returns
	// index_already_exists, which is the expected state, not worth a log
	// line per boot. Any OTHER failure (unreachable, a key without write
	// permission) is re-raised by the very next call below, which IS
	// checked.
	_, _ = s.client.CreateIndex(&meilisearch.IndexConfig{
		Uid:        s.cfg.UID,
		PrimaryKey: s.cfg.PrimaryKey,
	})

	searchable := s.cfg.Searchable
	if _, err := s.index.UpdateSearchableAttributes(&searchable); err != nil {
		return fmt.Errorf("update searchable attributes: %w", err)
	}
	filterable := s.cfg.Filterable
	if _, err := s.index.UpdateFilterableAttributes(&filterable); err != nil {
		return fmt.Errorf("update filterable attributes: %w", err)
	}
	sortable := s.cfg.Sortable
	if _, err := s.index.UpdateSortableAttributes(&sortable); err != nil {
		return fmt.Errorf("update sortable attributes: %w", err)
	}
	ranking := s.cfg.RankingRules
	if _, err := s.index.UpdateRankingRules(&ranking); err != nil {
		return fmt.Errorf("update ranking rules: %w", err)
	}
	if s.cfg.Synonyms != nil {
		synonyms := s.cfg.Synonyms
		if _, err := s.index.UpdateSynonyms(&synonyms); err != nil {
			return fmt.Errorf("update synonyms: %w", err)
		}
	}
	if s.cfg.MaxTotalHits > 0 {
		if _, err := s.index.UpdatePagination(&meilisearch.Pagination{MaxTotalHits: s.cfg.MaxTotalHits}); err != nil {
			return fmt.Errorf("update pagination: %w", err)
		}
	}
	if s.cfg.MaxValuesPerFacet > 0 {
		if _, err := s.index.UpdateFaceting(&meilisearch.Faceting{MaxValuesPerFacet: s.cfg.MaxValuesPerFacet}); err != nil {
			return fmt.Errorf("update faceting: %w", err)
		}
	}
	return nil
}

// meiliTaskTimeout bounds how long awaitTask waits for a write to land.
// Generous because a large document batch is not instant, and the cost of
// waiting too long is a slow job, while the cost of not waiting at all is
// measuring an index that has not applied any of the work.
const meiliTaskTimeout = 2 * time.Minute

// meiliTaskPoll is how often task status is checked while waiting.
const meiliTaskPoll = 250 * time.Millisecond

// awaitTask blocks until a Meilisearch write has been applied, and reports
// a failed task as an error.
//
// Every Meilisearch write is enqueued and returns before it is applied, so
// a caller that reads document counts immediately after a write measures
// state from before the write took effect. This function closes that gap.
// It always passes explicit WaitParams to WaitForTask (verified against
// meilisearch-go@v0.26.1), for two reasons: WaitForTask with no options
// applies an internal 5-second timeout, too short for a large batch; and a
// nil Context or a zero Interval in WaitParams both panic inside the
// library.
func (s *Searcher) awaitTask(ctx context.Context, info *meilisearch.TaskInfo, what string) error {
	if info == nil {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, meiliTaskTimeout)
	defer cancel()

	task, err := s.index.WaitForTask(info.TaskUID, meilisearch.WaitParams{
		Context:  waitCtx,
		Interval: meiliTaskPoll,
	})
	if err != nil {
		return fmt.Errorf("await %s (task %d): %w", what, info.TaskUID, err)
	}
	if task.Status != meilisearch.TaskStatusSucceeded {
		return fmt.Errorf("%s failed: task %d ended %s: %s",
			what, info.TaskUID, task.Status, task.Error.Message)
	}
	return nil
}

// primaryKeyArg returns the variadic primaryKey argument to pass to
// AddDocuments/UpdateDocuments. The variadic slice's NIL-NESS is the
// signal the library reads (see transformStringVariadicToMap in
// meilisearch-go), not just its content — a present-but-empty string would
// still set primaryKey="" on the request, so an unset Config.PrimaryKey
// must produce a genuinely nil slice, not a slice holding "".
func (s *Searcher) primaryKeyArg() []string {
	if s.cfg.PrimaryKey == "" {
		return nil
	}
	return []string{s.cfg.PrimaryKey}
}

// IndexDocuments adds or replaces docs in the index, in one Meilisearch
// call, and waits for the write to apply.
func (s *Searcher) IndexDocuments(ctx context.Context, docs []search.Document) error {
	if len(docs) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	task, err := s.index.AddDocuments(docs, s.primaryKeyArg()...)
	if err != nil {
		return fmt.Errorf("index %d document(s): %w", len(docs), err)
	}
	return s.awaitTask(ctx, task, fmt.Sprintf("index %d document(s)", len(docs)))
}

// UpdateDocuments partially updates docs already in the index — only the
// fields each document carries are changed, everything else is left alone
// — and waits for the write to apply. Every document must carry the
// primary-key field so Meilisearch knows which existing document to merge
// into.
func (s *Searcher) UpdateDocuments(ctx context.Context, docs []search.Document) error {
	if len(docs) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	task, err := s.index.UpdateDocuments(docs, s.primaryKeyArg()...)
	if err != nil {
		return fmt.Errorf("update %d document(s): %w", len(docs), err)
	}
	return s.awaitTask(ctx, task, fmt.Sprintf("update %d document(s)", len(docs)))
}

// RemoveDocuments deletes documents by id and waits for the delete to
// apply. A withdrawn or expired document that fails to leave the index
// silently would stay searchable indefinitely; awaiting the task is what
// makes that failure visible instead.
func (s *Searcher) RemoveDocuments(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	const batchSize = 1000
	for start := 0; start < len(ids); start += batchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+batchSize, len(ids))
		task, err := s.index.DeleteDocuments(ids[start:end])
		if err != nil {
			return fmt.Errorf("delete documents: %w", err)
		}
		if err := s.awaitTask(ctx, task, "remove documents"); err != nil {
			return err
		}
	}
	return nil
}

// PruneStale removes every indexed document whose id is not in keep, and
// returns how many it deleted.
//
// A sync path that only ever ADDS documents leaves behind every document
// that later stops qualifying — deleted upstream, expired, opted out —
// and a job that only counts what it PUSHED can never see that. This is the
// other half: comparing the index's actual contents against the
// authoritative set and deleting the difference.
func (s *Searcher) PruneStale(ctx context.Context, keep map[string]struct{}) (int, error) {
	const pageSize = 1000

	var stale []string
	for offset := int64(0); ; offset += pageSize {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var page meilisearch.DocumentsResult
		err := s.index.GetDocuments(&meilisearch.DocumentsQuery{
			Offset: offset,
			Limit:  pageSize,
			Fields: []string{s.cfg.PrimaryKey},
		}, &page)
		if err != nil {
			return 0, fmt.Errorf("list indexed documents at offset %d: %w", offset, err)
		}
		if len(page.Results) == 0 {
			break
		}
		for _, doc := range page.Results {
			id, ok := doc[s.cfg.PrimaryKey].(string)
			if !ok || id == "" {
				continue
			}
			if _, wanted := keep[id]; !wanted {
				stale = append(stale, id)
			}
		}
		if int64(len(page.Results)) < pageSize {
			break
		}
	}

	if len(stale) == 0 {
		return 0, nil
	}
	if err := s.RemoveDocuments(ctx, stale); err != nil {
		return 0, err
	}
	return len(stale), nil
}

// DocumentCount reports how many documents the index currently holds.
// Compared against the count of documents that should be indexed, this
// detects the class of bug PruneStale exists to fix: a sync job that only
// ever adds documents accumulates stale ones indefinitely, and a job that
// logs only what it pushed cannot detect that on its own.
func (s *Searcher) DocumentCount(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	stats, err := s.index.GetStats()
	if err != nil {
		return 0, fmt.Errorf("index stats: %w", err)
	}
	return stats.NumberOfDocuments, nil
}
