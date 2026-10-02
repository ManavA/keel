package pg_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/log"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
	"github.com/ManavA/keel/policy"
	policypg "github.com/ManavA/keel/policy/pg"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// base is the time every test counts from. Nothing here reads the clock: the
// store is handed a time and must give the same one back.
var base = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

var schemaCounter atomic.Int64

// openStore gives a test its own schema on the package's shared database, with
// the package's migrations applied through MigrationsFS, so the table holds
// only what the test wrote and a zero Filter can be asserted against it. The
// pool is returned for the checks that read the table directly.
func openStore(t *testing.T) (*policypg.Store, *pgxpool.Pool) {
	t.Helper()
	db := testdb.Shared(t)
	ctx := context.Background()

	schema := fmt.Sprintf("policy_pg_%d", schemaCounter.Add(1))
	admin, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL, MaxConns: 2})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	admin.Close()

	pool, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL + "&search_path=" + schema, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = migrate.Run(ctx, pool, migrate.Options{FS: policypg.MigrationsFS, Dir: "migrations"})
	require.NoError(t, err)
	return policypg.New(pool), pool
}

// sample is a record of the given kind, decided at the given time, with
// everything a record carries set.
func sample(kind string, at time.Time) policy.Record {
	return policy.Record{
		At:     at,
		Action: policy.Action{Kind: kind, Target: "doc:" + kind, Attrs: map[string]any{"size": json.Number("3")}},
		Decision: policy.Decision{
			Effect: policy.Ask, Rule: "Sending needs a person", Index: 1,
			Matched: []string{"Sending needs a person"},
		},
		Version: "v1",
	}
}

// requireRecords compares what List returned with what was written. The times
// are compared as instants and must be in UTC, every record must carry an id,
// and the rest is compared whole.
func requireRecords(t *testing.T, want, got []policy.Record) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		assert.True(t, want[i].At.Equal(got[i].At), "record %d: decided at %v, listed at %v", i, want[i].At, got[i].At)
		assert.Equal(t, time.UTC, got[i].At.Location(), "record %d must be read back in UTC", i)
		assert.Positive(t, got[i].ID, "record %d must carry its id", i)
		w, g := want[i], got[i]
		w.At, g.At = time.Time{}, time.Time{}
		w.ID, g.ID = 0, 0
		assert.Equal(t, w, g, "record %d", i)
	}
}

// targets are the identifying part of each record, in the order listed.
func targets(recs []policy.Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Action.Target)
	}
	return out
}

func TestStore_ARecordIsListedBackWithEverythingItCarries(t *testing.T) {
	tests := []struct {
		name  string
		attrs map[string]any
	}{
		{"a string", map[string]any{"to": "ap@example.com"}},
		{"a boolean", map[string]any{"external": true, "sensitive": false}},
		{"null", map[string]any{"approver": nil}},
		{"a small integer", map[string]any{"count": json.Number("3")}},
		{"a large integer", map[string]any{"amount": json.Number("1180591620717411303424")}},
		{"a long fraction", map[string]any{"ratio": json.Number("0.30000000000000004")}},
		{"a list", map[string]any{"to": []any{"a@example.com", json.Number("2"), true, nil}}},
		{"an empty list", map[string]any{"to": []any{}}},
		{"a nested object", map[string]any{"cc": map[string]any{"team": map[string]any{"name": "ap", "size": json.Number("12")}}}},
		{"an empty object", map[string]any{"opts": map[string]any{}}},
		{"an empty string and a key that looks like a number", map[string]any{"": "", "1": "one"}},
		{"text that is not ASCII", map[string]any{"note": "café ☃ \U0001F600 <&>"}},
		{"text that looks like a NUL escape", map[string]any{"note": `\u0000 is written as a backslash and a u`}},
		{"every type together", map[string]any{
			"s": "text", "b": true, "n": nil, "i": json.Number("7"),
			"big": json.Number("1180591620717411303424"), "frac": json.Number("0.30000000000000004"),
			"list":   []any{map[string]any{"deep": []any{json.Number("1")}}},
			"nested": map[string]any{"a": map[string]any{"b": "c"}},
		}},
	}
	store, _ := openStore(t)
	ctx := t.Context()

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The time has a sub-microsecond part the column cannot keep, and a
			// zone other than UTC, so the comparison is made where the store
			// promises to be exact.
			zone := time.FixedZone("east", 5*3600+1800)
			want := policy.Record{
				At:     base.Add(time.Duration(i) * time.Second).Add(123456*time.Microsecond + 300).In(zone),
				Action: policy.Action{Kind: tt.name, Target: "email:ap@example.com", Attrs: tt.attrs},
				Decision: policy.Decision{
					Effect: policy.Ask, Rule: "Payment above the $200 limit", Index: 2,
					Matched:   []string{"Default for pay actions", "Payment above the $200 limit"},
					Uncertain: []string{"amount"},
				},
				Version: "2026-10-02",
			}
			require.NoError(t, store.Record(ctx, want))

			got, err := store.List(ctx, policypg.Filter{Kind: tt.name})
			require.NoError(t, err)
			want.At = want.At.UTC().Truncate(time.Microsecond)
			requireRecords(t, []policy.Record{want}, got)
		})
	}
}

