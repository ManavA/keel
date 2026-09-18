package meili

import (
	"errors"

	"github.com/meilisearch/meilisearch-go"
)

// fakeClient and fakeIndex let this package's tests exercise SetupIndex,
// Search, document sync, and settings-drift logic without a live
// Meilisearch instance — there is no in-memory Meilisearch to run instead.

type fakeClient struct {
	createIndexErr error
	createIndexN   int
	healthErr      error
}

func (c *fakeClient) CreateIndex(*meilisearch.IndexConfig) (*meilisearch.TaskInfo, error) {
	c.createIndexN++
	if c.createIndexErr != nil {
		return nil, c.createIndexErr
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (c *fakeClient) Health() (*meilisearch.Health, error) {
	if c.healthErr != nil {
		return nil, c.healthErr
	}
	return &meilisearch.Health{Status: "available"}, nil
}

// fakeIndex is a scriptable stand-in for *meilisearch.Index. Each field can
// be set by a test to force a particular return value or error; nil funcs
// fall back to an innocuous default.
type fakeIndex struct {
	searchFn                     func(query string, req *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error)
	getStatsFn                   func() (*meilisearch.StatsIndex, error)
	getTasksFn                   func(*meilisearch.TasksQuery) (*meilisearch.TaskResult, error)
	waitForTaskFn                func(taskUID int64, options ...meilisearch.WaitParams) (*meilisearch.Task, error)
	getDocumentsFn               func(*meilisearch.DocumentsQuery, *meilisearch.DocumentsResult) error
	addDocumentsFn               func(interface{}, ...string) (*meilisearch.TaskInfo, error)
	updateDocumentsFn            func(interface{}, ...string) (*meilisearch.TaskInfo, error)
	deleteDocumentsFn            func([]string) (*meilisearch.TaskInfo, error)
	getSettingsFn                func() (*meilisearch.Settings, error)
	updateRankingRulesFn         func(*[]string) (*meilisearch.TaskInfo, error)
	updateSearchableAttributesFn func(*[]string) (*meilisearch.TaskInfo, error)
	updateSynonymsFn             func(*map[string][]string) (*meilisearch.TaskInfo, error)
	updateFilterableAttributesFn func(*[]string) (*meilisearch.TaskInfo, error)
	updateSortableAttributesFn   func(*[]string) (*meilisearch.TaskInfo, error)
	updatePaginationFn           func(*meilisearch.Pagination) (*meilisearch.TaskInfo, error)
	updateFacetingFn             func(*meilisearch.Faceting) (*meilisearch.TaskInfo, error)

	// deleteCalls records every DeleteDocuments call, for tests that check
	// batching.
	deleteCalls [][]string
}

func (i *fakeIndex) Search(query string, req *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error) {
	if i.searchFn != nil {
		return i.searchFn(query, req)
	}
	return &meilisearch.SearchResponse{}, nil
}

func (i *fakeIndex) GetStats() (*meilisearch.StatsIndex, error) {
	if i.getStatsFn != nil {
		return i.getStatsFn()
	}
	return &meilisearch.StatsIndex{}, nil
}

func (i *fakeIndex) GetTasks(q *meilisearch.TasksQuery) (*meilisearch.TaskResult, error) {
	if i.getTasksFn != nil {
		return i.getTasksFn(q)
	}
	return &meilisearch.TaskResult{}, nil
}

func (i *fakeIndex) WaitForTask(taskUID int64, options ...meilisearch.WaitParams) (*meilisearch.Task, error) {
	if i.waitForTaskFn != nil {
		return i.waitForTaskFn(taskUID, options...)
	}
	return &meilisearch.Task{UID: taskUID, Status: meilisearch.TaskStatusSucceeded}, nil
}

func (i *fakeIndex) GetDocuments(q *meilisearch.DocumentsQuery, resp *meilisearch.DocumentsResult) error {
	if i.getDocumentsFn != nil {
		return i.getDocumentsFn(q, resp)
	}
	return nil
}

func (i *fakeIndex) AddDocuments(docs interface{}, primaryKey ...string) (*meilisearch.TaskInfo, error) {
	if i.addDocumentsFn != nil {
		return i.addDocumentsFn(docs, primaryKey...)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (i *fakeIndex) UpdateDocuments(docs interface{}, primaryKey ...string) (*meilisearch.TaskInfo, error) {
	if i.updateDocumentsFn != nil {
		return i.updateDocumentsFn(docs, primaryKey...)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (i *fakeIndex) DeleteDocuments(ids []string) (*meilisearch.TaskInfo, error) {
	i.deleteCalls = append(i.deleteCalls, ids)
	if i.deleteDocumentsFn != nil {
		return i.deleteDocumentsFn(ids)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (i *fakeIndex) GetSettings() (*meilisearch.Settings, error) {
	if i.getSettingsFn != nil {
		return i.getSettingsFn()
	}
	return &meilisearch.Settings{}, nil
}

func (i *fakeIndex) UpdateRankingRules(r *[]string) (*meilisearch.TaskInfo, error) {
	if i.updateRankingRulesFn != nil {
		return i.updateRankingRulesFn(r)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (i *fakeIndex) UpdateSearchableAttributes(r *[]string) (*meilisearch.TaskInfo, error) {
	if i.updateSearchableAttributesFn != nil {
		return i.updateSearchableAttributesFn(r)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (i *fakeIndex) UpdateSynonyms(r *map[string][]string) (*meilisearch.TaskInfo, error) {
	if i.updateSynonymsFn != nil {
		return i.updateSynonymsFn(r)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (i *fakeIndex) UpdateFilterableAttributes(r *[]string) (*meilisearch.TaskInfo, error) {
	if i.updateFilterableAttributesFn != nil {
		return i.updateFilterableAttributesFn(r)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (i *fakeIndex) UpdateSortableAttributes(r *[]string) (*meilisearch.TaskInfo, error) {
	if i.updateSortableAttributesFn != nil {
		return i.updateSortableAttributesFn(r)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (i *fakeIndex) UpdatePagination(r *meilisearch.Pagination) (*meilisearch.TaskInfo, error) {
	if i.updatePaginationFn != nil {
		return i.updatePaginationFn(r)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

func (i *fakeIndex) UpdateFaceting(r *meilisearch.Faceting) (*meilisearch.TaskInfo, error) {
	if i.updateFacetingFn != nil {
		return i.updateFacetingFn(r)
	}
	return &meilisearch.TaskInfo{TaskUID: 1}, nil
}

var errFake = errors.New("fake failure")
