package search

import (
	"context"
	"encoding/json"
	"fmt"
)

// FilterOp is a comparison a [Filter] applies.
type FilterOp string

const (
	// OpEq matches a field equal to Value.
	OpEq FilterOp = "eq"
	// OpNeq matches a field not equal to Value.
	OpNeq FilterOp = "neq"
	// OpIn matches a field equal to one of Values. This and every range op
	// below assume a numeric or string field; mixing types across one
	// field is a caller error.
	OpIn FilterOp = "in"
	// OpGte matches a numeric field greater than or equal to Value.
	OpGte FilterOp = "gte"
	// OpLte matches a numeric field less than or equal to Value.
	OpLte FilterOp = "lte"
	// OpGt matches a numeric field greater than Value.
	OpGt FilterOp = "gt"
	// OpLt matches a numeric field less than Value.
	OpLt FilterOp = "lt"
	// OpRaw carries a raw, backend-specific expression in Value, for a
	// Meilisearch function such as `_geoBoundingBox(...)` that has no
	// portable equivalent. Only [Searcher] supports it; [PostgresIndex]
	// returns an error for it rather than ignoring it.
	OpRaw FilterOp = "raw"
)

// Filter is one clause a [Query] applies. Build one with [Eq], [In],
// [Gte], [Lte], [Gt], [Lt], [Neq] or [Raw] rather than the struct literal —
// the constructors pick the right field for each Op so a caller cannot set
// Value for an OpIn filter and have it silently ignored.
type Filter struct {
	Field string
	Op    FilterOp
	// Value holds the comparison value for every Op except OpIn.
	Value any
	// Values holds the candidate set for OpIn.
	Values []string
}

// Eq builds an equality filter.
func Eq(field string, value any) Filter { return Filter{Field: field, Op: OpEq, Value: value} }

// Neq builds an inequality filter.
func Neq(field string, value any) Filter { return Filter{Field: field, Op: OpNeq, Value: value} }

// In builds a set-membership filter. An empty values slice is a caller
// error at Search time for [PostgresIndex] and is dropped (matches
// everything) for [Searcher] — callers that mean "match nothing" should not
// construct a Filter at all.
func In(field string, values []string) Filter {
	return Filter{Field: field, Op: OpIn, Values: values}
}

// Gte builds a `field >= value` filter, over a numeric field.
func Gte(field string, value any) Filter { return Filter{Field: field, Op: OpGte, Value: value} }

// Lte builds a `field <= value` filter, over a numeric field.
func Lte(field string, value any) Filter { return Filter{Field: field, Op: OpLte, Value: value} }

// Gt builds a `field > value` filter, over a numeric field.
func Gt(field string, value any) Filter { return Filter{Field: field, Op: OpGt, Value: value} }

// Lt builds a `field < value` filter, over a numeric field.
func Lt(field string, value any) Filter { return Filter{Field: field, Op: OpLt, Value: value} }

// Raw builds a backend-specific filter. See [OpRaw].
func Raw(expr string) Filter { return Filter{Op: OpRaw, Value: expr} }

// SortDir is the direction of a [SortField].
type SortDir string

const (
	// Asc sorts ascending.
	Asc SortDir = "asc"
	// Desc sorts descending.
	Desc SortDir = "desc"
)

// SortField is one sort key in a [Query].
type SortField struct {
	Field string
	Dir   SortDir
	// Numeric tells [PostgresIndex] to sort by this field's numeric value
	// rather than its text value — Postgres has no declared column types
	// to infer this from the way Meilisearch's declared Config.Sortable
	// does, so it has to be told. Searcher (Meilisearch) ignores this
	// field.
	Numeric bool
}

// SortTable maps a public sort key (however a caller's own API spells its
// sort options — "price_asc", "newest") to the [SortField] it produces. A
// table rather than a switch statement so a settings-drift style check can
// enumerate every sort key it declares without a second, hand-maintained
// copy of the list.
type SortTable map[string]SortField

// BuildSort resolves key against table. An unknown or empty key returns nil
// (each backend's own default order) rather than an error — an unknown sort
// key is usually caller-facing input, and falling back to default order is
// a better answer than a hard failure for it.
func BuildSort(table SortTable, key string) []SortField {
	if key == "" {
		return nil
	}
	if f, ok := table[key]; ok {
		return []SortField{f}
	}
	return nil
}

// Query is one search request, backend-agnostic: the same Query works
// against [Searcher] (Meilisearch) and [PostgresIndex].
type Query struct {
	// Text is the free-text query. Empty matches everything, subject to
	// Filters.
	Text string
	// Filters is a set of filters, ANDed together.
	Filters []Filter
	// Sort orders the results. Empty means each backend's own default
	// order (Meilisearch: relevance; Postgres: insertion order).
	Sort []SortField
	// Facets lists which attributes to return a facet distribution for.
	// Meilisearch-only; [PostgresIndex] ignores this (a facet count over a
	// JSONB column at this tier is better done as a caller's own query).
	Facets []string
	// Offset and Limit page the result. Zero Limit is treated as each
	// backend's own default.
	Offset int64
	Limit  int64
	// AttributesToRetrieve restricts which document fields come back.
	// Meilisearch-only; [PostgresIndex] always returns the whole document.
	AttributesToRetrieve []string
}

// Result is one search response.
type Result struct {
	// Hits is the matched documents, decoded as plain maps. Use
	// [DecodeHits] to decode them into a caller's own type.
	Hits []map[string]any
	// Total is the total matching document count. For [Searcher] this is
	// Meilisearch's estimate, capped by Config.MaxTotalHits (see the
	// package doc); for [PostgresIndex] it is an exact count.
	Total int64
	// Facets is the facet distribution for every attribute named in
	// Query.Facets, or nil if none were requested or the backend does not
	// support facets.
	Facets map[string]any
}

// Index is what a caller depends on to store and search documents. Two
// implementations ship: search/meili's Searcher wraps Meilisearch and
// supports facets and typo tolerance; [PostgresIndex] needs no service
// beyond a Postgres database and is suitable for small tables. A caller
// that depends on Index rather than either concrete type can switch
// implementations without changing its own code. search/meili is a
// separate package specifically so that depending on search alone (for
// PostgresIndex, or just for Query/Filter/Index) does not pull in the
// Meilisearch client — see search/meili's package doc.
type Index interface {
	IndexDocuments(ctx context.Context, docs []Document) error
	UpdateDocuments(ctx context.Context, docs []Document) error
	RemoveDocuments(ctx context.Context, ids []string) error
	PruneStale(ctx context.Context, keep map[string]struct{}) (int, error)
	DocumentCount(ctx context.Context) (int64, error)
	Search(ctx context.Context, q Query) (*Result, error)
	Health(ctx context.Context) error
}

var _ Index = (*PostgresIndex)(nil)

// DecodeHits decodes hits (as returned in [Result.Hits]) into a slice of T,
// via a JSON round trip. Use this when a caller wants typed documents back
// instead of raw maps; the package itself stays generic by never assuming
// T.
func DecodeHits[T any](hits []map[string]any) ([]T, error) {
	out := make([]T, 0, len(hits))
	for i, h := range hits {
		b, err := json.Marshal(h)
		if err != nil {
			return nil, fmt.Errorf("marshal hit %d: %w", i, err)
		}
		var v T
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("decode hit %d: %w", i, err)
		}
		out = append(out, v)
	}
	return out, nil
}