func TestStore_AttributesReadBackAsJSONGivesThem(t *testing.T) {
	// Attributes are stored as JSON, so what comes back is what encoding/json
	// decodes into an any, with numbers as json.Number: nothing here is a Go
	// type the writer used.
	store, _ := openStore(t)
	ctx := t.Context()
	when := time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

	in := map[string]any{
		"int":     42,
		"int64":   int64(-7),
		"float":   0.5,
		"strings": []string{"a", "b"},
		"time":    when,
		"bytes":   []byte("hi"),
		"struct":  struct{ X int }{X: 1},
		"pointer": new(string),
	}
	require.NoError(t, store.Record(ctx, policy.Record{At: base, Action: policy.Action{Kind: "k", Attrs: in}, Decision: policy.Decision{Effect: policy.Allow}}))

	got, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, map[string]any{
		"int":     json.Number("42"),
		"int64":   json.Number("-7"),
		"float":   json.Number("0.5"),
		"strings": []any{"a", "b"},
		"time":    "2026-10-02T09:30:00Z",
		"bytes":   "aGk=",
		"struct":  map[string]any{"X": json.Number("1")},
		"pointer": "",
	}, got[0].Action.Attrs)
}

func TestStore_NumbersAreNotRounded(t *testing.T) {
	// want is the text read back. Where it differs from the text written, the
	// number is still the same decimal: jsonb stores a number as a numeric, which
	// keeps every digit and the written scale, and prints it without an exponent.
	tests := []struct {
		name  string
		write string
		want  string
	}{
		{"2^70", "1180591620717411303424", "1180591620717411303424"},
		{"minus 2^70", "-1180591620717411303424", "-1180591620717411303424"},
		{"a double's neighbour of 0.3", "0.30000000000000004", "0.30000000000000004"},
		{"2^53 plus 1", "9007199254740993", "9007199254740993"},
		{"the integer a float64 1e23 holds", "99999999999999991611392", "99999999999999991611392"},
		{"thirty digits either side", "123456789012345678901234567890.123456789012345678901234567890", "123456789012345678901234567890.123456789012345678901234567890"},
		{"a trailing zero is kept", "1.50", "1.50"},
		{"a tenth", "0.1", "0.1"},
		{"an exponent is written out", "1e23", "100000000000000000000000"},
		{"a signed exponent", "1E+2", "100"},
		{"a small exponent", "1e-7", "0.0000001"},
		{"minus zero", "-0", "0"},
	}
	store, pool := openStore(t)
	ctx := t.Context()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := policy.Record{
				At:       base,
				Action:   policy.Action{Kind: tt.name, Attrs: map[string]any{"n": json.Number(tt.write)}},
				Decision: policy.Decision{Effect: policy.Allow},
			}
			require.NoError(t, store.Record(ctx, rec))

			// The column itself keeps the digits: this is the text Postgres holds,
			// before anything reads it into Go.
			var stored string
			require.NoError(t, pool.QueryRow(ctx, `select (attrs -> 'n')::text from `+policypg.Table+` where kind = $1`, tt.name).Scan(&stored))
			assert.Equal(t, tt.want, stored, "what jsonb holds")

			got, err := store.List(ctx, policypg.Filter{Kind: tt.name})
			require.NoError(t, err)
			require.Len(t, got, 1)
			n, ok := got[0].Action.Attrs["n"].(json.Number)
			require.Truef(t, ok, "a number must read back as a json.Number, got %T", got[0].Action.Attrs["n"])
			assert.Equal(t, tt.want, n.String())

			written, _ := new(big.Rat).SetString(tt.write)
			read, _ := new(big.Rat).SetString(n.String())
			require.NotNil(t, written)
			assert.Zero(t, written.Cmp(read), "%s and %s must be one decimal", tt.write, n)
		})
	}

	t.Run("a float64 is stored as the shortest decimal that gives it back", func(t *testing.T) {
		// 2^70 as a float64 has two readings (the exact integer and its shortest
		// decimal), and encoding/json writes the second. A number that has to be
		// exact reaches the store as a json.Number.
		require.NoError(t, store.Record(ctx, policy.Record{At: base, Action: policy.Action{Kind: "float", Attrs: map[string]any{"n": math.Pow(2, 70), "tenth": 0.1}}, Decision: policy.Decision{Effect: policy.Allow}}))
		got, err := store.List(ctx, policypg.Filter{Kind: "float"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, json.Number("1180591620717411300000"), got[0].Action.Attrs["n"])
		assert.Equal(t, json.Number("0.1"), got[0].Action.Attrs["tenth"])
	})
}

