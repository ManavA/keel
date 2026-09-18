package meili

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/meilisearch/meilisearch-go"
)

// SettingDrift is one setting whose live value differs from Config.
type SettingDrift struct {
	// Setting is the Meilisearch setting name, as its API spells it.
	Setting string
	// Missing is declared in Config but absent from the live index. This
	// is the direction that breaks queries.
	Missing []string
	// Extra is present on the live index but not declared. It does not
	// break a query today, but the next SetupIndex call REPLACES the
	// whole list, so anything depending on it breaks at the next deploy
	// instead.
	Extra []string
	// Want and Got carry the whole value for settings where order or
	// magnitude is the point (ranking rules, pagination, faceting) and
	// Missing/Extra cannot express the difference.
	Want string
	Got  string
	// Impact is what this specific drift does to a live search.
	Impact string
}

func (d SettingDrift) String() string {
	var b strings.Builder
	b.WriteString(d.Setting)
	b.WriteString(": ")
	switch {
	case len(d.Missing) > 0 && len(d.Extra) > 0:
		fmt.Fprintf(&b, "missing %v, unexpected %v", d.Missing, d.Extra)
	case len(d.Missing) > 0:
		fmt.Fprintf(&b, "missing %v", d.Missing)
	case len(d.Extra) > 0:
		fmt.Fprintf(&b, "unexpected %v", d.Extra)
	default:
		fmt.Fprintf(&b, "want %s, got %s", d.Want, d.Got)
	}
	if d.Impact != "" {
		b.WriteString(" — ")
		b.WriteString(d.Impact)
	}
	return b.String()
}

// SettingsReport is the outcome of a settings check.
//
// Measured is deliberately false on the zero value: a report that was never
// filled in, because the read failed or a caller ignored the error, must
// not read as "checked, and fine". OK requires Measured true for exactly
// this reason.
type SettingsReport struct {
	Measured bool
	Drifts   []SettingDrift
}

// OK reports whether the live index matches Config. It is false on an
// unmeasured report, so "could not check" never passes for "clean".
func (r SettingsReport) OK() bool {
	return r.Measured && len(r.Drifts) == 0
}

// Describe renders the report for a human, distinguishing all three states
// a two-value boolean cannot: unmeasured, drifted, and clean.
func (r SettingsReport) Describe() string {
	if !r.Measured {
		return "search index settings NOT CHECKED — the live settings could not be read, which is not a pass"
	}
	if len(r.Drifts) == 0 {
		return "search index settings match the declared Config"
	}
	lines := make([]string, 0, len(r.Drifts)+1)
	lines = append(lines, fmt.Sprintf(
		"search index settings drifted from Config in %d setting(s); SetupIndex has not run "+
			"against this index since Config changed", len(r.Drifts)))
	for _, d := range r.Drifts {
		lines = append(lines, "  - "+d.String())
	}
	return strings.Join(lines, "\n")
}

// CheckSettings reads the LIVE index settings and compares them to Config.
// See [Searcher.Health] for what ctx can and cannot do here.
func (s *Searcher) CheckSettings(ctx context.Context) (SettingsReport, error) {
	if err := ctx.Err(); err != nil {
		return SettingsReport{}, err
	}
	live, err := s.index.GetSettings()
	if err != nil {
		return SettingsReport{}, fmt.Errorf("read live index settings: %w", err)
	}
	if live == nil {
		// Defensive: a nil settings body with a nil error would otherwise
		// compare as "every setting empty" and report every setting as
		// drifted, which is a misdiagnosis rather than an accurate report.
		return SettingsReport{}, fmt.Errorf("read live index settings: Meilisearch returned no settings body")
	}
	return SettingsReport{Measured: true, Drifts: compareIndexSettings(s.cfg, *live)}, nil
}

// settingsSettleTimeout bounds how long SetupIndexAndVerify waits for
// Meilisearch to apply the settings SetupIndex enqueued.
const settingsSettleTimeout = 60 * time.Second

