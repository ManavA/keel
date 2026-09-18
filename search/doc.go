// Package search indexes and queries generic, schema-less documents. A
// document is a `map[string]any` ([MapDocument]) or any JSON-marshalable
// type implementing [Document]. This package declares no document type of
// its own, so it stays usable regardless of what a caller indexes.
//
// # Two implementations
//
// [PostgresIndex] stores documents in a single Postgres table and requires
// no service beyond a database a caller likely already runs. It supports
// equality, range and set-membership filters, sorting, and ILIKE-style free
// text search over declared fields via a tsvector column. It is suitable
// for small tables; it has no facet counts and no typo tolerance. Use it as
// the default.
//
// [Searcher] wraps Meilisearch and adds facets, typo tolerance and
// synonyms. Switch to it once result-set size, facets, or typo tolerance
// justify running Meilisearch.
//
// Both implement [Index]. A caller depending on Index rather than either
// concrete type can switch implementations without changing its own code.
// [Query], [Filter] and [SortField] are the same shape for both
// implementations; [OpRaw] is the one exception, supported only by Searcher.
//
// # Settings drift, in the Meilisearch backend
//
// [SetupIndex] configures Meilisearch's searchable, filterable and sortable
// attributes, ranking rules, and result caps. It runs at process startup, so
// a change to [Config] has no effect on a running index until the process
// calling SetupIndex is redeployed and restarted. Meilisearch also applies
// settings updates asynchronously: SetupIndex can return nil while a
// setting is still enqueued or has already failed. [SetupIndexAndVerify]
// waits for those tasks to settle and reads the live settings back with
// [CheckSettings]; it blocks, so it is meant for a batch job such as a
// reindex, not a server's startup path.
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
// # Filter value quoting, in the Meilisearch backend
//
// A filter value containing a space or a hyphen must be quoted before it is
// interpolated into a Meilisearch filter expression, or the parser can read
// it as more than one token. [Filter] constructors and [buildMeiliFilters]
// quote every string value; call [QuoteFilterValue] directly only when
// building a [Raw] expression by hand.
//
// # Result caps, in the Meilisearch backend
//
// Meilisearch caps total hits at 1,000 by default and reports the capped
// total as the real one past that point. [Config.MaxTotalHits] raises this
// cap. Facet values are truncated alphabetically past
// [Config.MaxValuesPerFacet]'s default of 100.
package search
