// Package meili implements [search.Index] for Meilisearch: facets, typo
// tolerance, synonyms, and settings-drift detection against a live index.
//
// It is a separate package from search, rather than a type inside it, so
// that a caller who only needs [search.PostgresIndex] — the in-process
// default — does not pull in the meilisearch-go client and its dependency
// tree merely by importing search. Import this package only when a service
// actually needs Meilisearch.
//
// # Settings drift
//
// [Searcher.SetupIndex] configures Meilisearch's searchable, filterable and
// sortable attributes, ranking rules, and result caps. It runs at process
// startup, so a change to [Config] has no effect on a running index until
// the process calling SetupIndex is redeployed and restarted. Meilisearch
// also applies settings updates asynchronously: SetupIndex can return nil
// while a setting is still enqueued or has already failed.
// [Searcher.SetupIndexAndVerify] waits for those tasks to settle and reads
// the live settings back with [Searcher.CheckSettings]; it blocks, so it is
// meant for a batch job such as a reindex, not a server's startup path.
//
// The effect of each kind of drift differs:
//
//   - a missing filterable attribute: the query naming it is rejected
//   - a missing sortable attribute: the search succeeds, but in the wrong
//     order, if the caller retries without an unsupported sort
//   - a missing searchable attribute: free-text search over that field
//     returns zero hits
//   - reverted pagination or faceting settings: results or facet values
//     are truncated
//
// [Searcher.DriftCheck] runs this comparison on a schedule and fails loudly
// on drift; [Searcher.RequireSettings] refuses startup on drift. Both report
// "not checked" rather than clean when the live settings could not be read.
//
// # Runbook: re-applying settings after drift
//
// Drift means the live index no longer matches [Config]: someone edited the
// settings out of band, or a deploy carrying a Config change never applied
// them. The repair is to apply Config and prove it landed:
//
//  1. From a batch caller such as a reindex job, run
//     [Searcher.SetupIndexAndVerify]. It applies Config, waits for the
//     settings tasks to settle, and reads the settings back; it returns an
//     error while they differ.
//  2. If the process that calls [Searcher.SetupIndex] at startup is already
//     deployed with the current Config, restarting it re-applies Config.
//     SetupIndex alone does not wait, so confirm with DriftCheck or
//     [Searcher.CheckSettings] afterwards rather than assuming the restart
//     fixed it.
//  3. If neither applies the settings — SetupIndexAndVerify keeps failing —
//     the writes themselves are being rejected (permissions, an unreachable
//     index), not merely slow. The error names the failing task; fix that
//     before re-running.
//
// Do not "fix" drift by editing the live index by hand to match. The next
// SetupIndex call replaces the whole settings list, so a hand edit that
// Config does not declare is drift again the moment anything re-applies.
//
// # Filter value quoting
//
// A filter value containing a space or a hyphen must be quoted before it is
// interpolated into a Meilisearch filter expression, or the parser can read
// it as more than one token. [search.Filter] constructors, translated by
// this package's internal buildMeiliFilters, quote every string value; call
// [QuoteFilterValue] directly only when building a [search.Raw] expression
// by hand.
//
// # Result caps
//
// Meilisearch caps total hits at 1,000 by default and reports the capped
// total as the real one past that point. [Config.MaxTotalHits] raises this
// cap. Facet values are truncated alphabetically past
// [Config.MaxValuesPerFacet]'s default of 100.

