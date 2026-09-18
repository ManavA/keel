package meili

import (
	"context"
	"testing"

	"github.com/meilisearch/meilisearch-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/search"
)

func testSearcher(client *fakeClient, index *fakeIndex, cfg Config) *Searcher {
	return newSearcherWithFakes(client, index, cfg)
}

// TestSearcher_HonorsAnAlreadyCanceledContext checks the one thing Searcher
// CAN do with ctx, given that meilisearch-go@v0.26.1 accepts no context on
// these calls: refuse to even try when ctx is already done. Every method
// below must not reach the fake at all in that case.
func TestSearcher_HonorsAnAlreadyCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	idx := &fakeIndex{
		getSettingsFn: func() (*meilisearch.Settings, error) { called = true; return &meilisearch.Settings{}, nil },
		getStatsFn:    func() (*meilisearch.StatsIndex, error) { called = true; return &meilisearch.StatsIndex{}, nil },
		searchFn: func(string, *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error) {
			called = true
			return &meilisearch.SearchResponse{}, nil
		},
	}
	client := &fakeClient{}
	s := testSearcher(client, idx, Config{})

	assert.Error(t, s.Health(ctx))
	assert.Error(t, s.SetupIndex(ctx))
	assert.Error(t, s.IndexDocuments(ctx, []search.Document{search.MapDocument{"id": "1"}}))
	assert.Error(t, s.UpdateDocuments(ctx, []search.Document{search.MapDocument{"id": "1"}}))
	assert.Error(t, s.RemoveDocuments(ctx, []string{"1"}))
	_, err := s.PruneStale(ctx, nil)
	assert.Error(t, err)
	_, err = s.DocumentCount(ctx)
	assert.Error(t, err)
	_, err = s.CheckSettings(ctx)
	assert.Error(t, err)
	_, err = s.Search(ctx, search.Query{})
	assert.Error(t, err)

	assert.False(t, called, "an already-canceled context must stop every call before it reaches Meilisearch")
	assert.Zero(t, client.createIndexN)
}

func TestSetupIndex_AppliesEveryDeclaredSetting(t *testing.T) {
	var gotSearchable, gotFilterable, gotSortable, gotRanking []string
	var gotSynonyms map[string][]string
	var gotMaxTotalHits, gotMaxFacet int64

	idx := &fakeIndex{
		updateSearchableAttributesFn: func(r *[]string) (*meilisearch.TaskInfo, error) {
			gotSearchable = *r
			return &meilisearch.TaskInfo{}, nil
		},
		updateFilterableAttributesFn: func(r *[]string) (*meilisearch.TaskInfo, error) {
			gotFilterable = *r
			return &meilisearch.TaskInfo{}, nil
		},
		updateSortableAttributesFn: func(r *[]string) (*meilisearch.TaskInfo, error) {
			gotSortable = *r
			return &meilisearch.TaskInfo{}, nil
		},
		updateRankingRulesFn: func(r *[]string) (*meilisearch.TaskInfo, error) {
			gotRanking = *r
			return &meilisearch.TaskInfo{}, nil
		},
		updateSynonymsFn: func(r *map[string][]string) (*meilisearch.TaskInfo, error) {
			gotSynonyms = *r
			return &meilisearch.TaskInfo{}, nil
		},
		updatePaginationFn: func(r *meilisearch.Pagination) (*meilisearch.TaskInfo, error) {
			gotMaxTotalHits = r.MaxTotalHits
			return &meilisearch.TaskInfo{}, nil
		},
		updateFacetingFn: func(r *meilisearch.Faceting) (*meilisearch.TaskInfo, error) {
			gotMaxFacet = r.MaxValuesPerFacet
			return &meilisearch.TaskInfo{}, nil
		},
	}
	cfg := Config{
		UID:               testUID,
		Searchable:        []string{"name", "description"},
		Filterable:        []string{testStatus},
		Sortable:          []string{testPrice},
		RankingRules:      []string{testWords, testTypo},
		Synonyms:          map[string][]string{"sf": {testSF}},
		MaxTotalHits:      20000,
		MaxValuesPerFacet: 500,
	}
	s := testSearcher(&fakeClient{}, idx, cfg)

	require.NoError(t, s.SetupIndex(context.Background()))
	assert.Equal(t, cfg.Searchable, gotSearchable)
	assert.Equal(t, cfg.Filterable, gotFilterable)
	assert.Equal(t, cfg.Sortable, gotSortable)
	assert.Equal(t, cfg.RankingRules, gotRanking)
	assert.Equal(t, cfg.Synonyms, gotSynonyms)
	assert.Equal(t, cfg.MaxTotalHits, gotMaxTotalHits)
	assert.Equal(t, cfg.MaxValuesPerFacet, gotMaxFacet)
}

func TestSetupIndex_IndexAlreadyExistsIsNotAnError(t *testing.T) {
	client := &fakeClient{createIndexErr: errFake}
	s := testSearcher(client, &fakeIndex{}, Config{UID: testUID})
	require.NoError(t, s.SetupIndex(context.Background()),
		"CreateIndex failing (as it does on every call after the first) must not fail SetupIndex")
}

func TestSetupIndex_SearchableAttributesFailureIsReturned(t *testing.T) {
	idx := &fakeIndex{
		updateSearchableAttributesFn: func(*[]string) (*meilisearch.TaskInfo, error) { return nil, errFake },
	}
	s := testSearcher(&fakeClient{}, idx, Config{UID: testUID})
	err := s.SetupIndex(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "searchable")
}

