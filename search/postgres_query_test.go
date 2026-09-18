package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildPostgresWhere_TextOnly(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("victorian", nil, &args)
	require.NoError(t, err)
	assert.Equal(t, "search_text @@ plainto_tsquery('simple', $1)", clause)
	assert.Equal(t, []any{"victorian"}, args)
}

func TestBuildPostgresWhere_NoTextNoFilters(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", nil, &args)
	require.NoError(t, err)
	assert.Empty(t, clause)
	assert.Empty(t, args)
}

func TestBuildPostgresWhere_EqFilter(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", []Filter{Eq("city", "Redwood City")}, &args)
	require.NoError(t, err)
	assert.Equal(t, "document->>$1 = $2", clause)
	assert.Equal(t, []any{"city", "Redwood City"}, args)
}

func TestBuildPostgresWhere_RangeFiltersAreCastToNumeric(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", []Filter{Gte("price", 500000), Lte("price", 900000)}, &args)
	require.NoError(t, err)
	assert.Equal(t, "(document->>$1)::numeric >= $2::numeric AND (document->>$3)::numeric <= $4::numeric", clause)
	assert.Equal(t, []any{"price", 500000, "price", 900000}, args)
}

func TestBuildPostgresWhere_InFilter(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", []Filter{In("status", []string{"active", "pending"})}, &args)
	require.NoError(t, err)
	assert.Equal(t, "document->>$1 = ANY($2)", clause)
	require.Len(t, args, 2)
	assert.Equal(t, "status", args[0])
	assert.Equal(t, []string{"active", "pending"}, args[1])
}

func TestBuildPostgresWhere_EmptyInFilterMatchesNothing(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", []Filter{In("status", nil)}, &args)
	require.NoError(t, err)
	assert.Equal(t, "false", clause)
}

func TestBuildPostgresWhere_RawFilterIsAnError(t *testing.T) {
	var args []any
	_, err := buildPostgresWhere("", []Filter{Raw("_geoBoundingBox(1,2,3,4)")}, &args)
	require.Error(t, err, "PostgresIndex must refuse a filter it cannot apply, not silently drop it")
}

func TestBuildPostgresWhere_UnsupportedOpIsAnError(t *testing.T) {
	var args []any
	_, err := buildPostgresWhere("", []Filter{{Field: "x", Op: "bogus"}}, &args)
	require.Error(t, err)
}

func TestBuildPostgresWhere_TextAndFiltersShareOneParameterSequence(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("garden", []Filter{Eq("city", "Palo Alto")}, &args)
	require.NoError(t, err)
	assert.Equal(t, "search_text @@ plainto_tsquery('simple', $1) AND document->>$2 = $3", clause)
	assert.Equal(t, []any{"garden", "city", "Palo Alto"}, args)
}

func TestBuildPostgresOrderBy_Empty(t *testing.T) {
	var args []any
	assert.Empty(t, buildPostgresOrderBy(nil, &args))
	assert.Empty(t, args)
}

func TestBuildPostgresOrderBy_TextSort(t *testing.T) {
	var args []any
	clause := buildPostgresOrderBy([]SortField{{Field: "city", Dir: Asc}}, &args)
	assert.Equal(t, "document->>$1 ASC", clause)
	assert.Equal(t, []any{"city"}, args)
}

func TestBuildPostgresOrderBy_NumericSortIsCast(t *testing.T) {
	var args []any
	clause := buildPostgresOrderBy([]SortField{{Field: "price", Dir: Desc, Numeric: true}}, &args)
	assert.Equal(t, "(document->>$1)::numeric DESC", clause)
}

func TestBuildPostgresOrderBy_ContinuesParameterNumberingFromExistingArgs(t *testing.T) {
	args := []any{"already-here"}
	clause := buildPostgresOrderBy([]SortField{{Field: "price", Dir: Asc}}, &args)
	assert.Equal(t, "document->>$2 ASC", clause,
		"ORDER BY parameters must continue numbering after any WHERE arguments already appended")
}

func TestComputeSearchText_ConcatenatesOnlyDeclaredStringFields(t *testing.T) {
	raw := []byte(`{"title":"Charming bungalow","city":"Alameda","price":900000,"nested":{"a":1}}`)
	text, err := computeSearchText([]string{"title", "city", "price", "missing"}, raw)
	require.NoError(t, err)
	assert.Equal(t, "Charming bungalow Alameda", text,
		"a numeric field and a missing field must contribute nothing")
}

func TestComputeSearchText_NoFieldsDeclaredYieldsEmptyWithoutParsing(t *testing.T) {
	text, err := computeSearchText(nil, []byte(`not even valid json`))
	require.NoError(t, err)
	assert.Empty(t, text)
}

func TestComputeSearchText_InvalidJSONIsAnError(t *testing.T) {
	_, err := computeSearchText([]string{"title"}, []byte(`not json`))
	require.Error(t, err)
}

func TestQuoteIdent(t *testing.T) {
	assert.Equal(t, `"listings"`, quoteIdent("listings"))
	assert.Equal(t, `"weird""name"`, quoteIdent(`weird"name`),
		"an embedded quote must be doubled, not stripped or left to close the identifier early")
}
