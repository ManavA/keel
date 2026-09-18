package search

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresConfig declares one [PostgresIndex]'s table.
type PostgresConfig struct {
	// Table is the documents table name. Required. Quoted as an
	// identifier wherever it appears in generated SQL (see quoteIdent) —
	// it never needs to be a valid bare identifier itself.
	Table string
	// SearchableFields lists the JSON document fields concatenated into
	// the table's tsvector column at write time, for Query.Text matching.
	// A field whose value is missing or is not a JSON string is skipped.
	SearchableFields []string
}

// PostgresIndexOptions configures a [PostgresIndex].
type PostgresIndexOptions struct {
	// Pool is an already-opened connection pool. Required; its lifecycle
	// (including Close) belongs to the caller.
	Pool *pgxpool.Pool
	// Config declares the table and searchable fields.
	Config PostgresConfig
	// Logger receives this index's log lines. Nil falls back to
	// slog.Default(); this package never calls slog.SetDefault.
	Logger *slog.Logger
}

// PostgresIndex is an [Index] backed by a single Postgres table, for
// document search without running Meilisearch.
//
// It stores one JSONB column holding the whole document and one tsvector
// column for free-text matching, and translates filters to
// `document->>field` comparisons. This is suitable for small tables. It has
// no equivalent to Meilisearch's inverted index, no typo tolerance, and no
// facet counts; Query.Facets and Query.AttributesToRetrieve are ignored.
// Switch to [Searcher] when result-set size, facets, or typo tolerance
// matter.
type PostgresIndex struct {
	pool   *pgxpool.Pool
	cfg    PostgresConfig
	logger *slog.Logger
}

// NewPostgresIndex builds a PostgresIndex from opts. Call [PostgresIndex.EnsureSchema]
// once before using it against a fresh database.
func NewPostgresIndex(opts PostgresIndexOptions) *PostgresIndex {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &PostgresIndex{pool: opts.Pool, cfg: opts.Config, logger: logger}
}

// EnsureSchema creates the documents table and its search index if they do
// not already exist. It never drops or alters an existing table, so it is
// safe to call on every process start.
func (p *PostgresIndex) EnsureSchema(ctx context.Context) error {
	table := quoteIdent(p.cfg.Table)
	if _, err := p.pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			document JSONB NOT NULL,
			search_text TSVECTOR NOT NULL DEFAULT ''
		)
	`, table)); err != nil {
		return fmt.Errorf("create table %s: %w", p.cfg.Table, err)
	}
	if _, err := p.pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s ON %s USING GIN (search_text)`,
		quoteIdent(p.cfg.Table+"_search_text_idx"), table,
	)); err != nil {
		return fmt.Errorf("create search index on %s: %w", p.cfg.Table, err)
	}
	return nil
}

// Health pings the pool.
func (p *PostgresIndex) Health(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// IndexDocuments upserts docs: an existing id's document is replaced
// wholesale (use [PostgresIndex.UpdateDocuments] for a partial merge).
func (p *PostgresIndex) IndexDocuments(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}
	table := quoteIdent(p.cfg.Table)
	batch := &pgx.Batch{}
	for _, d := range docs {
		id := d.ID()
		if id == "" {
			return fmt.Errorf("index document: document has an empty id")
		}
		raw, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("marshal document %q: %w", id, err)
		}
		text, err := computeSearchText(p.cfg.SearchableFields, raw)
		if err != nil {
			return fmt.Errorf("document %q: %w", id, err)
		}
		batch.Queue(fmt.Sprintf(`
			INSERT INTO %s (id, document, search_text)
			VALUES ($1, $2, to_tsvector('simple', $3))
			ON CONFLICT (id) DO UPDATE SET document = EXCLUDED.document, search_text = EXCLUDED.search_text
		`, table), id, raw, text)
	}
	return p.runBatch(ctx, batch, len(docs), "index")
}

