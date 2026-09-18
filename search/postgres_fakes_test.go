package search

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var errFake = errors.New("fake failure")

// fakeConn, fakeRows, fakeRow and fakeBatchResults let this package's tests
// exercise PostgresIndex's query-building and result-processing logic
// without a live Postgres — there is no in-memory Postgres to run instead.
// Anything that needs to observe real Postgres behavior (constraint
// enforcement, the actual tsvector/JSONB semantics) is instead a
// `live`-tagged test; see postgres_live_test.go.

type fakeConn struct {
	execFn        func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryFn       func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	queryRowFn    func(ctx context.Context, sql string, args ...any) pgx.Row
	sendBatchFn   func(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	pingErr       error
	execCalls     []execCall
	queryCalls    []queryCall
	queryRowCalls []queryCall
}

type execCall struct {
	sql  string
	args []any
}

type queryCall struct {
	sql  string
	args []any
}

func (c *fakeConn) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.execCalls = append(c.execCalls, execCall{sql: sql, args: args})
	if c.execFn != nil {
		return c.execFn(ctx, sql, args...)
	}
	return pgconn.NewCommandTag(""), nil
}

func (c *fakeConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.queryCalls = append(c.queryCalls, queryCall{sql: sql, args: args})
	if c.queryFn != nil {
		return c.queryFn(ctx, sql, args...)
	}
	return &fakeRows{}, nil
}

func (c *fakeConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.queryRowCalls = append(c.queryRowCalls, queryCall{sql: sql, args: args})
	if c.queryRowFn != nil {
		return c.queryRowFn(ctx, sql, args...)
	}
	return fakeRow{}
}

func (c *fakeConn) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	if c.sendBatchFn != nil {
		return c.sendBatchFn(ctx, b)
	}
	return &fakeBatchResults{n: b.Len()}
}

func (c *fakeConn) Ping(context.Context) error { return c.pingErr }

// fakeRows serves a fixed set of rows, each scanned positionally into the
// Scan destinations in order.
type fakeRows struct {
	rows [][]any
	pos  int
	err  error
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("") }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return r.rows[r.pos-1], nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeRows) TypeMap() *pgtype.Map                         { return nil }

func (r *fakeRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	for i, d := range dest {
		switch v := d.(type) {
		case *string:
			*v = row[i].(string)
		case *[]byte:
			*v = row[i].([]byte)
		default:
			panic("fakeRows.Scan: unsupported destination type in test double")
		}
	}
	return nil
}

// fakeRow is a single-value pgx.Row, for QueryRow.
type fakeRow struct {
	scanFn func(dest ...any) error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return nil
}

// fakeBatchResults reports n successful Exec results and nothing else; the
// tests here only exercise batches built from Exec-style statements
// (INSERT ... ON CONFLICT).
type fakeBatchResults struct {
	n       int
	i       int
	execErr error
}

func (b *fakeBatchResults) Exec() (pgconn.CommandTag, error) {
	b.i++
	if b.execErr != nil {
		return pgconn.CommandTag{}, b.execErr
	}
	return pgconn.NewCommandTag(""), nil
}
func (b *fakeBatchResults) Query() (pgx.Rows, error) { return &fakeRows{}, nil }
func (b *fakeBatchResults) QueryRow() pgx.Row        { return fakeRow{} }
func (b *fakeBatchResults) Close() error             { return nil }
