package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildPostgresWhere_TextOnly(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("victorian", nil, "simple", &args)
	require.NoError(t, err)
	assert.Equal(t, "search_text @@ plainto_tsquery($1::regconfig, $2)", clause)
	assert.Equal(t, []any{"simple", "victorian"}, args)
}

func TestBuildPostgresWhere_TextConfigIsParameterizedNotInterpolated(t *testing.T) {
	var args []any
	_, err := buildPostgresWhere("victorian", nil, "english", &args)
	require.NoError(t, err)
	assert.Equal(t, []any{"english", "victorian"}, args, "the configured language must travel as a bound parameter, not literal SQL text")
}

func TestBuildPostgresWhere_NoTextNoFilters(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", nil, "simple", &args)
	require.NoError(t, err)
	assert.Empty(t, clause)
	assert.Empty(t, args)
}

func TestBuildPostgresWhere_EqFilter(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", []Filter{Eq(testCity, "Redwood City")}, "simple", &args)
	require.NoError(t, err)
	assert.Equal(t, "document->>$1 = $2", clause)
	assert.Equal(t, []any{testCity, "Redwood City"}, args)
}

func TestBuildPostgresWhere_RangeFiltersAreCastToNumeric(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", []Filter{Gte(testField, 500000), Lte(testField, 900000)}, "simple", &args)
	require.NoError(t, err)
	assert.Equal(t, "(document->>$1)::numeric >= $2::numeric AND (document->>$3)::numeric <= $4::numeric", clause)
	assert.Equal(t, []any{testField, 500000, testField, 900000}, args)
}

func TestBuildPostgresWhere_InFilter(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", []Filter{In("status", []string{"active", "pending"})}, "simple", &args)
	require.NoError(t, err)
	assert.Equal(t, "document->>$1 = ANY($2)", clause)
	require.Len(t, args, 2)
	assert.Equal(t, "status", args[0])
	assert.Equal(t, []string{"active", "pending"}, args[1])
}

func TestBuildPostgresWhere_EmptyInFilterMatchesNothing(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("", []Filter{In("status", nil)}, "simple", &args)
	require.NoError(t, err)
	assert.Equal(t, "false", clause)
}

func TestBuildPostgresWhere_RawFilterIsAnError(t *testing.T) {
	var args []any
	_, err := buildPostgresWhere("", []Filter{Raw("_geoBoundingBox(1,2,3,4)")}, "simple", &args)
	require.Error(t, err, "PostgresIndex must refuse a filter it cannot apply, not silently drop it")
}

func TestBuildPostgresWhere_UnsupportedOpIsAnError(t *testing.T) {
	var args []any
	_, err := buildPostgresWhere("", []Filter{{Field: "x", Op: "bogus"}}, "simple", &args)
	require.Error(t, err)
}

func TestBuildPostgresWhere_TextAndFiltersShareOneParameterSequence(t *testing.T) {
	var args []any
	clause, err := buildPostgresWhere("garden", []Filter{Eq(testCity, "Palo Alto")}, "simple", &args)
	require.NoError(t, err)
	assert.Equal(t, "search_text @@ plainto_tsquery($1::regconfig, $2) AND document->>$3 = $4", clause)
	assert.Equal(t, []any{"simple", "garden", testCity, "Palo Alto"}, args)
}

func TestBuildPostgresOrderBy_Empty(t *testing.T) {
	var args []any
	assert.Empty(t, buildPostgresOrderBy(nil, &args))
	assert.Empty(t, args)
}

func TestBuildPostgresOrderBy_TextSort(t *testing.T) {
	var args []any
	clause := buildPostgresOrderBy([]SortField{{Field: testCity, Dir: Asc}}, &args)
	assert.Equal(t, "document->>$1 ASC", clause)
	assert.Equal(t, []any{testCity}, args)
}

func TestBuildPostgresOrderBy_NumericSortIsCast(t *testing.T) {
	var args []any
	clause := buildPostgresOrderBy([]SortField{{Field: testField, Dir: Desc, Numeric: true}}, &args)
	assert.Equal(t, "(document->>$1)::numeric DESC", clause)
}

func TestBuildPostgresOrderBy_ContinuesParameterNumberingFromExistingArgs(t *testing.T) {
	args := []any{"already-here"}
	clause := buildPostgresOrderBy([]SortField{{Field: testField, Dir: Asc}}, &args)
	assert.Equal(t, "document->>$2 ASC", clause,
		"ORDER BY parameters must continue numbering after any WHERE arguments already appended")
}

// TestBuildPostgresQueries_DefaultsToOrderByID is the regression test
// pinning the default order: without it, LIMIT/OFFSET paging over an
// unordered result set can return the same row twice or skip one, since
// Postgres gives no ordering guarantee absent an ORDER BY.
func TestBuildPostgresQueries_DefaultsToOrderByID(t *testing.T) {
	_, _, selectQuery, _, err := buildPostgresQueries(testTable, PostgresConfig{}, Query{})
	require.NoError(t, err)
	assert.Contains(t, selectQuery, "ORDER BY id")
}

func TestBuildPostgresQueries_ExplicitSortOverridesTheDefault(t *testing.T) {
	_, _, selectQuery, _, err := buildPostgresQueries(testTable, PostgresConfig{}, Query{
		Sort: []SortField{{Field: testField, Dir: Desc}},
	})
	require.NoError(t, err)
	assert.NotContains(t, selectQuery, "ORDER BY id")
	assert.Contains(t, selectQuery, "ORDER BY document->>")
}

func TestBuildPostgresQueries_NegativeOffsetIsClampedToZero(t *testing.T) {
	_, _, _, selectArgs, err := buildPostgresQueries(testTable, PostgresConfig{}, Query{Offset: -5, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(0), selectArgs[len(selectArgs)-1], "a negative Offset must be clamped to 0, not sent to Postgres, which rejects it")
}

func TestBuildPostgresQueries_ZeroOrNegativeLimitUsesTheDefault(t *testing.T) {
	_, _, _, selectArgs, err := buildPostgresQueries(testTable, PostgresConfig{}, Query{Limit: -1})
	require.NoError(t, err)
	assert.Equal(t, int64(defaultPostgresLimit), selectArgs[len(selectArgs)-2])
}

func TestBuildPostgresQueries_CountAndSelectShareTheSameWhereArgs(t *testing.T) {
	countQuery, countArgs, selectQuery, selectArgs, err := buildPostgresQueries(testTable, PostgresConfig{}, Query{
		Filters: []Filter{Eq(testCity, "Alameda")},
	})
	require.NoError(t, err)
	assert.Contains(t, countQuery, "WHERE document->>$1 = $2")
	assert.Contains(t, selectQuery, "WHERE document->>$1 = $2")
	assert.Equal(t, countArgs, selectArgs[:len(countArgs)], "the select query's leading args must match the count query's args exactly")
}

func TestComputeSearchText_ConcatenatesOnlyDeclaredStringFields(t *testing.T) {
	raw := []byte(`{"title":"Charming bungalow","city":"Alameda","price":900000,"nested":{"a":1}}`)
	text, err := computeSearchText([]string{testTitle, testCity, testField, "missing"}, raw)
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
	_, err := computeSearchText([]string{testTitle}, []byte(`not json`))
	require.Error(t, err)
}

func TestQuoteIdent(t *testing.T) {
	assert.Equal(t, `"documents"`, quoteIdent(testTable))
	assert.Equal(t, `"weird""name"`, quoteIdent(`weird"name`),
		"an embedded quote must be doubled, not stripped or left to close the identifier early")
}