func TestStore_NilCollectionsAreStoredAsEmptyAndReadBackEmpty(t *testing.T) {
	tests := []struct {
		name      string
		attrs     map[string]any
		matched   []string
		uncertain []string
	}{
		{"all nil", nil, nil, nil},
		{"all empty", map[string]any{}, []string{}, []string{}},
		{"nil attributes only", nil, []string{"a"}, []string{"b"}},
		{"nil lists only", map[string]any{"x": json.Number("1")}, nil, nil},
	}
	store, pool := openStore(t)
	ctx := t.Context()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := policy.Record{
				At:       base,
				Action:   policy.Action{Kind: tt.name, Attrs: tt.attrs},
				Decision: policy.Decision{Effect: policy.Block, Rule: policy.RuleDefault, Index: -1, Matched: tt.matched, Uncertain: tt.uncertain},
			}
			require.NoError(t, store.Record(ctx, rec))

			// Never JSON null, which a reader of the table would have to handle.
			var attrs, matched, uncertain string
			require.NoError(t, pool.QueryRow(ctx,
				`select jsonb_typeof(attrs), jsonb_typeof(matched), jsonb_typeof(uncertain) from `+policypg.Table+` where kind = $1`,
				tt.name).Scan(&attrs, &matched, &uncertain))
			assert.Equal(t, []string{"object", "array", "array"}, []string{attrs, matched, uncertain})

			got, err := store.List(ctx, policypg.Filter{Kind: tt.name})
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.NotNil(t, got[0].Action.Attrs)
			assert.NotNil(t, got[0].Decision.Matched)
			assert.NotNil(t, got[0].Decision.Uncertain)
			assert.Len(t, got[0].Action.Attrs, len(tt.attrs))
			assert.Len(t, got[0].Decision.Matched, len(tt.matched))
			assert.Len(t, got[0].Decision.Uncertain, len(tt.uncertain))
			assert.Equal(t, -1, got[0].Decision.Index)
		})
	}
}

