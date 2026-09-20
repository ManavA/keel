package search

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresIndex_Health_ForwardsPingError(t *testing.T) {
	idx := newPostgresIndexWithConn(&fakeConn{pingErr: errFake}, PostgresConfig{Table: testTable})
	assert.ErrorIs(t, idx.Health(context.Background()), errFake)
}

func TestPostgresIndex_EnsureSchema_CreatesTableThenIndex(t *testing.T) {
	conn := &fakeConn{}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})

	require.NoError(t, idx.EnsureSchema(context.Background()))
	require.Len(t, conn.execCalls, 2)
	assert.Contains(t, conn.execCalls[0].sql, "CREATE TABLE IF NOT EXISTS")
	assert.Contains(t, conn.execCalls[0].sql, `"documents"`)
	assert.Contains(t, conn.execCalls[1].sql, "CREATE INDEX IF NOT EXISTS")
}

func TestPostgresIndex_RemoveDocuments_EmptyIsANoop(t *testing.T) {
	conn := &fakeConn{}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})
	require.NoError(t, idx.RemoveDocuments(context.Background(), nil))
	assert.Empty(t, conn.execCalls, "no ids means no DELETE should be sent at all")
}

func TestPostgresIndex_DocumentCount_ReadsScannedValue(t *testing.T) {
	conn := &fakeConn{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return fakeRow{scanFn: func(dest ...any) error {
				*(dest[0].(*int64)) = 7
				return nil
			}}
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})

	n, err := idx.DocumentCount(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 7, n)
}

// TestPostgresIndex_PruneStale_ReturnsTheActualDeletedCount is the
// regression test: a
// PruneStale that always returned 0 stayed green under the default test
// suite and was caught only by a `live`-tagged test against a real
// database. This test needs no live database — it fakes the "list all ids"
// query and asserts the return value matches the ids the fake reports as
// not in keep.
func TestPostgresIndex_PruneStale_ReturnsTheActualDeletedCount(t *testing.T) {
	conn := &fakeConn{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &fakeRows{rows: [][]any{{testKeep1}, {testStale1}, {testStale2}}}, nil
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})

	n, err := idx.PruneStale(context.Background(), map[string]struct{}{testKeep1: {}})
	require.NoError(t, err)
	assert.Equal(t, 2, n, "PruneStale must report exactly how many rows it deleted, not a placeholder")

	require.Len(t, conn.execCalls, 1, "the stale ids must actually be sent to DELETE")
	deleteArgs, ok := conn.execCalls[0].args[0].([]string)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{testStale1, testStale2}, deleteArgs)
}

func TestPostgresIndex_PruneStale_NothingStaleDeletesNothing(t *testing.T) {
	conn := &fakeConn{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &fakeRows{rows: [][]any{{testKeep1}}}, nil
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})

	n, err := idx.PruneStale(context.Background(), map[string]struct{}{testKeep1: {}})
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, conn.execCalls)
}

// TestPostgresIndex_Search_DefaultsToOrderByID_EndToEnd is the same N2
// regression as the pure-function test in postgres_query_test.go, exercised
// through the full Search call with a faked connection, so the pinning does
// not depend solely on Search actually calling buildPostgresQueries.
func TestPostgresIndex_Search_DefaultsToOrderByID_EndToEnd(t *testing.T) {
	conn := &fakeConn{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return fakeRow{scanFn: func(dest ...any) error {
				*(dest[0].(*int64)) = 1
				return nil
			}}
		},
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &fakeRows{rows: [][]any{{"1", []byte(`{"id":"1"}`)}}}, nil
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})

	_, err := idx.Search(context.Background(), Query{})
	require.NoError(t, err)

	require.NotEmpty(t, conn.queryCalls)
	assert.Contains(t, conn.queryCalls[len(conn.queryCalls)-1].sql, "ORDER BY id")
}

func TestPostgresIndex_Search_ZeroTotalSkipsTheSelectQuery(t *testing.T) {
	conn := &fakeConn{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return fakeRow{scanFn: func(dest ...any) error {
				*(dest[0].(*int64)) = 0
				return nil
			}}
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})

	res, err := idx.Search(context.Background(), Query{})
	require.NoError(t, err)
	assert.Empty(t, res.Hits)
	assert.Zero(t, res.Total)
	assert.Empty(t, conn.queryCalls, "a zero total must skip the paged SELECT entirely")
}

