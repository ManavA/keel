package pg_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/policy"
	policypg "github.com/ManavA/keel/policy/pg"
)

// fakeDB stands in for the pool where a test is about what the store sends, or
// whether it sends anything. It needs no database.
type fakeDB struct {
	execs, queries int
	tag            pgconn.CommandTag
	err            error
	rows           pgx.Rows
}

func (f *fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	f.execs++
	return f.tag, f.err
}

func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	f.queries++
	return f.rows, f.err
}

// brokenRows is a result that ends in an error instead of in its last row, as
// a connection lost in the middle of a read does.
type brokenRows struct{ err error }

func (r brokenRows) Close()                                       {}
func (r brokenRows) Err() error                                   { return r.err }
func (r brokenRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r brokenRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r brokenRows) Next() bool                                   { return false }
func (r brokenRows) Scan(...any) error                            { return errors.New("no row") }
func (r brokenRows) Values() ([]any, error)                       { return nil, errors.New("no row") }
func (r brokenRows) RawValues() [][]byte                          { return nil }
func (r brokenRows) Conn() *pgx.Conn                              { return nil }
func (r brokenRows) TypeMap() *pgtype.Map                         { return nil }

// recordWith is a valid record with one part replaced by the given change.
func recordWith(change func(*policy.Record)) policy.Record {
	rec := sample("send", base)
	rec.Action.Attrs = map[string]any{"to": "ap@example.com"}
	change(&rec)
	return rec
}

func TestStore_RecordRefusesWhatPostgresCannotStoreBeforeAskingIt(t *testing.T) {
	// A NUL character cannot be stored: a text column refuses the byte and a
	// jsonb column refuses \u0000. Attribute values come from tool input a model
	// wrote, so one can arrive, and the record then fails, which leaves the
	// action not allowed. It fails here, in plain words and with nothing sent,
	// so the error does not depend on the server's wording and a caller's
	// transaction is not left aborted.
	const nul = "a\x00b"
	tests := []struct {
		name   string
		change func(*policy.Record)
		want   string
	}{
		{"an attribute value", func(r *policy.Record) { r.Action.Attrs["note"] = nul }, "attribute"},
		{"an attribute name", func(r *policy.Record) { r.Action.Attrs[nul] = "x" }, "attribute"},
		{"a value nested in an object", func(r *policy.Record) {
			r.Action.Attrs["cc"] = map[string]any{"team": map[string]any{"name": nul}}
		}, "attribute"},
		{"a value in a list", func(r *policy.Record) { r.Action.Attrs["to"] = []any{"fine", nul} }, "attribute"},
		{"a value in a list of strings", func(r *policy.Record) { r.Action.Attrs["to"] = []string{nul} }, "attribute"},
		{"a value a type of the caller's writes", func(r *policy.Record) {
			r.Action.Attrs["v"] = struct{ S string }{S: nul}
		}, "attribute"},
		{"raw JSON the caller supplies", func(r *policy.Record) {
			r.Action.Attrs["raw"] = json.RawMessage(`{"a":"x\u0000y"}`)
		}, "attribute"},
		{"a NUL after a backslash", func(r *policy.Record) {
			// JSON writes the backslash as two characters, and the NUL after it
			// must still be found.
			r.Action.Attrs["v"] = "\\" + nul
		}, "attribute"},
		{"the kind", func(r *policy.Record) { r.Action.Kind = nul }, "kind"},
		{"the target", func(r *policy.Record) { r.Action.Target = nul }, "target"},
		{"the rule", func(r *policy.Record) { r.Decision.Rule = nul }, "rule"},
		{"a matched rule", func(r *policy.Record) { r.Decision.Matched = []string{"fine", nul} }, "matched"},
		{"an uncertain attribute", func(r *policy.Record) { r.Decision.Uncertain = []string{nul} }, "uncertain"},
		{"the version", func(r *policy.Record) { r.Version = nul }, "version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &fakeDB{tag: pgconn.NewCommandTag("INSERT 0 1")}
			err := policypg.New(db).Record(t.Context(), recordWith(tt.change))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "NUL")
			assert.Contains(t, err.Error(), tt.want)
			assert.Zero(t, db.execs, "nothing is sent")
		})
	}

	t.Run("text that only looks like a NUL escape is stored", func(t *testing.T) {
		for _, text := range []string{`\u0000`, `\\u0000`, `x\u0000y`, `\u00000`} {
			db := &fakeDB{tag: pgconn.NewCommandTag("INSERT 0 1")}
			rec := recordWith(func(r *policy.Record) { r.Action.Attrs["v"] = text })
			require.NoErrorf(t, policypg.New(db).Record(t.Context(), rec), "%q", text)
			assert.Equal(t, 1, db.execs)
		}
	})
}

func TestStore_RecordRefusesWhatCannotBeRecordedAtAll(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	tests := []struct {
		name   string
		change func(*policy.Record)
		want   string
	}{
		{"an attribute that is NaN", func(r *policy.Record) { r.Action.Attrs["n"] = math.NaN() }, "attributes"},
		{"an attribute that is infinite", func(r *policy.Record) { r.Action.Attrs["n"] = math.Inf(1) }, "attributes"},
		{"an attribute that is a function", func(r *policy.Record) { r.Action.Attrs["f"] = func() {} }, "attributes"},
		{"an attribute that is a channel", func(r *policy.Record) { r.Action.Attrs["c"] = make(chan int) }, "attributes"},
		{"an attribute that holds itself", func(r *policy.Record) { r.Action.Attrs["c"] = cyclic }, "attributes"},
		{"a number that is not a number", func(r *policy.Record) { r.Action.Attrs["n"] = json.Number("12abc") }, "attributes"},
		{"no effect, the zero Decision", func(r *policy.Record) { r.Decision = policy.Decision{} }, "effect"},
		{"an effect that is not one of the three", func(r *policy.Record) { r.Decision.Effect = "approve" }, "effect"},
		{"an effect in capitals", func(r *policy.Record) { r.Decision.Effect = "ALLOW" }, "effect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &fakeDB{tag: pgconn.NewCommandTag("INSERT 0 1")}
			err := policypg.New(db).Record(t.Context(), recordWith(tt.change))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Zero(t, db.execs, "nothing is sent")
		})
	}
}

