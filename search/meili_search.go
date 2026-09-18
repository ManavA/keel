package search

import (
	"context"
	"fmt"

	"github.com/meilisearch/meilisearch-go"
)

// Search runs q against the Meilisearch index.
func (s *Searcher) Search(_ context.Context, q Query) (*Result, error) {
	filters, err := buildMeiliFilters(q.Filters)
	if err != nil {
		return nil, err
	}

	req := &meilisearch.SearchRequest{
		Query:                q.Text,
		Filter:               anyFilter(filters),
		Sort:                 buildMeiliSort(q.Sort),
		Offset:               q.Offset,
		Limit:                q.Limit,
		Facets:               q.Facets,
		AttributesToRetrieve: q.AttributesToRetrieve,
	}

	resp, err := s.index.Search(q.Text, req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	hits := make([]map[string]any, 0, len(resp.Hits))
	skipped := 0
	for _, hit := range resp.Hits {
		// Comma-ok: a hit that is not a map cannot become a result either
		// way, and the difference between the two is a panicked search
		// versus a logged, counted omission.
		m, ok := hit.(map[string]interface{})
		if !ok {
			skipped++
			continue
		}
		hits = append(hits, m)
	}
	if skipped > 0 {
		s.logger.Warn("search hits skipped: unexpected hit shape from Meilisearch",
			"skipped", skipped, "returned", len(hits), "query", q.Text)
	}

	var facets map[string]any
	if resp.FacetDistribution != nil {
		if f, ok := resp.FacetDistribution.(map[string]interface{}); ok {
			facets = f
		}
	}

	return &Result{Hits: hits, Total: resp.EstimatedTotalHits, Facets: facets}, nil
}

// anyFilter adapts []string to the interface{} meilisearch.SearchRequest.Filter
// expects, returning nil for an empty slice rather than an empty-but-non-nil
// one, which is the unambiguous "no filter" that the vast majority of
// queries actually want.
func anyFilter(filters []string) interface{} {
	if len(filters) == 0 {
		return nil
	}
	return filters
}
