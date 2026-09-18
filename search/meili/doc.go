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
