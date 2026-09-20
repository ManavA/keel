package meili

import (
	"context"
	"testing"

	"github.com/meilisearch/meilisearch-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/search"
)

// cutoverConfig is the Config the cutover guide's procedure applies. It sets
// every setting so the tests below pin the full application order and the
// full verification read-back, not just one attribute.
func cutoverConfig() Config {
	return Config{
		UID:               testUID,
		PrimaryKey:        "id",
		Searchable:        []string{"name", "description"},
		Filterable:        []string{testStatus, testCity},
		Sortable:          []string{testPrice},
		RankingRules:      []string{testWords, testTypo},
		Synonyms:          map[string][]string{"sf": {testSF}},
		MaxTotalHits:      20000,
		MaxValuesPerFacet: 500,
	}
}

// cutoverLiveSettings is the live index state after SetupIndexAndVerify has
// applied cutoverConfig: every declared setting present with the same value.
func cutoverLiveSettings() *meilisearch.Settings {
	return &meilisearch.Settings{
		SearchableAttributes: []string{"name", "description"},
		FilterableAttributes: []string{testStatus, testCity},
		SortableAttributes:   []string{testPrice},
		RankingRules:         []string{testWords, testTypo},
		Synonyms:             map[string][]string{"sf": {testSF}},
		Pagination:           &meilisearch.Pagination{MaxTotalHits: 20000},
		Faceting:             &meilisearch.Faceting{MaxValuesPerFacet: 500},
	}
}

// TestCutover_SetupIndexAppliesSettingsInDocumentedOrder pins the order the
// cutover guide documents for SetupIndex: searchable, filterable, sortable,
// ranking rules, synonyms, pagination, faceting. The guide's backfill and
// verification steps assume this order is stable, so a reorder must update
// the guide, not slip through silently.
func TestCutover_SetupIndexAppliesSettingsInDocumentedOrder(t *testing.T) {
	var order []string
	record := func(name string) (*meilisearch.TaskInfo, error) { //nolint:unparam // signature dictated by the fakeIndex field type; the error is always nil
		order = append(order, name)
		return &meilisearch.TaskInfo{}, nil
	}
	idx := &fakeIndex{
		updateSearchableAttributesFn: func(*[]string) (*meilisearch.TaskInfo, error) { return record("searchable") },
		updateFilterableAttributesFn: func(*[]string) (*meilisearch.TaskInfo, error) { return record("filterable") },
		updateSortableAttributesFn:   func(*[]string) (*meilisearch.TaskInfo, error) { return record("sortable") },
		updateRankingRulesFn:         func(*[]string) (*meilisearch.TaskInfo, error) { return record("ranking") },
		updateSynonymsFn:             func(*map[string][]string) (*meilisearch.TaskInfo, error) { return record("synonyms") },
		updatePaginationFn:           func(*meilisearch.Pagination) (*meilisearch.TaskInfo, error) { return record("pagination") },
		updateFacetingFn:             func(*meilisearch.Faceting) (*meilisearch.TaskInfo, error) { return record("faceting") },
	}
	s := testSearcher(&fakeClient{}, idx, cutoverConfig())

	require.NoError(t, s.SetupIndex(context.Background()))
	assert.Equal(t,
		[]string{"searchable", "filterable", "sortable", "ranking", "synonyms", "pagination", "faceting"},
		order)
}

// TestCutover_VerifyThenDriftCheckThenGate walks the guide's cutover core
// against fakes: apply Config with SetupIndexAndVerify from the backfill
// caller, confirm the drift check is clean, confirm the startup gate passes,
// then backfill documents and confirm the count read-back used to compare
// both sides of the cutover.
func TestCutover_VerifyThenDriftCheckThenGate(t *testing.T) {
	ctx := context.Background()
	idx := &fakeIndex{
		getTasksFn: func(*meilisearch.TasksQuery) (*meilisearch.TaskResult, error) {
			return &meilisearch.TaskResult{}, nil
		},
		getSettingsFn: func() (*meilisearch.Settings, error) { return cutoverLiveSettings(), nil },
		getStatsFn: func() (*meilisearch.StatsIndex, error) {
			return &meilisearch.StatsIndex{NumberOfDocuments: 2}, nil
		},
	}
	s := testSearcher(&fakeClient{}, idx, cutoverConfig())

	report, err := s.SetupIndexAndVerify(ctx)
	require.NoError(t, err, "a fresh index with Config applied must verify clean")
	assert.True(t, report.OK())

	outcome, err := s.DriftCheck()(ctx)
	require.NoError(t, err, "the drift check must be clean once settings are verified")
	assert.True(t, outcome.OK())

	require.NoError(t, s.RequireSettings(ctx), "the startup gate must pass on the verified index")

	docs := []search.Document{
		search.MapDocument{"id": "1", "name": "bungalow"},
		search.MapDocument{"id": "2", "name": "cottage"},
	}
	require.NoError(t, s.IndexDocuments(ctx, docs))
	n, err := s.DocumentCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "the backfill count read-back must see what was written")
}
