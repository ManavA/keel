package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		filters []Filter
		want    []string
	}{
		{"eq string is quoted", []Filter{Eq("status", "active")}, []string{`status = "active"`}},
		{"eq number is bare", []Filter{Eq("beds", 3)}, []string{"beds = 3"}},
		{"neq", []Filter{Neq("status", "sold")}, []string{`status != "sold"`}},
		{"gte", []Filter{Gte("price", 500000)}, []string{"price >= 500000"}},
		{"lte", []Filter{Lte("price", 900000)}, []string{"price <= 900000"}},
		{"gt", []Filter{Gt("beds", 2)}, []string{"beds > 2"}},
		{"lt", []Filter{Lt("beds", 5)}, []string{"beds < 5"}},
		{"in quotes every value", []Filter{In("city", []string{"Alameda", "Palo Alto"})}, []string{`city IN ["Alameda", "Palo Alto"]`}},
		{"empty in produces nothing", []Filter{In("city", nil)}, []string{}},
		{"raw passes through verbatim", []Filter{Raw("_geoBoundingBox([1,2],[3,4])")}, []string{"_geoBoundingBox([1,2],[3,4])"}},
		{"empty raw produces nothing", []Filter{Raw("")}, []string{}},
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
	_, err := buildMeiliFilters([]Filter{{Field: "x", Op: "bogus"}})
	require.Error(t, err)
}

func TestBuildMeiliSort(t *testing.T) {
	tests := []struct {
		name string
		sort []SortField
		want []string
	}{
		{"ascending", []SortField{{Field: "price", Dir: Asc}}, []string{"price:asc"}},
		{"descending", []SortField{{Field: "price", Dir: Desc}}, []string{"price:desc"}},
		{"multiple fields", []SortField{{Field: "price", Dir: Asc}, {Field: "sqft", Dir: Desc}}, []string{"price:asc", "sqft:desc"}},
		{"empty", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, buildMeiliSort(tt.sort))
		})
	}
}
