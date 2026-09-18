package search

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
// pattern in postgres_live_test.go.
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

// Document is anything that can be stored in the index. A struct
// implementing ID (whatever its JSON tags name that field) or a
// [MapDocument] both work; this package never assumes more about a
// document's shape than "it JSON-marshals, and it has an id."
type Document interface {
	// ID returns the document's primary-key value. It must match the JSON
	// field name given as Config.PrimaryKey when the document is marshaled.
	ID() string
}

// MapDocument is a schema-less [Document] backed by a plain map. ID reads
// the "id" key as a string; documents that key their primary key under a
// different JSON field should implement Document directly instead.
type MapDocument map[string]any

// ID returns the "id" key as a string, or "" if it is absent or not a
// string.
func (d MapDocument) ID() string {
	v, _ := d["id"].(string)
	return v
}