// TestPostgresIndex_Search_OffsetPastEndKeepsRealTotal is the regression test
// for the deep-page total: an Offset past every matching row must return zero
// hits with the real total, not a zero total. Search uses a separate COUNT(*)
// query rather than a COUNT(*) OVER() window on the paged query exactly so
// the total survives an empty page; this test pins that through the full
// Search call with a faked connection.
func TestPostgresIndex_Search_OffsetPastEndKeepsRealTotal(t *testing.T) {
	conn := &fakeConn{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return fakeRow{scanFn: func(dest ...any) error {
				*(dest[0].(*int64)) = 3
				return nil
			}}
		},
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &fakeRows{}, nil
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})

	res, err := idx.Search(context.Background(), Query{Offset: 3, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, res.Hits)
	assert.EqualValues(t, 3, res.Total, "an offset past every row must still report the real total")
	assert.NotEmpty(t, conn.queryCalls, "a nonzero total must still run the paged SELECT even when the page comes back empty")
}

// TestPostgresIndex_Search_WrapsInvalidTextRepresentation is the regression
// test for a numeric filter over a mixed-type JSONB field surfacing the
// driver's raw error, which echoes the offending value back to the caller.
func TestPostgresIndex_Search_WrapsInvalidTextRepresentation(t *testing.T) {
	pgErr := &pgconn.PgError{Code: pgInvalidTextRepresentation, Message: `invalid input syntax for type numeric: "not a number"`}
	conn := &fakeConn{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return fakeRow{scanFn: func(dest ...any) error { return pgErr }}
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})

	_, err := idx.Search(context.Background(), Query{Filters: []Filter{Gte("price", 50)}})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not a number", "the raw offending value must not reach the caller")
	assert.NotContains(t, err.Error(), "invalid input syntax")

	// The containment must survive unwrapping too: this is why wrapPgError
	// builds a fresh error instead of using %w.
	var recovered *pgconn.PgError
	assert.False(t, errors.As(err, &recovered),
		"the original *pgconn.PgError, and the offending value inside it, must not be recoverable via errors.As")
}

func TestPostgresIndex_Search_OtherPgErrorsPassThroughUnchanged(t *testing.T) {
	conn := &fakeConn{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return fakeRow{scanFn: func(dest ...any) error {
				return &pgconn.PgError{Code: "42601", Message: "syntax error"}
			}}
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})

	_, err := idx.Search(context.Background(), Query{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "syntax error", "only the specific invalid-input code is redacted; other errors keep their detail")
}

func TestWrapPgError_NonPgErrorPassesThrough(t *testing.T) {
	assert.ErrorIs(t, wrapPgError(errFake), errFake)
}

func TestPostgresIndex_IndexDocuments_PassesTextSearchConfigToBatch(t *testing.T) {
	conn := &fakeConn{
		sendBatchFn: func(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
			require.Len(t, b.QueuedQueries, 1)
			args := b.QueuedQueries[0].Arguments
			assert.Equal(t, "english", args[len(args)-1], "the configured text search config must reach the batch as a parameter")
			return &fakeBatchResults{n: b.Len()}
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable, TextSearchConfig: "english"})

	err := idx.IndexDocuments(context.Background(), []Document{MapDocument{"id": "1", "title": "x"}})
	require.NoError(t, err)
}

func TestPostgresIndex_IndexDocuments_EmptyIDIsAnError(t *testing.T) {
	idx := newPostgresIndexWithConn(&fakeConn{}, PostgresConfig{Table: testTable})
	err := idx.IndexDocuments(context.Background(), []Document{MapDocument{"title": "no id"}})
	require.Error(t, err)
}

func TestPostgresIndex_IndexDocuments_BatchExecFailureIsWrapped(t *testing.T) {
	conn := &fakeConn{
		sendBatchFn: func(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
			return &fakeBatchResults{n: b.Len(), execErr: errors.New("constraint violation")}
		},
	}
	idx := newPostgresIndexWithConn(conn, PostgresConfig{Table: testTable})
	err := idx.IndexDocuments(context.Background(), []Document{MapDocument{"id": "1"}})
	require.Error(t, err)
}

func TestStaleIDs(t *testing.T) {
	tests := []struct {
		name string
		all  []string
		keep map[string]struct{}
		want []string
	}{
		{"nothing stale", []string{"a"}, map[string]struct{}{"a": {}}, nil},
		{"one stale", []string{"a", "b"}, map[string]struct{}{"a": {}}, []string{"b"}},
		{"empty ids skipped", []string{"", "a"}, map[string]struct{}{}, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, staleIDs(tt.all, tt.keep))
		})
	}
}
