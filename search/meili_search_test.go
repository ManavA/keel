package search

import (
	"context"
	"testing"

	"github.com/meilisearch/meilisearch-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearch_PassesTextFiltersAndSort(t *testing.T) {
	var gotQuery string
	var gotReq *meilisearch.SearchRequest
	idx := &fakeIndex{searchFn: func(query string, req *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error) {
		gotQuery = query
		gotReq = req
		return &meilisearch.SearchResponse{}, nil
	}}
	s := testSearcher(&fakeClient{}, idx, Config{})

	_, err := s.Search(context.Background(), Query{
		Text:    "bungalow",
		Filters: []Filter{Eq("city", "Alameda")},
		Sort:    []SortField{{Field: "price", Dir: Asc}},
		Offset:  20,
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, "bungalow", gotQuery)
	assert.Equal(t, []string{`city = "Alameda"`}, gotReq.Filter)
	assert.Equal(t, []string{"price:asc"}, gotReq.Sort)
	assert.EqualValues(t, 20, gotReq.Offset)
	assert.EqualValues(t, 10, gotReq.Limit)
}

func TestSearch_NoFiltersSendsNilNotEmptySlice(t *testing.T) {
	var gotReq *meilisearch.SearchRequest
	idx := &fakeIndex{searchFn: func(query string, req *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error) {
		gotReq = req
		return &meilisearch.SearchResponse{}, nil
	}}
	s := testSearcher(&fakeClient{}, idx, Config{})

	_, err := s.Search(context.Background(), Query{})
	require.NoError(t, err)
	assert.Nil(t, gotReq.Filter)
}

func TestSearch_DecodesHitsAndSkipsUnexpectedShapes(t *testing.T) {
	idx := &fakeIndex{searchFn: func(string, *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error) {
		return &meilisearch.SearchResponse{
			Hits:               []interface{}{map[string]interface{}{"id": "1"}, "not-a-map", map[string]interface{}{"id": "2"}},
			EstimatedTotalHits: 2,
		}, nil
	}}
	s := testSearcher(&fakeClient{}, idx, Config{})

	res, err := s.Search(context.Background(), Query{})
	require.NoError(t, err)
	require.Len(t, res.Hits, 2, "a hit that is not a map must be skipped, not panic the whole search")
	assert.Equal(t, "1", res.Hits[0]["id"])
	assert.Equal(t, "2", res.Hits[1]["id"])
	assert.EqualValues(t, 2, res.Total)
}

func TestSearch_DecodesFacetDistribution(t *testing.T) {
	idx := &fakeIndex{searchFn: func(string, *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error) {
		return &meilisearch.SearchResponse{
			FacetDistribution: map[string]interface{}{"city": map[string]interface{}{"Alameda": float64(3)}},
		}, nil
	}}
	s := testSearcher(&fakeClient{}, idx, Config{})

	res, err := s.Search(context.Background(), Query{Facets: []string{"city"}})
	require.NoError(t, err)
	require.NotNil(t, res.Facets)
	assert.Contains(t, res.Facets, "city")
}

func TestSearch_UnderlyingErrorIsWrapped(t *testing.T) {
	idx := &fakeIndex{searchFn: func(string, *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error) {
		return nil, errFake
	}}
	s := testSearcher(&fakeClient{}, idx, Config{})

	_, err := s.Search(context.Background(), Query{})
	require.Error(t, err)
}

func TestSearch_InvalidFilterOpIsReturnedBeforeCallingMeilisearch(t *testing.T) {
	called := false
	idx := &fakeIndex{searchFn: func(string, *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error) {
		called = true
		return &meilisearch.SearchResponse{}, nil
	}}
	s := testSearcher(&fakeClient{}, idx, Config{})

	_, err := s.Search(context.Background(), Query{Filters: []Filter{{Field: "x", Op: "bogus"}}})
	require.Error(t, err)
	assert.False(t, called, "an unbuildable filter must not reach Meilisearch at all")
}