func TestStore_RecordIsAnErrorUnlessOneRowWasWritten(t *testing.T) {
	boom := errors.New("connection refused")
	tests := []struct {
		name    string
		db      *fakeDB
		wantErr error
		wantMsg string
	}{
		{"the database fails", &fakeDB{err: boom}, boom, ""},
		{"no row written", &fakeDB{tag: pgconn.NewCommandTag("INSERT 0 0")}, nil, "0 rows"},
		{"two rows written", &fakeDB{tag: pgconn.NewCommandTag("INSERT 0 2")}, nil, "2 rows"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := policypg.New(tt.db).Record(t.Context(), sample("send", base))
			require.Error(t, err)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			}
			assert.Contains(t, err.Error(), tt.wantMsg)
			assert.Contains(t, err.Error(), "policy/pg")
		})
	}
}

func TestStore_ListSendsNothingForAnEffectItWillRefuse(t *testing.T) {
	db := &fakeDB{}
	_, err := policypg.New(db).List(t.Context(), policypg.Filter{Effect: "approve"})
	require.Error(t, err)
	assert.Zero(t, db.queries)
}

func TestStore_ListWrapsADatabaseError(t *testing.T) {
	boom := errors.New("connection refused")
	got, err := policypg.New(&fakeDB{err: boom}).List(t.Context(), policypg.Filter{})
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "policy/pg")
	assert.Nil(t, got)
}

func TestStore_ListIsAnErrorAndNotAShortListWhenTheReadFailsPartway(t *testing.T) {
	boom := errors.New("connection reset by peer")
	got, err := policypg.New(&fakeDB{rows: brokenRows{err: boom}}).List(t.Context(), policypg.Filter{})
	require.ErrorIs(t, err, boom)
	assert.Nil(t, got, "a list that stopped early must not be returned as the log")
}

func TestStore_ANulRefusedInGoLeavesACallersTransactionUsable(t *testing.T) {
	store, pool := openStore(t)
	ctx := t.Context()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	inTx := policypg.New(tx)

	err = inTx.Record(ctx, recordWith(func(r *policy.Record) { r.Action.Attrs["note"] = "a\x00b" }))
	require.Error(t, err)

	// The same transaction goes on to record, which it could not if the NUL had
	// reached Postgres: the failed statement would have aborted it.
	require.NoError(t, inTx.Record(ctx, sample("after", base)))
	require.NoError(t, tx.Commit(ctx))

	got, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"doc:after"}, targets(got))
}

func TestPostgres_RefusesANulItself(t *testing.T) {
	// This is what the check in Record stands in front of, and the reason it is
	// there. Pinned so that a Postgres release that changes it shows up here.
	_, pool := openStore(t)
	ctx := t.Context()
	const insert = `insert into ` + policypg.Table + ` (decided_at, kind, target, attrs, effect, rule, rule_index) values ($1, $2, $3, $4, 'allow', 'r', 0)`

	tests := []struct {
		name     string
		kind     string
		target   string
		attrs    string
		wantCode string
	}{
		{"a jsonb value", "k", "", `{"a":"x\u0000y"}`, "22P05"},
		{"a jsonb key", "k", "", `{"x\u0000":1}`, "22P05"},
		{"a text column", "k", "a\x00b", `{}`, "22021"},
		{"a lone surrogate, which Go's encoder never writes but raw JSON can carry", "k", "", `{"a":"\ud800"}`, "22P02"},
		{"a number past numeric's range", "k", "", `{"a":1e131072}`, "22003"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, insert, base, tt.kind, tt.target, tt.attrs)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			assert.Equal(t, tt.wantCode, pgErr.Code)
		})
	}
}

func TestStore_ANumberPostgresCannotHoldIsAnErrorAndLeavesNothing(t *testing.T) {
	// The package compares a number of any size up to 4096 bytes and an exponent
	// of 4096; jsonb holds one up to 131071 digits and no more. Past that the
	// record fails, and the action is not allowed.
	store, pool := openStore(t)
	ctx := t.Context()

	err := store.Record(ctx, recordWith(func(r *policy.Record) { r.Action.Attrs["n"] = json.Number("1e131072") }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "policy/pg")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+policypg.Table).Scan(&n))
	assert.Zero(t, n)
}

func TestStore_ARecordThatIsRefusedLeavesNothingInTheLog(t *testing.T) {
	store, pool := openStore(t)
	ctx := t.Context()

	for _, change := range []func(*policy.Record){
		func(r *policy.Record) { r.Action.Attrs["note"] = "a\x00b" },
		func(r *policy.Record) { r.Action.Attrs["n"] = math.NaN() },
		func(r *policy.Record) { r.Decision.Effect = "" },
		func(r *policy.Record) { r.Action.Target = "a\x00b" },
	} {
		require.Error(t, store.Record(ctx, recordWith(change)))
	}

	var n int
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+policypg.Table).Scan(&n))
	assert.Zero(t, n)

	// And the store is not wedged: the next record lands.
	require.NoError(t, store.Record(ctx, sample("after", base)))
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+policypg.Table).Scan(&n))
	assert.Equal(t, 1, n)
}
