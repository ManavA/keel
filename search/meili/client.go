package meili

import "github.com/meilisearch/meilisearch-go"

// clientAPI is the subset of *meilisearch.Client this package needs. It
// exists so tests can fake index creation without a live server; the
// concrete *meilisearch.Client satisfies it without any adapter.
type clientAPI interface {
	CreateIndex(config *meilisearch.IndexConfig) (*meilisearch.TaskInfo, error)
	Health() (*meilisearch.Health, error)
}

// indexAPI is the subset of *meilisearch.Index this package needs. Same
// rationale as clientAPI: Meilisearch has no in-memory test double, so
// [Searcher]'s own tests fake this interface to exercise the query builder
// and setup logic without a running Meilisearch instance. A test that
// needs a live server should use the `live` build tag, following the
// pattern in search/postgres_live_test.go.
type indexAPI interface {
	Search(query string, request *meilisearch.SearchRequest) (*meilisearch.SearchResponse, error)
	GetStats() (*meilisearch.StatsIndex, error)
	GetTasks(param *meilisearch.TasksQuery) (*meilisearch.TaskResult, error)
	WaitForTask(taskUID int64, options ...meilisearch.WaitParams) (*meilisearch.Task, error)
	GetDocuments(request *meilisearch.DocumentsQuery, resp *meilisearch.DocumentsResult) error
	AddDocuments(documentsPtr interface{}, primaryKey ...string) (*meilisearch.TaskInfo, error)
	UpdateDocuments(documentsPtr interface{}, primaryKey ...string) (*meilisearch.TaskInfo, error)
	DeleteDocuments(identifier []string) (*meilisearch.TaskInfo, error)
	GetSettings() (*meilisearch.Settings, error)
	UpdateRankingRules(request *[]string) (*meilisearch.TaskInfo, error)
	UpdateSearchableAttributes(request *[]string) (*meilisearch.TaskInfo, error)
	UpdateSynonyms(request *map[string][]string) (*meilisearch.TaskInfo, error)
	UpdateFilterableAttributes(request *[]string) (*meilisearch.TaskInfo, error)
	UpdateSortableAttributes(request *[]string) (*meilisearch.TaskInfo, error)
	UpdatePagination(request *meilisearch.Pagination) (*meilisearch.TaskInfo, error)
	UpdateFaceting(request *meilisearch.Faceting) (*meilisearch.TaskInfo, error)
}