// SetupIndexAndVerify applies Config and then PROVES the settings landed.
//
// SetupIndex alone cannot do that: Meilisearch applies every settings
// update asynchronously, so a `Update*Attributes` call returns a task id
// and SetupIndex discards it, returning nil while the work may still be
// enqueued — or may have already failed. SetupIndexAndVerify waits for
// those specific tasks and then reads the settings back with
// [CheckSettings].
//
// This deliberately is NOT what SetupIndex itself does. SetupIndex runs on
// a server's startup path, where blocking on a task queue that may be busy
// with an unrelated bulk write would trade a silent settings bug for a
// failed deploy. A batch caller — a reindex job, above all — should prefer
// this.
func (s *Searcher) SetupIndexAndVerify(ctx context.Context) (SettingsReport, error) {
	// Bound "tasks this call is responsible for" by the queue head BEFORE
	// the writes, so an unrelated failure from an earlier run is not
	// blamed on this call.
	since, err := s.latestSettingsTaskUID()
	if err != nil {
		return SettingsReport{}, fmt.Errorf("read task queue before setup: %w", err)
	}
	if err := s.SetupIndex(ctx); err != nil {
		return SettingsReport{}, err
	}
	if err := s.awaitSettingsTasks(ctx, since); err != nil {
		// Explicitly NOT a drift report: "the settings did not settle" is a
		// different claim from "the settings settled and are wrong", and
		// collapsing them would report a run that failed to measure as a
		// reported pass.
		return SettingsReport{}, err
	}
	report, err := s.CheckSettings(ctx)
	if err != nil {
		return report, err
	}
	if !report.OK() {
		return report, fmt.Errorf("index settings did not apply: %s", report.Describe())
	}
	return report, nil
}

// benignSettingsTaskFailure reports whether a failed task means nothing is
// wrong: SetupIndex always tries to create the index, and on every call
// after the first that fails with index_already_exists — the desired end
// state, not a problem. Narrow on purpose: only this exact code, for only
// index creation. Every settings failure, and any OTHER creation failure,
// still fails the run.
func benignSettingsTaskFailure(task meilisearch.Task) bool {
	return task.Type == meilisearch.TaskTypeIndexCreation && task.Error.Code == "index_already_exists"
}

func settingsTaskQuery(limit int64) *meilisearch.TasksQuery {
	return &meilisearch.TasksQuery{
		Limit: limit,
		// The TYPE filter is load-bearing: a caller may be writing
		// documents to the same index concurrently, and without this a
		// failed document write would be misreported as "the settings did
		// not apply" — a wrong diagnosis pointing at the wrong system.
		Types: []meilisearch.TaskType{meilisearch.TaskTypeSettingsUpdate, meilisearch.TaskTypeIndexCreation},
	}
}

func (s *Searcher) latestSettingsTaskUID() (int64, error) {
	res, err := s.index.GetTasks(settingsTaskQuery(1))
	if err != nil {
		return 0, err
	}
	if len(res.Results) == 0 {
		return -1, nil
	}
	return res.Results[0].UID, nil
}

func (s *Searcher) awaitSettingsTasks(ctx context.Context, since int64) error {
	deadline := time.Now().Add(settingsSettleTimeout)
	for {
		res, err := s.index.GetTasks(settingsTaskQuery(100))
		if err != nil {
			return fmt.Errorf("read task queue: %w", err)
		}

		pending := 0
		var failed []string
		for _, task := range res.Results {
			if task.UID <= since {
				continue
			}
			switch task.Status {
			case meilisearch.TaskStatusEnqueued, meilisearch.TaskStatusProcessing:
				pending++
			case meilisearch.TaskStatusSucceeded:
			case meilisearch.TaskStatusFailed, meilisearch.TaskStatusCanceled:
				if benignSettingsTaskFailure(task) {
					continue
				}
				fallthrough
			default:
				failed = append(failed, fmt.Sprintf("task %d (%s) ended %s: [%s] %s",
					task.UID, task.Type, task.Status, task.Error.Code, task.Error.Message))
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("index settings task(s) did not succeed: %s", strings.Join(failed, "; "))
		}
		if pending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("index settings did not settle within %s (%d task(s) still pending) — "+
				"the applied settings are UNKNOWN, which is not a pass", settingsSettleTimeout, pending)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(meiliTaskPoll):
		}
	}
}

