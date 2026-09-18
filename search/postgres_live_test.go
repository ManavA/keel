//go:build live

package search

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestLive_PostgresIndex_RoundTrip exercises PostgresIndex against a real
// Postgres, gated behind the `live` build tag and KEEL_TEST_POSTGRES_DSN so
// the rest of the suite never needs a database. Run with:
//
//	KEEL_TEST_POSTGRES_DSN=postgres://... go test -tags=live ./search/...
func TestLive_PostgresIndex_RoundTrip(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_POSTGRES_DSN not set; skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	table := fmt.Sprintf("keel_search_test_%d", time.Now().UnixNano())
	idx := NewPostgresIndex(PostgresIndexOptions{
		Pool: pool,
		Config: PostgresConfig{
			Table:            table,
			SearchableFields: []string{"title", "city"},
		},
	})
	defer func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+quoteIdent(table)) }()

	require.NoError(t, idx.EnsureSchema(ctx))
	require.NoError(t, idx.Health(ctx))

	docs := []Document{
		MapDocument{"id": "1", "title": "Charming bungalow", "city": "Alameda", "price": float64(750000)},
		MapDocument{"id": "2", "title": "Modern condo", "city": "Oakland", "price": float64(500000)},
		MapDocument{"id": "3", "title": "Spacious bungalow", "city": "Alameda", "price": float64(1200000)},
	}
	require.NoError(t, idx.IndexDocuments(ctx, docs))

	count, err := idx.DocumentCount(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, count)

	// Free-text + equality filter + numeric sort.
	res, err := idx.Search(ctx, Query{
		Text:    "bungalow",
		Filters: []Filter{Eq("city", "Alameda")},
		Sort:    []SortField{{Field: "price", Dir: Asc, Numeric: true}},
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, res.Total)
	require.Len(t, res.Hits, 2)
	require.Equal(t, "1", res.Hits[0]["id"])
	require.Equal(t, "3", res.Hits[1]["id"])

	// Partial update merges rather than replaces.
	require.NoError(t, idx.UpdateDocuments(ctx, []Document{
		MapDocument{"id": "1", "price": float64(725000)},
	}))
	res, err = idx.Search(ctx, Query{Filters: []Filter{Eq("id", "1")}})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	require.Equal(t, "Charming bungalow", res.Hits[0]["title"], "a partial update must not erase fields it did not mention")
	require.EqualValues(t, 725000, res.Hits[0]["price"])

	// PruneStale removes anything not in keep.
	pruned, err := idx.PruneStale(ctx, map[string]struct{}{"1": {}, "3": {}})
	require.NoError(t, err)
	require.Equal(t, 1, pruned)
	count, err = idx.DocumentCount(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, count)

	require.NoError(t, idx.RemoveDocuments(ctx, []string{"1"}))
	count, err = idx.DocumentCount(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
}

// TestLive_PostgresIndex_SearchBeyondLastPageReportsCorrectTotal is the
// control for the two-query Total design in Search: an Offset past every
// matching row must still report the real total, not zero.
func TestLive_PostgresIndex_SearchBeyondLastPageReportsCorrectTotal(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_POSTGRES_DSN not set; skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	table := fmt.Sprintf("keel_search_test_%d", time.Now().UnixNano())
	idx := NewPostgresIndex(PostgresIndexOptions{Pool: pool, Config: PostgresConfig{Table: table}})
	defer func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+quoteIdent(table)) }()

	require.NoError(t, idx.EnsureSchema(ctx))
	require.NoError(t, idx.IndexDocuments(ctx, []Document{
		MapDocument{"id": "1"}, MapDocument{"id": "2"},
	}))

	res, err := idx.Search(ctx, Query{Offset: 50, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, res.Hits)
	require.EqualValues(t, 2, res.Total, "an offset past every row must still report the real total")
}