func TestSetupIndex_ZeroCapsAreNotSent(t *testing.T) {
	called := false
	idx := &fakeIndex{
		updatePaginationFn: func(*meilisearch.Pagination) (*meilisearch.TaskInfo, error) {
			called = true
			return &meilisearch.TaskInfo{}, nil
		},
	}
	s := testSearcher(&fakeClient{}, idx, Config{UID: testUID}) // MaxTotalHits unset
	require.NoError(t, s.SetupIndex(context.Background()))
	assert.False(t, called, "an unset MaxTotalHits must leave Meilisearch's own default alone")
}

func TestHealth(t *testing.T) {
	s := testSearcher(&fakeClient{}, &fakeIndex{}, Config{})
	assert.NoError(t, s.Health(context.Background()))

	s2 := testSearcher(&fakeClient{healthErr: errFake}, &fakeIndex{}, Config{})
	assert.Error(t, s2.Health(context.Background()))
}

func TestIndexDocuments_EmptyIsANoop(t *testing.T) {
	idx := &fakeIndex{
		addDocumentsFn: func(interface{}, ...string) (*meilisearch.TaskInfo, error) {
			t.Fatal("AddDocuments must not be called for an empty batch")
			return nil, nil
		},
	}
	s := testSearcher(&fakeClient{}, idx, Config{})
	require.NoError(t, s.IndexDocuments(context.Background(), nil))
}

func TestIndexDocuments_AwaitsTheTask(t *testing.T) {
	waited := false
	idx := &fakeIndex{
		waitForTaskFn: func(taskUID int64, options ...meilisearch.WaitParams) (*meilisearch.Task, error) {
			waited = true
			require.NotEmpty(t, options, "WaitForTask must always be called with explicit WaitParams (nil options apply an internal 5s timeout)")
			require.NotNil(t, options[0].Context, "a nil Context panics inside the library's own select")
			require.NotZero(t, options[0].Interval, "a zero Interval panics time.NewTicker")
			return &meilisearch.Task{Status: meilisearch.TaskStatusSucceeded}, nil
		},
	}
	s := testSearcher(&fakeClient{}, idx, Config{PrimaryKey: "id"})
	require.NoError(t, s.IndexDocuments(context.Background(), []search.Document{search.MapDocument{"id": "1"}}))
	assert.True(t, waited)
}

func TestIndexDocuments_FailedTaskIsAnError(t *testing.T) {
	idx := &fakeIndex{
		waitForTaskFn: func(int64, ...meilisearch.WaitParams) (*meilisearch.Task, error) {
			task := &meilisearch.Task{Status: meilisearch.TaskStatusFailed}
			task.Error.Message = "boom" // Error's type is unexported; its fields are not.
			return task, nil
		},
	}
	s := testSearcher(&fakeClient{}, idx, Config{})
	err := s.IndexDocuments(context.Background(), []search.Document{search.MapDocument{"id": "1"}})
	require.Error(t, err, "a task that finished in any status other than Succeeded did NOT do the work")
	assert.Contains(t, err.Error(), "boom")
}

func TestPrimaryKeyArg_EmptyConfigYieldsNilNotEmptyString(t *testing.T) {
	s := testSearcher(&fakeClient{}, &fakeIndex{}, Config{})
	assert.Nil(t, s.primaryKeyArg(), "an unset PrimaryKey must produce a nil slice, not [\"\"]")

	s2 := testSearcher(&fakeClient{}, &fakeIndex{}, Config{PrimaryKey: "id"})
	assert.Equal(t, []string{"id"}, s2.primaryKeyArg())
}

func TestRemoveDocuments_BatchesAtOneThousand(t *testing.T) {
	idx := &fakeIndex{}
	s := testSearcher(&fakeClient{}, idx, Config{})

	ids := make([]string, 1500)
	for i := range ids {
		ids[i] = "id"
	}
	require.NoError(t, s.RemoveDocuments(context.Background(), ids))
	require.Len(t, idx.deleteCalls, 2)
	assert.Len(t, idx.deleteCalls[0], 1000)
	assert.Len(t, idx.deleteCalls[1], 500)
}

func TestPruneStale_DeletesOnlyWhatIsNotInKeep(t *testing.T) {
	idx := &fakeIndex{
		getDocumentsFn: func(q *meilisearch.DocumentsQuery, resp *meilisearch.DocumentsResult) error {
			if q.Offset == 0 {
				resp.Results = []map[string]interface{}{{"id": testKeep1}, {"id": "stale-1"}}
			}
			return nil
		},
	}
	s := testSearcher(&fakeClient{}, idx, Config{PrimaryKey: "id"})

	n, err := s.PruneStale(context.Background(), map[string]struct{}{testKeep1: {}})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.Len(t, idx.deleteCalls, 1)
	assert.Equal(t, []string{"stale-1"}, idx.deleteCalls[0])
}

func TestPruneStale_NothingStaleDeletesNothing(t *testing.T) {
	idx := &fakeIndex{
		getDocumentsFn: func(q *meilisearch.DocumentsQuery, resp *meilisearch.DocumentsResult) error {
			if q.Offset == 0 {
				resp.Results = []map[string]interface{}{{"id": testKeep1}}
			}
			return nil
		},
	}
	s := testSearcher(&fakeClient{}, idx, Config{PrimaryKey: "id"})
	n, err := s.PruneStale(context.Background(), map[string]struct{}{testKeep1: {}})
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, idx.deleteCalls)
}

func TestDocumentCount(t *testing.T) {
	idx := &fakeIndex{getStatsFn: func() (*meilisearch.StatsIndex, error) {
		return &meilisearch.StatsIndex{NumberOfDocuments: 42}, nil
	}}
	s := testSearcher(&fakeClient{}, idx, Config{})
	n, err := s.DocumentCount(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 42, n)
}