func TestStore_AtIsStoredAndReadInUTCToTheMicrosecond(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"already UTC", base, base},
		{"east of UTC", time.Date(2026, 10, 2, 14, 30, 0, 0, time.FixedZone("east", 5*3600+1800)), base},
		{"west of UTC", time.Date(2026, 10, 2, 1, 0, 0, 0, time.FixedZone("west", -8*3600)), base},
		{"nanoseconds below a microsecond are dropped", base.Add(123456789 * time.Nanosecond), base.Add(123456 * time.Microsecond)},
		{"just under a microsecond is dropped, not rounded up", base.Add(999 * time.Nanosecond), base},
		{"the zero time", time.Time{}, time.Time{}},
		{"the last microsecond of year 9999", time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)},
		{"the last nanosecond of year 9999, which is dropped to the microsecond", time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)},
	}
	store, pool := openStore(t)
	ctx := t.Context()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := sample(tt.name, tt.at)
			require.NoError(t, store.Record(ctx, rec))

			// What the column holds is the instant, whatever zone the reader's
			// session is in.
			var stored time.Time
			require.NoError(t, pool.QueryRow(ctx, `select decided_at from `+policypg.Table+` where kind = $1`, tt.name).Scan(&stored))
			assert.True(t, tt.want.Equal(stored), "the column holds %v, want %v", stored.UTC(), tt.want)

			got, err := store.List(ctx, policypg.Filter{Kind: tt.name})
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.True(t, tt.want.Equal(got[0].At), "listed at %v, want %v", got[0].At, tt.want)
			assert.Equal(t, time.UTC, got[0].At.Location())
		})
	}
}

func TestStore_ListIsNewestFirstWhateverTheOrderOfInsertion(t *testing.T) {
	store, _ := openStore(t)
	ctx := t.Context()

	// Decided times arrive out of order, as they do from two processes whose
	// clocks differ: newest means decided last, not inserted last.
	for _, n := range []int{3, 1, 4, 0, 2} {
		rec := sample("k", base.Add(time.Duration(n)*time.Second))
		rec.Action.Target = fmt.Sprintf("n:%d", n)
		require.NoError(t, store.Record(ctx, rec))
	}

	got, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"n:4", "n:3", "n:2", "n:1", "n:0"}, targets(got))
}

func TestStore_RecordsDecidedAtTheSameTimeListLatestInsertedFirstAndStayPut(t *testing.T) {
	store, pool := openStore(t)
	ctx := t.Context()

	// Five decisions in one instant, which a fixed clock or a busy second
	// produces. Without a tie-break their order is the planner's to choose.
	for n := range 5 {
		rec := sample("k", base)
		rec.Action.Target = fmt.Sprintf("n:%d", n)
		require.NoError(t, store.Record(ctx, rec))
	}

	// The same table under the plan that does not read the index, which holds
	// ties in id order already: a sequential scan hands them to the sort in the
	// order they were written, the reverse of the order wanted.
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	for _, setting := range []string{"enable_indexscan", "enable_indexonlyscan", "enable_bitmapscan"} {
		_, err := conn.Exec(ctx, "set "+setting+" = off")
		require.NoError(t, err)
	}
	var plan string
	require.NoError(t, conn.QueryRow(ctx, `explain select id from `+policypg.Table+` order by decided_at desc`).Scan(&plan))
	require.Contains(t, plan, "Sort", "the table must be read through a sort for this to test the tie-break")

	for _, readers := range []struct {
		name  string
		store *policypg.Store
	}{
		{"with the index", store},
		{"with a sequential scan and a sort", policypg.New(conn)},
	} {
		for _, tt := range []struct {
			name  string
			limit int
			want  []string
		}{
			{"all", 0, []string{"n:4", "n:3", "n:2", "n:1", "n:0"}},
			{"the first two, so a limit cuts the same end each time", 2, []string{"n:4", "n:3"}},
			{"one", 1, []string{"n:4"}},
		} {
			t.Run(readers.name+", "+tt.name, func(t *testing.T) {
				for range 3 {
					got, err := readers.store.List(ctx, policypg.Filter{Limit: tt.limit})
					require.NoError(t, err)
					assert.Equal(t, tt.want, targets(got))
				}
			})
		}
	}
}