// UpdateDocuments partially updates docs already in the index — fields the
// document carries are merged (via JSONB `||`) into the existing document
// rather than replacing it, matching [Searcher.UpdateDocuments]. A
// document with no existing row is inserted as given, the same as
// IndexDocuments.
//
// The search_text column is recomputed from only the fields present in
// this partial write and concatenated onto the EXISTING tsvector, rather
// than rebuilt from the merged document — reading the merged document back
// first would cost a round trip this tier is meant to avoid. This can
// accumulate duplicate lexemes across repeated partial updates to the same
// field; harmless for `@@` matching, but not a source of truth for
// anything that counts occurrences.
func (p *PostgresIndex) UpdateDocuments(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}
	table := quoteIdent(p.cfg.Table)
	batch := &pgx.Batch{}
	for _, d := range docs {
		id := d.ID()
		if id == "" {
			return fmt.Errorf("update document: document has an empty id")
		}
		raw, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("marshal document %q: %w", id, err)
		}
		text, err := computeSearchText(p.cfg.SearchableFields, raw)
		if err != nil {
			return fmt.Errorf("document %q: %w", id, err)
		}
		batch.Queue(fmt.Sprintf(`
			INSERT INTO %[1]s (id, document, search_text)
			VALUES ($1, $2, to_tsvector('simple', $3))
			ON CONFLICT (id) DO UPDATE SET
				document = %[1]s.document || EXCLUDED.document,
				search_text = CASE WHEN $3 = '' THEN %[1]s.search_text ELSE %[1]s.search_text || EXCLUDED.search_text END
		`, table), id, raw, text)
	}
	return p.runBatch(ctx, batch, len(docs), "update")
}

func (p *PostgresIndex) runBatch(ctx context.Context, batch *pgx.Batch, n int, verb string) error {
	br := p.pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()
	for i := 0; i < n; i++ {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("%s document %d/%d: %w", verb, i+1, n, err)
		}
	}
	return nil
}

// RemoveDocuments deletes documents by id.
func (p *PostgresIndex) RemoveDocuments(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.pool.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE id = ANY($1)`, quoteIdent(p.cfg.Table)), ids)
	if err != nil {
		return fmt.Errorf("remove documents: %w", err)
	}
	return nil
}

// PruneStale removes every indexed document whose id is not in keep, and
// returns how many it deleted. See [Searcher.PruneStale] for why this
// exists as its own step rather than being implied by IndexDocuments.
func (p *PostgresIndex) PruneStale(ctx context.Context, keep map[string]struct{}) (int, error) {
	rows, err := p.pool.Query(ctx, fmt.Sprintf(`SELECT id FROM %s`, quoteIdent(p.cfg.Table)))
	if err != nil {
		return 0, fmt.Errorf("list indexed documents: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan indexed document id: %w", err)
		}
		if _, wanted := keep[id]; !wanted {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list indexed documents: %w", err)
	}

	if len(stale) == 0 {
		return 0, nil
	}
	if err := p.RemoveDocuments(ctx, stale); err != nil {
		return 0, err
	}
	return len(stale), nil
}

// DocumentCount reports how many documents the table currently holds.
func (p *PostgresIndex) DocumentCount(ctx context.Context) (int64, error) {
	var n int64
	err := p.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, quoteIdent(p.cfg.Table))).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count documents: %w", err)
	}
	return n, nil
}

// defaultPostgresLimit is used when Query.Limit is unset, matching
// [Searcher]'s own default page size so a caller switching backends sees
// the same default.
const defaultPostgresLimit = 20

// Search runs q against the table.
//
// Query.Facets and Query.AttributesToRetrieve are silently ignored — see
// the PostgresIndex doc for why. Total is computed with its own COUNT(*)
// query rather than a `COUNT(*) OVER()` window on the paged query, because
// the window approach reports 0 whenever Offset skips past every matching
// row, which is a wrong total, not just a missing page.
func (p *PostgresIndex) Search(ctx context.Context, q Query) (*Result, error) {
	var whereArgs []any
	where, err := buildPostgresWhere(q.Text, q.Filters, &whereArgs)
	if err != nil {
		return nil, err
	}

	total, err := p.countMatching(ctx, where, whereArgs)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return &Result{Hits: []map[string]any{}, Total: 0}, nil
	}

	args := append([]any(nil), whereArgs...)
	query := fmt.Sprintf("SELECT id, document FROM %s", quoteIdent(p.cfg.Table))
	if where != "" {
		query += " WHERE " + where
	}
	if orderClause := buildPostgresOrderBy(q.Sort, &args); orderClause != "" {
		query += " ORDER BY " + orderClause
	} else {
		// A stable default order. Without one, LIMIT/OFFSET paging over an
		// unordered result can return the same row on two different pages,
		// or skip one, because Postgres makes no ordering guarantee at all
		// absent an ORDER BY.
		query += " ORDER BY id"
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultPostgresLimit
	}
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, q.Offset)
	query += fmt.Sprintf(" OFFSET $%d", len(args))

	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	hits := make([]map[string]any, 0, limit)
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("decode document %q: %w", id, err)
		}
		hits = append(hits, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	return &Result{Hits: hits, Total: total}, nil
}

func (p *PostgresIndex) countMatching(ctx context.Context, where string, args []any) (int64, error) {
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", quoteIdent(p.cfg.Table))
	if where != "" {
		query += " WHERE " + where
	}
	var n int64
	if err := p.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count matching documents: %w", err)
	}
	return n, nil
}
