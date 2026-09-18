package meili

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/search"
)

func TestQuoteFilterValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"plain value", "active", `"active"`},
		{"space is preserved inside quotes", "Redwood City", `"Redwood City"`},
		{"hyphen is preserved inside quotes", "Rancho-Bernardo", `"Rancho-Bernardo"`},
		{"embedded quote is removed", `say "hi"`, `"say hi"`},
		{"embedded backslash is removed", `back\slash`, `"backslash"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, QuoteFilterValue(tt.value))
		})
	}
}

func TestBuildMeiliFilters(t *testing.T) {
	tests := []struct {
		name    string
		filters []search.Filter
		want    []string
	}{
		{"eq string is quoted", []search.Filter{search.Eq("status", "active")}, []string{`status = "active"`}},
		{"eq number is bare", []search.Filter{search.Eq("beds", 3)}, []string{"beds = 3"}},
		{"neq", []search.Filter{search.Neq("status", "sold")}, []string{`status != "sold"`}},
		{"gte", []search.Filter{search.Gte(testPrice, 500000)}, []string{"price >= 500000"}},
		{"lte", []search.Filter{search.Lte(testPrice, 900000)}, []string{"price <= 900000"}},
		{"gt", []search.Filter{search.Gt("beds", 2)}, []string{"beds > 2"}},
		{"lt", []search.Filter{search.Lt("beds", 5)}, []string{"beds < 5"}},
		{"in quotes every value", []search.Filter{search.In("city", []string{"Alameda", "Palo Alto"})}, []string{`city IN ["Alameda", "Palo Alto"]`}},
		{"empty in produces nothing", []search.Filter{search.In("city", nil)}, []string{}},
		{"raw passes through verbatim", []search.Filter{search.Raw("_geoBoundingBox([1,2],[3,4])")}, []string{"_geoBoundingBox([1,2],[3,4])"}},
		{"empty raw produces nothing", []search.Filter{search.Raw("")}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildMeiliFilters(tt.filters)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildMeiliFilters_UnsupportedOpIsAnError(t *testing.T) {
	_, err := buildMeiliFilters([]search.Filter{{Field: "x", Op: "bogus"}})
	require.Error(t, err)
}

// TestBuildMeiliFilters_RangeOpsQuoteAHostileStringValue is the regression
// test for the range ops (Gte/Lte/Gt/Lt) interpolating Value bare: a string
// value reached the filter expression unquoted, so a value like
// "0 OR secret = 1" became a second clause instead of an inert comparison
// against a number. Value is `any`, and range filters are the ones most
// often built directly from caller-facing input (a query-string "min
// price"), so a string reaching one is not hypothetical.
func TestBuildMeiliFilters_RangeOpsQuoteAHostileStringValue(t *testing.T) {
	hostile := "0 OR secret = 1"
	tests := []struct {
		name   string
		filter search.Filter
		want   string
	}{
		{"gte", search.Gte(testPrice, hostile), `price >= "0 OR secret = 1"`},
		{"lte", search.Lte(testPrice, hostile), `price <= "0 OR secret = 1"`},
		{"gt", search.Gt("price", hostile), `price > "0 OR secret = 1"`},
		{"lt", search.Lt("price", hostile), `price < "0 OR secret = 1"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildMeiliFilters([]search.Filter{tt.filter})
			require.NoError(t, err)
			require.Len(t, got, 1)
			// The hostile text must appear exactly once, inside the quotes
			// the whole expression ends with — never as a second,
			// unquoted clause the way `price >= 0 OR secret = 1` would be.
			assert.Equal(t, tt.want, got[0])
			assert.Equal(t, 2, strings.Count(got[0], "\""), "the value must be wrapped in exactly one pair of quotes")
		})
	}
}

func TestBuildMeiliFilters_RangeOpsPrintNumericValuesBare(t *testing.T) {
	got, err := buildMeiliFilters([]search.Filter{search.Gte(testPrice, 500000)})
	require.NoError(t, err)
	assert.Equal(t, []string{"price >= 500000"}, got, "a numeric value must not be quoted")
}

func TestBuildMeiliSort(t *testing.T) {
	tests := []struct {
		name string
		sort []search.SortField
		want []string
	}{
		{"ascending", []search.SortField{{Field: testPrice, Dir: search.Asc}}, []string{testPriceAsc}},
		{"descending", []search.SortField{{Field: testPrice, Dir: search.Desc}}, []string{"price:desc"}},
		{"multiple fields", []search.SortField{{Field: testPrice, Dir: search.Asc}, {Field: testSqft, Dir: search.Desc}}, []string{testPriceAsc, "sqft:desc"}},
		{"empty", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, buildMeiliSort(tt.sort))
		})
	}
}