func TestStore_ListFilters(t *testing.T) {
	store, _ := openStore(t)
	ctx := t.Context()

	type seed struct {
		effect policy.Effect
		rule   string
		kind   string
	}
	const (
		reading  = "Reading is allowed"
		sending  = "Sending needs a person"
		payment  = "Payment above the limit"
		deleting = "Deleting is never allowed"
	)
	seeds := []seed{
		0: {policy.Allow, reading, "read"},
		1: {policy.Ask, sending, "send"},
		2: {policy.Ask, payment, "pay"},
		3: {policy.Block, deleting, "delete"},
		4: {policy.Allow, reading, "read"},
		5: {policy.Ask, payment, "pay"},
		6: {policy.Block, policy.RuleDefault, "send"},
	}
	at := func(n int) time.Time { return base.Add(time.Duration(n) * time.Minute) }
	for n, s := range seeds {
		require.NoError(t, store.Record(ctx, policy.Record{
			At:       at(n),
			Action:   policy.Action{Kind: s.kind, Target: fmt.Sprintf("n:%d", n)},
			Decision: policy.Decision{Effect: s.effect, Rule: s.rule},
		}))
	}

	tests := []struct {
		name   string
		filter policypg.Filter
		want   []string
	}{
		{"no filter lists everything", policypg.Filter{}, []string{"n:6", "n:5", "n:4", "n:3", "n:2", "n:1", "n:0"}},
		{"effect ask", policypg.Filter{Effect: policy.Ask}, []string{"n:5", "n:2", "n:1"}},
		{"effect block", policypg.Filter{Effect: policy.Block}, []string{"n:6", "n:3"}},
		{"effect allow", policypg.Filter{Effect: policy.Allow}, []string{"n:4", "n:0"}},
		{"rule", policypg.Filter{Rule: payment}, []string{"n:5", "n:2"}},
		{"the rule that means nothing matched", policypg.Filter{Rule: policy.RuleDefault}, []string{"n:6"}},
		{"kind", policypg.Filter{Kind: "send"}, []string{"n:6", "n:1"}},
		{"since, which includes its own instant", policypg.Filter{Since: at(4)}, []string{"n:6", "n:5", "n:4"}},
		{"since, one microsecond later", policypg.Filter{Since: at(4).Add(time.Microsecond)}, []string{"n:6", "n:5"}},
		{"since, given in another zone", policypg.Filter{Since: at(4).In(time.FixedZone("west", -7*3600))}, []string{"n:6", "n:5", "n:4"}},
		{"since, after everything", policypg.Filter{Since: at(7)}, []string{}},
		{"effect and kind", policypg.Filter{Effect: policy.Ask, Kind: "pay"}, []string{"n:5", "n:2"}},
		{"effect and rule", policypg.Filter{Effect: policy.Block, Rule: policy.RuleDefault}, []string{"n:6"}},
		{"rule and kind", policypg.Filter{Rule: sending, Kind: "send"}, []string{"n:1"}},
		{"all four", policypg.Filter{Effect: policy.Ask, Rule: payment, Kind: "pay", Since: at(3)}, []string{"n:5"}},
		{"all four, the last excluding", policypg.Filter{Effect: policy.Ask, Rule: payment, Kind: "pay", Since: at(6)}, []string{}},
		{"filters that agree on nothing", policypg.Filter{Effect: policy.Ask, Kind: "delete"}, []string{}},
		{"a rule nobody wrote", policypg.Filter{Rule: "No such rule"}, []string{}},
		{"a filter and a limit", policypg.Filter{Effect: policy.Ask, Limit: 2}, []string{"n:5", "n:2"}},
		{"a limit alone", policypg.Filter{Limit: 3}, []string{"n:6", "n:5", "n:4"}},
		{"filters match exactly, and so case-sensitively", policypg.Filter{Kind: "PAY"}, []string{}},
		{"a rule is matched case-sensitively too", policypg.Filter{Rule: "payment above the limit"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.List(ctx, tt.filter)
			require.NoError(t, err)
			assert.NotNil(t, got, "no records is an empty list, not nil")
			assert.Equal(t, tt.want, targets(got))
		})
	}
}

func TestStore_ListRefusesAnEffectThatIsNotOneOfTheThree(t *testing.T) {
	// "approve" is what the reference calls Ask, and a log read for it would
	// come back empty, which reads as "nothing was asked".
	store, _ := openStore(t)
	require.NoError(t, store.Record(t.Context(), sample("k", base)))

	for _, effect := range []policy.Effect{"approve", "ALLOW", "deny", " ask"} {
		got, err := store.List(t.Context(), policypg.Filter{Effect: effect})
		require.Errorf(t, err, "effect %q", effect)
		assert.Contains(t, err.Error(), "allow, ask or block")
		assert.Nil(t, got)
	}
}

