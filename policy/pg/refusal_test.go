package pg_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
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
	if f.rows == nil && f.err == nil {
		return nil, errors.New("the fake has no rows to give")
	}
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
			assert.ErrorIs(t, err, policy.ErrUnrecordable, "no retry will make a NUL storable")
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
		{"a number with no digits", func(r *policy.Record) { r.Action.Attrs["n"] = json.Number("") }, "empty json.Number"},
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
			assert.ErrorIs(t, err, policy.ErrUnrecordable, "no retry will make it storable")
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
			assert.NotErrorIs(t, err, policy.ErrUnrecordable, "a database that failed, or did not write, is one a retry may find well")
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
	assert.ErrorIs(t, err, policy.ErrUnrecordable, "no retry will make the number smaller")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the server's own error is still in the chain")
	assert.Equal(t, "22003", pgErr.Code)

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

// An empty json.Number is not a number, and encoding/json writes one as 0: a
// zero nobody sent. Record refuses it wherever encoding/json would write it, and
// takes what encoding/json would not write.
func TestStore_RecordRefusesAnEmptyNumberWhereverItWouldBeWritten(t *testing.T) {
	var empty json.Number
	tests := []struct {
		name    string
		attrs   map[string]any
		refused bool
	}{
		{"in an attribute", map[string]any{"n": json.Number("")}, true},
		{"in a nested object", map[string]any{"a": map[string]any{"b": map[string]any{"n": json.Number("")}}}, true},
		{"in a list", map[string]any{"a": []any{"x", json.Number("")}}, true},
		{"in a list of numbers", map[string]any{"a": []json.Number{"1", ""}}, true},
		{"in an array", map[string]any{"a": [2]json.Number{"1", ""}}, true},
		{"behind a pointer", map[string]any{"a": &empty}, true},
		{"behind a pointer in a list", map[string]any{"a": []any{&empty}}, true},
		{"in a map of numbers", map[string]any{"a": map[string]json.Number{"k": ""}}, true},
		{"in a field of a struct", map[string]any{"a": numbered{}}, true},
		{"in a field of a struct behind a pointer", map[string]any{"a": &numbered{}}, true},
		{"in a field of an embedded struct", map[string]any{"a": embedsNumbered{}}, true},
		{"in a written field of a struct that skips others", map[string]any{"a": tagged{Shown: ""}}, true},
		{"deep in a mixture", map[string]any{"a": []any{map[string]any{"b": []any{&numbered{}}}}}, true},
		{"after numbers that are fine", map[string]any{"a": json.Number("1"), "b": []any{json.Number("2"), json.Number("")}}, true},

		{"zero, which was sent", map[string]any{"n": json.Number("0")}, false},
		{"a number with digits", map[string]any{"n": json.Number("1.50"), "m": []json.Number{"1", "-2e3"}}, false},
		{"a nil pointer to one", map[string]any{"n": (*json.Number)(nil)}, false},
		{"a string with nothing in it", map[string]any{"n": ""}, false},
		{"a list with nothing in it", map[string]any{"n": []any{}, "m": []json.Number{}, "o": map[string]json.Number{}}, false},
		{"bytes", map[string]any{"n": []byte("abc")}, false},
		{"a struct field encoding/json does not write", map[string]any{"a": tagged{Shown: "1", hidden: ""}}, false},
		{"a struct that writes itself", map[string]any{"a": selfWriting{}}, false},
		{"a struct that writes itself, behind a pointer", map[string]any{"a": &selfWriting{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &fakeDB{tag: pgconn.NewCommandTag("INSERT 0 1")}
			err := policypg.New(db).Record(t.Context(), recordWith(func(r *policy.Record) { r.Action.Attrs = tt.attrs }))
			if !tt.refused {
				require.NoError(t, err)
				assert.Equal(t, 1, db.execs)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "empty json.Number")
			assert.ErrorIs(t, err, policy.ErrUnrecordable)
			assert.Zero(t, db.execs, "nothing is sent")
		})
	}
}

type numbered struct{ N json.Number }

type embedsNumbered struct{ numbered }

// tagged has fields encoding/json writes and fields it does not, each of them
// empty.
type tagged struct {
	Shown   json.Number `json:"shown"`
	Skipped json.Number `json:"-"`
	Omitted json.Number `json:"omitted,omitempty"`
	Zeroed  json.Number `json:"zeroed,omitzero"`
	hidden  json.Number
}

// selfWriting writes itself, whatever it holds.
type selfWriting struct{ N json.Number }

func (selfWriting) MarshalJSON() ([]byte, error) { return []byte(`{"written":true}`), nil }

func TestStore_ARecordThatCanNeverBeStoredIsToldFromADatabaseThatFailed(t *testing.T) {
	// What the server says about the record's own content or size is something no
	// retry changes: a data exception (class 22) and a program limit (class 54).
	// What it says about itself or the moment is not.
	pgError := func(code string) error { return &pgconn.PgError{Code: code, Message: "m"} }
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"a number past numeric's range", pgError("22003"), true},
		{"an unsupported escape", pgError("22P05"), true},
		{"a byte sequence the encoding refuses", pgError("22021"), true},
		{"an invalid text representation", pgError("22P02"), true},
		{"a data exception that is none of those", pgError("22000"), true},
		{"a value nested too deeply", pgError("54001"), true},
		{"a row or an index entry too large", pgError("54000"), true},
		{"the same, wrapped", fmt.Errorf("pgx: %w", pgError("22003")), true},

		{"the server shutting down", pgError("57P01"), false},
		{"a connection that failed", pgError("08006"), false},
		{"too many connections", pgError("53300"), false},
		{"a serialization failure", pgError("40001"), false},
		{"a transaction that was aborted", pgError("25P02"), false},
		{"a deadlock", pgError("40P01"), false},
		{"a read-only server", pgError("25006"), false},
		{"a connection that was refused", errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"), false},
		{"a cancelled context", context.Canceled, false},
		{"a deadline", context.DeadlineExceeded, false},
		{"an unexpected end of file", io.ErrUnexpectedEOF, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := policypg.New(&fakeDB{err: tt.err}).Record(t.Context(), sample("send", base))
			require.Error(t, err)
			require.ErrorIs(t, err, tt.err, "the database's own error stays in the chain")
			assert.Equal(t, tt.want, errors.Is(err, policy.ErrUnrecordable))
			assert.Contains(t, err.Error(), "policy/pg")
		})
	}

	t.Run("with a real database", func(t *testing.T) {
		store, pool := openStore(t)
		ctx := t.Context()

		// Nested more deeply than the server's stack allows a value to be.
		var deep any = []any{}
		for range 20000 {
			deep = []any{deep}
		}
		// A name too large to be an entry in the index on the rule: it does not
		// compress, since it is a hash of each of its own pieces.
		var long strings.Builder
		for i := range 400 {
			fmt.Fprintf(&long, "%x", sha256.Sum256([]byte{byte(i), byte(i >> 8)}))
		}

		for _, tt := range []struct {
			name   string
			change func(*policy.Record)
			code   string
		}{
			{"a number past the server's range", func(r *policy.Record) { r.Action.Attrs["n"] = json.Number("1e131072") }, "22003"},
			{"a value nested too deeply", func(r *policy.Record) { r.Action.Attrs["deep"] = deep }, "54001"},
			{"a rule name too large for its index", func(r *policy.Record) { r.Decision.Rule = long.String() }, "54000"},
		} {
			err := store.Record(ctx, recordWith(tt.change))
			require.Errorf(t, err, "%s", tt.name)
			assert.ErrorIs(t, err, policy.ErrUnrecordable, tt.name)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr, tt.name)
			assert.Equal(t, tt.code, pgErr.Code, tt.name)
		}

		var n int
		require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+policypg.Table).Scan(&n))
		assert.Zero(t, n, "none of them is on the log")
	})

	t.Run("a database that is not there is not one", func(t *testing.T) {
		_, closed := openStore(t)
		closed.Close()
		store, _ := openStore(t)
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()

		for name, rec := range map[string]func() error{
			"a closed pool":         func() error { return policypg.New(closed).Record(t.Context(), sample("send", base)) },
			"nothing listening":     func() error { return policypg.New(deadPool(t)).Record(t.Context(), sample("send", base)) },
			"a cancelled context":   func() error { return store.Record(cancelled, sample("send", base)) },
			"a transaction aborted": func() error { return abortedTx(t).Record(t.Context(), sample("send", base)) },
		} {
			err := rec()
			require.Errorf(t, err, "%s", name)
			assert.NotErrorIs(t, err, policy.ErrUnrecordable, name)
		}
	})
}

// abortedTx is a store over a transaction that a failed statement has aborted,
// where every statement now fails with SQLSTATE 25P02.
func abortedTx(t *testing.T) *policypg.Store {
	t.Helper()
	_, pool := openStore(t)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	_, err = tx.Exec(t.Context(), `select 1/0`)
	require.Error(t, err)
	return policypg.New(tx)
}