// compareIndexSettings is the whole comparison, split out from the HTTP
// read so it can be tested without a server.
func compareIndexSettings(cfg Config, got meilisearch.Settings) []SettingDrift {
	var drifts []SettingDrift

	if d, ok := compareSequence("searchableAttributes", cfg.Searchable, got.SearchableAttributes,
		"free-text search stops matching the missing field and answers 200 with zero or wrong hits"); ok {
		drifts = append(drifts, d)
	}
	if d, ok := compareSet("filterableAttributes", cfg.Filterable, got.FilterableAttributes,
		"a search filtering on the missing attribute is REJECTED outright"); ok {
		drifts = append(drifts, d)
	}
	if d, ok := compareSet("sortableAttributes", cfg.Sortable, got.SortableAttributes,
		"a sort on the missing attribute is SILENTLY DROPPED by a caller's own retry-without-sort logic, "+
			"so the response is 200 in relevance order with nothing to say the sort was ignored"); ok {
		drifts = append(drifts, d)
	}
	if d, ok := compareSequence("rankingRules", cfg.RankingRules, got.RankingRules,
		"result ORDER changes with no error on any surface"); ok {
		drifts = append(drifts, d)
	}
	if d, ok := compareSynonyms(cfg.Synonyms, got.Synonyms); ok {
		drifts = append(drifts, d)
	}

	// Scalars. Only a live value BELOW the declaration truncates results,
	// but any difference means SetupIndex has not run, so report either
	// way and let Impact carry the distinction.
	if cfg.MaxTotalHits > 0 && (got.Pagination == nil || got.Pagination.MaxTotalHits != cfg.MaxTotalHits) {
		drifts = append(drifts, SettingDrift{
			Setting: "pagination.maxTotalHits",
			Want:    fmt.Sprint(cfg.MaxTotalHits),
			Got:     scalarOrAbsent(got.Pagination == nil, func() int64 { return got.Pagination.MaxTotalHits }),
			Impact: "below the declared value, search stops finding anything past that hit and " +
				"reports a CAPPED total as if it were the real one",
		})
	}
	if cfg.MaxValuesPerFacet > 0 && (got.Faceting == nil || got.Faceting.MaxValuesPerFacet != cfg.MaxValuesPerFacet) {
		drifts = append(drifts, SettingDrift{
			Setting: "faceting.maxValuesPerFacet",
			Want:    fmt.Sprint(cfg.MaxValuesPerFacet),
			Got:     scalarOrAbsent(got.Faceting == nil, func() int64 { return got.Faceting.MaxValuesPerFacet }),
			Impact:  "facet values are truncated ALPHABETICALLY past the declared value",
		})
	}

	return drifts
}

func scalarOrAbsent(absent bool, read func() int64) string {
	if absent {
		return "absent"
	}
	return fmt.Sprint(read())
}

// compareSet compares membership only. Meilisearch de-duplicates and
// reorders these on ingest, so a sequence comparison here would report
// drift on an index that is in fact correct.
func compareSet(name string, want, got []string, impact string) (SettingDrift, bool) {
	missing := notIn(want, got)
	extra := notIn(got, want)
	if len(missing) == 0 && len(extra) == 0 {
		return SettingDrift{}, false
	}
	return SettingDrift{
		Setting: name, Missing: missing, Extra: extra,
		Want: strings.Join(dedupeSorted(want), ", "), Got: strings.Join(dedupeSorted(got), ", "),
		Impact: impact,
	}, true
}

// compareSequence compares order as well as membership, for settings where
// the order IS the setting. It still reports Missing/Extra when membership
// differs, since that is the more actionable message; a pure reorder shows
// up as Want/Got.
func compareSequence(name string, want, got []string, impact string) (SettingDrift, bool) {
	if equalSequence(want, got) {
		return SettingDrift{}, false
	}
	return SettingDrift{
		Setting: name, Missing: notIn(want, got), Extra: notIn(got, want),
		Want: strings.Join(want, ", "), Got: strings.Join(got, ", "), Impact: impact,
	}, true
}

func compareSynonyms(want, got map[string][]string) (SettingDrift, bool) {
	var missing, extra []string
	for k, wv := range want {
		gv, ok := got[k]
		if !ok || !equalSet(wv, gv) {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return SettingDrift{}, false
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return SettingDrift{
		Setting: "synonyms", Missing: missing, Extra: extra,
		Impact: "the configured shorthand stops matching, and the query answers 200 with the wrong documents",
	}, true
}

func notIn(a, b []string) []string {
	have := make(map[string]struct{}, len(b))
	for _, v := range b {
		have[v] = struct{}{}
	}
	seen := make(map[string]struct{}, len(a))
	var out []string
	for _, v := range a {
		if _, ok := have[v]; ok {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func equalSet(a, b []string) bool {
	return len(notIn(a, b)) == 0 && len(notIn(b, a)) == 0
}

func equalSequence(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func dedupeSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	seen := make(map[string]struct{}, len(out))
	kept := out[:0]
	for _, v := range out {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		kept = append(kept, v)
	}
	return kept
}
