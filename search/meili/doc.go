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
package meili