func TestStore_ListLimit(t *testing.T) {
	store, pool := openStore(t)
	ctx := t.Context()

	// 1005 rows, written in one statement: this is about how many List returns,
	// and which end it cuts, not about Record.
	_, err := pool.Exec(ctx, `
		insert into `+policypg.Table+` (decided_at, kind, target, effect, rule, rule_index)
		select timestamptz '2026-10-02 09:00:00+00' + n * interval '1 second', 'bulk', n::text, 'allow', 'r', 0
		from generate_series(1, 1005) as n`)
	require.NoError(t, err)

	tests := []struct {
		name     string
		limit    int
		wantRows int
	}{
		{"zero is the default of 100", 0, 100},
		{"a negative limit is the default of 100", -5, 100},
		{"a limit is honoured", 7, 7},
		{"one", 1, 1},
		{"the most there is room for", 1000, 1000},
		{"one past the most is the most", 1001, 1000},
		{"a million rows is the most", 1_000_000, 1000},
		{"the largest int is the most", math.MaxInt, 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.List(ctx, policypg.Filter{Limit: tt.limit})
			require.NoError(t, err)
			require.Len(t, got, tt.wantRows)
			// The newest rows are the ones kept.
			assert.Equal(t, "1005", got[0].Action.Target)
			assert.Equal(t, fmt.Sprint(1005-tt.wantRows+1), got[len(got)-1].Action.Target)
		})
	}
}

func TestStore_TwentyConcurrentRecordsAllLand(t *testing.T) {
	store, pool := openStore(t)
	ctx := t.Context()
	const n = 20

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := sample(fmt.Sprintf("kind-%02d", i), base)
			rec.Action.Target = fmt.Sprintf("n:%02d", i)
			errs[i] = store.Record(ctx, rec)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoErrorf(t, err, "record %d", i)
	}

	got, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	require.Len(t, got, n)
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.Action.Target] = true
	}
	assert.Len(t, seen, n, "every record is there once")

	var ids int
	require.NoError(t, pool.QueryRow(ctx, `select count(distinct id) from `+policypg.Table).Scan(&ids))
	assert.Equal(t, n, ids)
}

func TestStore_ListNamesAnUnreadableRow(t *testing.T) {
	store, pool := openStore(t)
	ctx := t.Context()

	t.Run("attributes that are not an object", func(t *testing.T) {
		_, err := pool.Exec(ctx, `insert into `+policypg.Table+` (decided_at, kind, attrs, effect, rule, rule_index) values ($1, 'bad', '[1]', 'allow', 'r', 0)`, base)
		require.NoError(t, err)
		got, err := store.List(ctx, policypg.Filter{Kind: "bad"})
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "attrs")
		assert.Contains(t, err.Error(), "row ")
	})

	t.Run("JSON null where a list or object belongs reads as empty", func(t *testing.T) {
		_, err := pool.Exec(ctx, `insert into `+policypg.Table+` (decided_at, kind, attrs, effect, rule, rule_index, matched, uncertain) values ($1, 'null', 'null', 'allow', 'r', 0, 'null', 'null')`, base)
		require.NoError(t, err)
		got, err := store.List(ctx, policypg.Filter{Kind: "null"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, map[string]any{}, got[0].Action.Attrs)
		assert.Equal(t, []string{}, got[0].Decision.Matched)
		assert.Equal(t, []string{}, got[0].Decision.Uncertain)
	})
}

func TestStore_HasAnInsertAndAReadAndNothingThatChangesTheLog(t *testing.T) {
	typ := reflect.TypeFor[*policypg.Store]()
	var methods []string
	for i := range typ.NumMethod() {
		methods = append(methods, typ.Method(i).Name)
	}
	slices.Sort(methods)
	assert.Equal(t, []string{"List", "Record"}, methods)
}

