package pg_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
	"github.com/ManavA/keel/policy"
	policypg "github.com/ManavA/keel/policy/pg"
)

// A store built over a transaction shares it with its caller. Whatever the
// store refuses, the caller's transaction must still be usable afterwards: the
// failed statement of a transaction aborts it, and every statement after fails
// with SQLSTATE 25P02. The store checks what it can before it sends anything,
// and runs the insert in a savepoint, rolled back on failure, for what only the
// server can refuse.
func TestStore_OverATransactionARefusalLeavesTheTransactionUsable(t *testing.T) {
	var deep any = []any{}
	for range 20000 {
		deep = []any{deep}
	}
	tests := []struct {
		name     string
		change   func(*policy.Record)
		toServer bool // only the server can tell
	}{
		{"a lone surrogate in raw JSON", func(r *policy.Record) { r.Action.Attrs["raw"] = json.RawMessage(`"\ud800"`) }, true},
		{"a number past numeric's range", func(r *policy.Record) { r.Action.Attrs["n"] = json.Number("1e131072") }, true},
		{"a number with too many digits after the point", func(r *policy.Record) {
			r.Action.Attrs["n"] = json.Number("0." + strings.Repeat("1", 16384))
		}, true},
		{"a value nested too deeply", func(r *policy.Record) { r.Action.Attrs["deep"] = deep }, true},
		{"a rule name too large for its index", func(r *policy.Record) { r.Decision.Rule = hashed(400) }, true},
		{"invalid UTF-8 in the kind", func(r *policy.Record) { r.Action.Kind = "a\xffb" }, false},
		{"invalid UTF-8 in the target", func(r *policy.Record) { r.Action.Target = "a\xffb" }, false},
		{"invalid UTF-8 in the rule", func(r *policy.Record) { r.Decision.Rule = "a\xffb" }, false},
		{"invalid UTF-8 in the version", func(r *policy.Record) { r.Version = "a\xffb" }, false},
		{"a time in the year 300000", func(r *policy.Record) { r.At = time.Date(300000, 1, 1, 0, 0, 0, 0, time.UTC) }, false},
		{"a time too large for the clock to hold", func(r *policy.Record) { r.At = time.Unix(1<<60, 0) }, false},
		{"an index past the column's", func(r *policy.Record) { r.Decision.Index = 1 << 40 }, false},
		{"a NUL", func(r *policy.Record) { r.Action.Attrs["note"] = "a\x00b" }, false},
	}

	store, pool := openStore(t)
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	inTx := policypg.New(tx)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := inTx.Record(ctx, recordWith(tt.change))
			require.Error(t, err)
			assert.ErrorIs(t, err, policy.ErrUnrecordable)

			// The transaction goes on.
			var one int
			require.NoError(t, tx.QueryRow(ctx, `select 1`).Scan(&one), "the transaction was aborted by the refusal")
		})
	}

	// And records, in the same transaction, before and after.
	require.NoError(t, inTx.Record(ctx, sample("after", base)))
	require.NoError(t, tx.Commit(ctx))
	got, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"doc:after"}, targets(got), "only the record that was good is on the log")
}

// Over a pool the insert is the one statement it always was, and over a
// transaction it is inside a savepoint that is released when the record is
// written and rolled back to when it is not.
func TestStore_TheInsertIsOneStatementOverAPoolAndInASavepointOverATransaction(t *testing.T) {
	db := testdb.Shared(t)
	ctx := t.Context()
	var spy statementLog
	pool := tracedPool(t, db, &spy)
	store := policypg.New(pool)

	require.NoError(t, store.Record(ctx, sample("send", base)))
	assert.Len(t, spy.statements(), 1, "one statement")
	assert.Contains(t, spy.statements()[0], "insert into "+policypg.Table)
	assert.NotContains(t, strings.ToLower(strings.Join(spy.statements(), "\n")), "savepoint")

	spy.reset()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	spy.reset() // the begin is the caller's
	inTx := policypg.New(tx)

	require.NoError(t, inTx.Record(ctx, sample("send", base)))
	written := lower(spy.statements())
	require.Len(t, written, 3)
	assert.Contains(t, written[0], "savepoint")
	assert.Contains(t, written[1], "insert into "+policypg.Table)
	assert.Contains(t, written[2], "release savepoint")

	spy.reset()
	require.Error(t, inTx.Record(ctx, recordWith(func(r *policy.Record) { r.Action.Attrs["n"] = json.Number("1e131072") })))
	refused := lower(spy.statements())
	require.Len(t, refused, 3)
	assert.Contains(t, refused[0], "savepoint")
	assert.Contains(t, refused[1], "insert into "+policypg.Table)
	assert.Contains(t, refused[2], "rollback to savepoint")

	spy.reset()
	require.Error(t, inTx.Record(ctx, recordWith(func(r *policy.Record) { r.Action.Attrs["note"] = "a\x00b" })))
	assert.Empty(t, spy.statements(), "what Go can tell is not sent at all")
}

func lower(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}

// hashed is a name of 64 characters for each of n hashes: too large to be an
// entry in an index, and it does not compress.
func hashed(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%x", sha256.Sum256([]byte{byte(i), byte(i >> 8)}))
	}
	return b.String()
}

// statementLog is a pgx tracer that keeps the text of every statement sent.
type statementLog struct {
	mu  sync.Mutex
	sql []string
}

func (l *statementLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sql = append(l.sql, data.SQL)
	return ctx
}

func (l *statementLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (l *statementLog) statements() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.sql...)
}

func (l *statementLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sql = nil
}

// tracedPool is a pool on a new schema, migrated, that logs each statement it
// sends after the migration.
func tracedPool(t *testing.T, db *testdb.DB, log *statementLog) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("policy_pg_%d", schemaCounter.Add(1))
	admin, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL, MaxConns: 2})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	admin.Close()

	cfg, err := pgxpool.ParseConfig(db.URL + "&search_path=" + schema)
	require.NoError(t, err)
	cfg.MaxConns = 4
	cfg.ConnConfig.Tracer = log
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = migrate.Run(ctx, pool, migrate.Options{FS: policypg.MigrationsFS, Dir: "migrations"})
	require.NoError(t, err)
	log.reset()
	return pool
}
