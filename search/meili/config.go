package meili

// Config declares one index's settings. SetupIndex and CheckSettings both
// read the same Config, so the declared settings and the settings a check
// compares against are always the same value. Either can differ from the
// index's live settings; that difference is what CheckSettings reports.
type Config struct {
	// UID is the index's name.
	UID string
	// PrimaryKey is the JSON field Meilisearch treats as each document's
	// id. Required the first time the index is created; ignored after.
	PrimaryKey string

	// Searchable lists the fields free-text search matches, in PRIORITY
	// order — Meilisearch's "attribute" ranking rule favors earlier
	// entries over later ones, so this list is order-sensitive.
	Searchable []string
	// Filterable lists every attribute a query is allowed to filter on.
	// Order-insensitive: Meilisearch stores this as a set.
	Filterable []string
	// Sortable lists every attribute a query is allowed to sort on.
	// Order-insensitive.
	Sortable []string
	// RankingRules is Meilisearch's ranking-rule order. ORDER-SENSITIVE —
	// the order IS the setting, unlike Filterable/Sortable.
	RankingRules []string
	// Synonyms maps a term to the terms it should also match. One-way:
	// listing "sf" -> ["san francisco"] makes a query for "sf" also match
	// "san francisco" documents, not the reverse.
	Synonyms map[string][]string

	// MaxTotalHits raises Meilisearch's 1,000-hit default so totals are
	// exact and deep pagination works. Zero leaves Meilisearch's own
	// default in place.
	MaxTotalHits int64
	// MaxValuesPerFacet raises Meilisearch's 100-value default, past which
	// facet values are truncated ALPHABETICALLY. Zero leaves Meilisearch's
	// own default in place.
	MaxValuesPerFacet int64
}