func TestStore_InvalidUTF8IsReplacedInJSONAndRefusedInText(t *testing.T) {
	store, pool := openStore(t)
	ctx := t.Context()

	// In an attribute value, an attribute name, a matched rule and an uncertain
	// attribute, encoding/json writes U+FFFD for each bad byte, so that is what
	// the log holds.
	rec := policy.Record{
		At:     base,
		Action: policy.Action{Kind: "k", Attrs: map[string]any{"note": "a\xffb", "x\xfey": "v"}},
		Decision: policy.Decision{
			Effect: policy.Ask, Rule: "r", Matched: []string{"m\xff"}, Uncertain: []string{"u\xff"},
		},
	}
	require.NoError(t, store.Record(ctx, rec))
	got, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, map[string]any{"note": "a\ufffdb", "x\ufffdy": "v"}, got[0].Action.Attrs)
	assert.Equal(t, []string{"m\ufffd"}, got[0].Decision.Matched)
	assert.Equal(t, []string{"u\ufffd"}, got[0].Decision.Uncertain)

	// In a text column the server refuses it, and no retry will change that.
	for name, change := range map[string]func(*policy.Record){
		"the kind":    func(r *policy.Record) { r.Action.Kind = "a\xffb" },
		"the target":  func(r *policy.Record) { r.Action.Target = "a\xffb" },
		"the rule":    func(r *policy.Record) { r.Decision.Rule = "a\xffb" },
		"the version": func(r *policy.Record) { r.Version = "a\xffb" },
	} {
		err := store.Record(ctx, recordWith(change))
		require.Errorf(t, err, "%s", name)
		assert.ErrorIs(t, err, policy.ErrUnrecordable, name)
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+policypg.Table).Scan(&n))
	assert.Equal(t, 1, n, "only the first record is on the log")
}

func TestStore_AnIndexIsKeptWhateverTheColumnHolds(t *testing.T) {
	store, _ := openStore(t)
	ctx := t.Context()
	indexes := []int{math.MinInt32, -1, 0, 1, math.MaxInt32}
	for n, index := range indexes {
		rec := sample("k", base.Add(time.Duration(n)*time.Second))
		rec.Decision.Index = index
		require.NoError(t, store.Record(ctx, rec))
	}
	got, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	require.Len(t, got, len(indexes))
	for n, rec := range got {
		assert.Equal(t, indexes[len(indexes)-1-n], rec.Decision.Index)
	}
}

// jsonb keeps a number as a numeric, which holds at most 131072 digits before
// the point and 16383 after it; a number written with an exponent is held as it
// expands. The package comment says so, and past either limit the record fails
// as one that can never be stored.
func TestStore_JSONBsNumberLimitsAreWhatTheDocumentationSays(t *testing.T) {
	tests := []struct {
		name      string
		number    string
		ok        bool
		wantDigit int // the digits read back, when the spelling changes
	}{
		{"131072 digits before the point", strings.Repeat("9", 131072), true, 0},
		{"131073 digits before the point", strings.Repeat("9", 131073), false, 0},
		{"16383 digits after the point", "0." + strings.Repeat("1", 16383), true, 0},
		{"16384 digits after the point", "0." + strings.Repeat("1", 16384), false, 0},
		{"an exponent that fills the limit before the point", "1e131071", true, 131072},
		{"an exponent past it", "1e131072", false, 0},
		{"an exponent that fills the limit after the point", "1e-16383", true, 16385},
		{"an exponent past it, after the point", "1e-16384", false, 0},
	}
	store, pool := openStore(t)
	ctx := t.Context()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := store.Record(ctx, policy.Record{
				At:       base,
				Action:   policy.Action{Kind: tt.name, Attrs: map[string]any{"n": json.Number(tt.number)}},
				Decision: policy.Decision{Effect: policy.Allow},
			})
			if !tt.ok {
				require.Error(t, err)
				assert.ErrorIs(t, err, policy.ErrUnrecordable)
				var pgErr *pgconn.PgError
				require.ErrorAs(t, err, &pgErr)
				assert.Equal(t, "22003", pgErr.Code)
				return
			}
			require.NoError(t, err)
			got, err := store.List(ctx, policypg.Filter{Kind: tt.name})
			require.NoError(t, err)
			require.Len(t, got, 1)
			n, ok := got[0].Action.Attrs["n"].(json.Number)
			require.True(t, ok)
			if tt.wantDigit > 0 {
				assert.Len(t, n.String(), tt.wantDigit)
			} else {
				assert.Equal(t, tt.number, n.String())
			}
		})
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+policypg.Table).Scan(&n))
	assert.Equal(t, 4, n, "the four that fit")
}