// # Cutover from PostgresIndex
//
// [search.PostgresIndex] is the right default while the table is small and
// none of its three hard limits bite: it has no facet counts
// ([search.Query.Facets] is ignored), no typo tolerance, and free-text
// search is whole-lexeme matching, unstemmed under the default "simple"
// configuration. Move to this package when facet counts, typo tolerance,
// or synonyms become requirements — not before, because Meilisearch costs
// a second service and a second source of settings truth (see Settings
// drift above): a Config change applies only after the process calling
// [Searcher.SetupIndex] is redeployed and restarted, and asynchronously at
// that.
//
// The cutover keeps PostgresIndex as the source of truth until the new
// index is proven, so every step is reversible by pointing reads back at
// it. Both implementations satisfy [search.Index]; hold an Index on the
// serving path rather than a concrete type, and the flip — and the
// rollback — is a one-line change at the constructor.
//
//  1. Audit the behavior deltas. The same [search.Query] answers
//     differently on each backend, and the flip changes results even when
//     nothing is broken. Empty [search.In] matches nothing on Postgres and
//     everything here. [search.Query.Facets] and
//     [search.Query.AttributesToRetrieve] are ignored on Postgres and
//     honored here, so callers that assumed whole documents must be ready
//     for restricted fields. Default order changes from insertion order to
//     relevance, and [search.Result.Total] changes from an exact count to
//     an estimate capped by [Config.MaxTotalHits]. Keep setting
//     [search.SortField.Numeric]: Postgres needs it for numeric sorts and
//     this package ignores it, so it is safe on both sides. Do not use
//     [search.OpRaw] until reads flip: PostgresIndex rejects it, so a Raw
//     filter breaks the Postgres read path during dual-write. And only
//     hand-built Raw expressions need [QuoteFilterValue]; filters built
//     with the [search] constructors are quoted already.
//  2. Declare every filtered and sorted attribute in [Config] before
//     backfilling. An undeclared filterable attribute rejects the query
//     outright, an undeclared sortable silently misorders it, and an
//     undeclared searchable returns zero hits on that field — these fail
//     at read time, after the backfill looked fine.
//  3. Apply the settings from the backfill caller with
//     [Searcher.SetupIndexAndVerify], not [Searcher.SetupIndex]. SetupIndex
//     applies in a fixed order — create-if-missing, searchable,
//     filterable, sortable, ranking rules, synonyms (skipped when
//     [Config.Synonyms] is nil), pagination (skipped when
//     [Config.MaxTotalHits] is zero), faceting (skipped when
//     [Config.MaxValuesPerFacet] is zero) — and returns before any of it
//     has applied. SetupIndexAndVerify waits for the settings tasks to
//     settle and reads the settings back, so a clean report means the
//     backfill that follows runs against the declared settings. A server's
//     startup path should keep calling SetupIndex and gate serving on
//     [Searcher.RequireSettings] only once the settings have had a chance
//     to settle: gating immediately after SetupIndex in the same process
//     reports drift that is still enqueued.
//  4. Dual-write with Postgres primary. Every Index, Update, and Remove
//     writes PostgresIndex first and this index second; a Meilisearch
//     failure is logged, not served, because Postgres is still the source
//     of truth. Reads stay on Postgres.
//  5. Backfill from the source of truth. Page PostgresIndex with an
//     explicit Limit (it defaults to 20) and IndexDocuments each page —
//     every write waits for its task, so a finished backfill is applied,
//     not enqueued. Collect every id seen into a keep set, then compare
//     [Searcher.DocumentCount] against the Postgres count and run
//     [Searcher.PruneStale] with the keep set to delete anything that
//     stopped qualifying mid-backfill.
//  6. Shadow-read before flipping. Mirror a sample of production queries
//     to both backends and compare totals and top-hit ids, allowing for
//     the known deltas from step 1 (order, estimated totals, newly live
//     facets). Investigate anything else before it serves traffic.
//  7. Flip reads to the Searcher, keep dual-writing, and enable the drift
//     check (next section). The rollback point is this flip: one line at
//     the constructor, back to PostgresIndex, which never stopped being
//     written. Do not drop the Postgres table at cutover; decommission it
//     only after an agreed window with the drift check clean and the
//     counts matching.
//
// # Cutover: drift-check wiring and rollback criteria
//
// Wire the drift check into the scheduler exactly as [Searcher.DriftCheck]
// documents. A clean index reports Attempted 1, Succeeded 1; a drifted
// index fails the run with the setting and its consequence; an unreadable
// index is Fatal, which is not clean. Roll back to PostgresIndex — the
// one-line flip from step 7 — on any of: the drift check failing after
// re-applying with SetupIndexAndVerify, a DocumentCount mismatch past the
// caller's tolerance, search error rate or latency worse than the Postgres
// baseline over the window, or a Meilisearch outage of any kind. Postgres
// serves immediately in all four cases because it was dual-written since
// step 4.
package meili
