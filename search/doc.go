// Package search indexes and queries generic, schema-less documents. A
// document is a `map[string]any` ([MapDocument]) or any JSON-marshalable
// type implementing [Document]. This package declares no document type of
// its own, so it stays usable regardless of what a caller indexes.
//
// # Two implementations
//
// [PostgresIndex] stores documents in a single Postgres table and requires
// no service beyond a database a caller likely already runs. It supports
// equality, range and set-membership filters, sorting, and free text search
// over declared fields via a `tsvector` column. Free text search is
// whole-lexeme matching, not substring matching: with the default text
// search configuration ("simple"), "bungalow" matches a document containing
// "Bungalow" but not one containing only "bung", and it is unstemmed —
// "charm" does not match a document containing only "Charming".
// [PostgresConfig.TextSearchConfig] can be set to a language configuration
// such as "english" to add stemming and stop-word removal. It is suitable
// for small tables; it has no facet counts and no typo tolerance. Use it as
// the default: it needs no import beyond this package.
//
// The search/meili subpackage wraps Meilisearch and adds facets, typo
// tolerance and synonyms. Switch to it once result-set size, facets, or
// typo tolerance justify running Meilisearch — see its own package doc for
// what it adds and the gotchas specific to it (settings drift, filter value
// quoting, result caps). It is a separate package, not a type in this one,
// so that a caller using only PostgresIndex does not pull in the
// Meilisearch client.
//
// Both implement [Index]. A caller depending on Index rather than either
// concrete type can switch implementations without changing its own code.
// [Query], [Filter] and [SortField] are the same shape for both
// implementations; [OpRaw] is the one exception, supported only by
// search/meili's Searcher.
package search
