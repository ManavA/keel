package pg_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/policy"
	policypg "github.com/ManavA/keel/policy/pg"
)

// The log seeded for the paging tests: twelve decisions, recorded in a
// scrambled order so that an id says nothing about the time, with several that
// share a time, and with one such group (the four at minute 3) larger than a
// page.
type seeded struct {
	target string
	minute int
	effect policy.Effect
	rule   string
	kind   string
}

var pagingSeeds = []seeded{
	{"n:7", 3, policy.Ask, "Sending needs a person", "send"},
	{"n:0", 0, policy.Allow, "Reading is allowed", "read"},
	{"n:10", 4, policy.Block, "Deleting is never allowed", "delete"},
	{"n:3", 1, policy.Block, "Deleting is never allowed", "delete"},
	{"n:5", 3, policy.Ask, "Payment above the limit", "pay"},
	{"n:11", 0, policy.Ask, "Sending needs a person", "send"},
	{"n:1", 1, policy.Ask, "Sending needs a person", "send"},
	{"n:8", 3, policy.Allow, "Reading is allowed", "read"},
	{"n:2", 1, policy.Ask, "Payment above the limit", "pay"},
	{"n:9", 4, policy.Ask, "Payment above the limit", "pay"},
	{"n:4", 2, policy.Allow, "Reading is allowed", "read"},
	{"n:6", 3, policy.Block, policy.RuleDefault, "send"},
}

// pagingOrder is that log newest first, worked out by hand: by minute, latest
// first, and within a minute the one recorded later first. It is the oracle the
// paging tests compare with, so that they do not trust List to say what List
// should say.
var pagingOrder = []string{
	"n:9", "n:10", // minute 4
	"n:6", "n:8", "n:5", "n:7", // minute 3
	"n:4",               // minute 2
	"n:2", "n:1", "n:3", // minute 1
	"n:11", "n:0", // minute 0
}

func seedPagingLog(t *testing.T) *policypg.Store {
	t.Helper()
	store, _ := openStore(t)
	for _, s := range pagingSeeds {
		require.NoError(t, store.Record(t.Context(), policy.Record{
			At:       base.Add(time.Duration(s.minute) * time.Minute),
			Action:   policy.Action{Kind: s.kind, Target: s.target},
			Decision: policy.Decision{Effect: s.effect, Rule: s.rule},
		}))
	}
	return store
}

// after is the cursor a caller builds from a listed record: its time and id.
func after(rec policy.Record) *policypg.Cursor {
	return &policypg.Cursor{At: rec.At, ID: rec.ID}
}

// page reads every page of a listing, newest first, with one limit, building
// each cursor from the last record of the page before. It stops at the first
// empty page, or fails if there are more pages than records could fill.
func page(t *testing.T, store *policypg.Store, f policypg.Filter, pageSize int) (all []policy.Record, sizes []int) {
	t.Helper()
	f.Limit = pageSize
	for range 1000 {
		got, err := store.List(t.Context(), f)
		require.NoError(t, err)
		require.NotNil(t, got)
		sizes = append(sizes, len(got))
		if len(got) == 0 {
			return all, sizes
		}
		all = append(all, got...)
		f.Before = after(got[len(got)-1])
	}
	require.Fail(t, "the listing did not end")
	return nil, nil
}

func TestStore_ListedRecordsCarryTheirIDs(t *testing.T) {
	store := seedPagingLog(t)

	got, err := store.List(t.Context(), policypg.Filter{})
	require.NoError(t, err)
	require.Len(t, got, len(pagingSeeds))
	assert.Equal(t, pagingOrder, targets(got))

	// The id is the order of recording, which is what breaks a tie.
	idOf := map[string]int64{}
	for _, rec := range got {
		assert.Positive(t, rec.ID, rec.Action.Target)
		idOf[rec.Action.Target] = rec.ID
	}
	assert.Len(t, idOf, len(pagingSeeds), "no two records share an id")
	for i := 1; i < len(pagingSeeds); i++ {
		assert.Greater(t, idOf[pagingSeeds[i].target], idOf[pagingSeeds[i-1].target], "recorded in this order")
	}
}

func TestStore_RecordIgnoresTheIDItIsHanded(t *testing.T) {
	// The column is GENERATED ALWAYS, so an id written would be an error; a
	// record that was listed once and is recorded again gets a new number.
	store, _ := openStore(t)
	ctx := t.Context()

	rec := sample("send", base)
	rec.ID = 9999
	require.NoError(t, store.Record(ctx, rec))
	require.NoError(t, store.Record(ctx, rec))

	got, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, int64(2), got[0].ID)
	assert.Equal(t, int64(1), got[1].ID)
}

func TestStore_ListPagesBackThroughEveryRecordOnce(t *testing.T) {
	store := seedPagingLog(t)

	t.Run("three pages of four, with a time shared across each boundary", func(t *testing.T) {
		all, sizes := page(t, store, policypg.Filter{}, 4)
		assert.Equal(t, []int{4, 4, 4, 0}, sizes, "three full pages and then the end")
		assert.Equal(t, pagingOrder, targets(all))
	})

	t.Run("each page holds what the cursor leaves", func(t *testing.T) {
		first, err := store.List(t.Context(), policypg.Filter{Limit: 4})
		require.NoError(t, err)
		assert.Equal(t, []string{"n:9", "n:10", "n:6", "n:8"}, targets(first))

		second, err := store.List(t.Context(), policypg.Filter{Limit: 4, Before: after(first[3])})
		require.NoError(t, err)
		assert.Equal(t, []string{"n:5", "n:7", "n:4", "n:2"}, targets(second), "the minute-3 group is split between the pages, each record once")

		third, err := store.List(t.Context(), policypg.Filter{Limit: 4, Before: after(second[3])})
		require.NoError(t, err)
		assert.Equal(t, []string{"n:1", "n:3", "n:11", "n:0"}, targets(third))
	})

	// Every page size, so that each boundary falls in each place: before a
	// time, inside a group of records that share one, and after the last.
	for pageSize := 1; pageSize <= len(pagingSeeds)+1; pageSize++ {
		t.Run(fmt.Sprintf("a page of %d", pageSize), func(t *testing.T) {
			all, sizes := page(t, store, policypg.Filter{}, pageSize)
			assert.Equal(t, pagingOrder, targets(all), "every record once, in order")
			assert.Zero(t, sizes[len(sizes)-1], "the listing ends on an empty page")
			for _, n := range sizes[:len(sizes)-1] {
				assert.LessOrEqual(t, n, pageSize)
			}
		})
	}
}

func TestStore_ACursorIsAPositionAndNotARecord(t *testing.T) {
	store := seedPagingLog(t)
	ctx := t.Context()
	full, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	oldest, newest := full[len(full)-1], full[0]

	tests := []struct {
		name   string
		before policypg.Cursor
		want   []string
	}{
		{"the oldest record, which has nothing older", *after(oldest), []string{}},
		{"a time before any record", policypg.Cursor{At: base.Add(-time.Hour), ID: 1_000_000}, []string{}},
		{"the oldest record's time with the next id, which leaves that record", policypg.Cursor{At: oldest.At, ID: oldest.ID + 1}, []string{"n:0"}},
		{"the newest record, which leaves the rest", *after(newest), pagingOrder[1:]},
		{"a time after every record, which leaves all of them", policypg.Cursor{At: base.Add(24 * time.Hour), ID: 1}, pagingOrder},
		{"a time in the middle with the lowest id, which leaves what is older than that time", policypg.Cursor{At: base.Add(3 * time.Minute), ID: 1}, pagingOrder[6:]},
		{"the same time with an id above every record's, which leaves that time too", policypg.Cursor{At: base.Add(3 * time.Minute), ID: 9999}, pagingOrder[2:]},
		{"in another zone, as the same instant", policypg.Cursor{At: newest.At.In(time.FixedZone("west", -7*3600)), ID: newest.ID}, pagingOrder[1:]},
		{"a time finer than a microsecond, read as the microsecond", policypg.Cursor{At: newest.At.Add(500 * time.Nanosecond), ID: newest.ID}, pagingOrder[1:]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.List(ctx, policypg.Filter{Before: &tt.before})
			require.NoError(t, err)
			assert.NotNil(t, got, "the end of the log is an empty page, not nil")
			assert.Equal(t, tt.want, targets(got))
		})
	}
}

func TestStore_EachFilterCombinedWithACursor(t *testing.T) {
	store := seedPagingLog(t)
	ctx := t.Context()
	full, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	at := map[string]policy.Record{}
	for _, rec := range full {
		at[rec.Action.Target] = rec
	}
	minute := func(n int) time.Time { return base.Add(time.Duration(n) * time.Minute) }

	tests := []struct {
		name   string
		filter policypg.Filter
		before string // the target of the record the cursor is built from
		want   []string
	}{
		{"effect", policypg.Filter{Effect: policy.Ask}, "n:7", []string{"n:2", "n:1", "n:11"}},
		{"rule", policypg.Filter{Rule: "Payment above the limit"}, "n:5", []string{"n:2"}},
		{"kind", policypg.Filter{Kind: "send"}, "n:7", []string{"n:1", "n:11"}},
		{"since, which cuts the other end", policypg.Filter{Since: minute(2)}, "n:8", []string{"n:5", "n:7", "n:4"}},
		{"since, with a cursor older than it", policypg.Filter{Since: minute(3)}, "n:4", []string{}},
		{"effect and kind", policypg.Filter{Effect: policy.Block, Kind: "delete"}, "n:10", []string{"n:3"}},
		{"effect, rule and kind", policypg.Filter{Effect: policy.Ask, Rule: "Sending needs a person", Kind: "send"}, "n:7", []string{"n:1", "n:11"}},
		{"all four", policypg.Filter{Effect: policy.Ask, Rule: "Sending needs a person", Kind: "send", Since: minute(1)}, "n:7", []string{"n:1"}},
		{"a limit", policypg.Filter{Limit: 3}, "n:6", []string{"n:8", "n:5", "n:7"}},
		{"a filter and a limit", policypg.Filter{Effect: policy.Ask, Limit: 2}, "n:7", []string{"n:2", "n:1"}},
		{"a cursor on a record the filter does not select", policypg.Filter{Effect: policy.Allow}, "n:10", []string{"n:8", "n:4", "n:0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.filter
			f.Before = after(at[tt.before])
			got, err := store.List(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, tt.want, targets(got))
		})
	}

	// And a filtered listing paged all the way, whatever the page size, is the
	// filtered listing in one piece.
	filters := []policypg.Filter{
		{},
		{Effect: policy.Ask},
		{Effect: policy.Block},
		{Rule: "Reading is allowed"},
		{Kind: "send"},
		{Since: minute(1)},
		{Effect: policy.Ask, Kind: "pay", Since: minute(1)},
		{Effect: policy.Ask, Rule: "Sending needs a person", Kind: "send", Since: minute(0)},
	}
	for _, f := range filters {
		whole, err := store.List(ctx, f)
		require.NoError(t, err)
		for _, size := range []int{1, 2, 3, 5} {
			t.Run(fmt.Sprintf("%+v paged by %d", f, size), func(t *testing.T) {
				all, _ := page(t, store, f, size)
				assert.Equal(t, targets(whole), targets(all))
				assert.True(t, slices.IsSortedFunc(all, func(a, b policy.Record) int { return b.At.Compare(a.At) }), "newest first")
			})
		}
	}
}

func TestStore_ListRefusesAMalformedCursorBeforeAQuery(t *testing.T) {
	tests := []struct {
		name   string
		cursor policypg.Cursor
	}{
		{"no id", policypg.Cursor{At: base}},
		{"a zero id", policypg.Cursor{At: base, ID: 0}},
		{"a negative id", policypg.Cursor{At: base, ID: -1}},
		{"the smallest id there is", policypg.Cursor{At: base, ID: -1 << 63}},
		{"the zero cursor", policypg.Cursor{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &fakeDB{}
			got, err := policypg.New(db).List(t.Context(), policypg.Filter{Before: &tt.cursor})
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), "cursor")
			assert.Contains(t, err.Error(), "policy/pg")
			assert.Zero(t, db.queries, "a query is not made")
		})
	}

	t.Run("a cursor with an id is passed on, whatever its time", func(t *testing.T) {
		db := &fakeDB{rows: brokenRows{}}
		_, err := policypg.New(db).List(t.Context(), policypg.Filter{Before: &policypg.Cursor{ID: 1}})
		require.NoError(t, err)
		assert.Equal(t, 1, db.queries, "the zero time is a position too")
	})

	t.Run("no cursor is the newest page", func(t *testing.T) {
		db := &fakeDB{rows: brokenRows{}}
		_, err := policypg.New(db).List(t.Context(), policypg.Filter{})
		require.NoError(t, err)
		assert.Equal(t, 1, db.queries)
	})
}

func TestStore_TheCeilingOnALimitDoesNotHideTheOlderRecords(t *testing.T) {
	// A page is at most 1000 however much is asked for, and a cursor reaches
	// what the page left behind.
	store, pool := openStore(t)
	seedBulk(t, pool, 2005)

	all, sizes := page(t, store, policypg.Filter{}, 1_000_000)
	assert.Equal(t, []int{1000, 1000, 5, 0}, sizes)
	require.Len(t, all, 2005)
	assert.Equal(t, "2005", all[0].Action.Target)
	assert.Equal(t, "1", all[2004].Action.Target)
	seen := map[string]bool{}
	for _, rec := range all {
		seen[rec.Action.Target] = true
	}
	assert.Len(t, seen, 2005, "every record once")
}

// seedBulk writes n rows in one statement, decided one second apart from the
// base time, with targets "1" to n.
func seedBulk(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `
		insert into `+policypg.Table+` (decided_at, kind, target, effect, rule, rule_index)
		select timestamptz '2026-10-02 09:00:00+00' + n * interval '1 second', 'bulk', n::text, 'allow', 'r', 0
		from generate_series(1, $1::int) as n`, n)
	require.NoError(t, err)
}

// A rule is the one thing to filter on that has an index of its own, and it must
// serve the listing's order, or a rare rule is found by reading the time index
// from the newest decision back, or by sorting every decision of the rule. The
// server is asked for the plan of exactly the statement List runs.
func TestStore_ARuleFilterIsServedByTheIndexInTheOrderOfTheListing(t *testing.T) {
	store, pool := openStore(t)
	ctx := t.Context()

	// 30000 decisions, one second apart, of which every 3000th is of a rare rule.
	_, err := pool.Exec(ctx, `
		insert into `+policypg.Table+` (decided_at, kind, target, effect, rule, rule_index)
		select timestamptz '2026-10-02 09:00:00+00' + n * interval '1 second', 'bulk', n::text, 'allow',
		       case when n % 3000 = 0 then 'rare' else 'common' end, 0
		from generate_series(1, 30000) as n`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `analyze `+policypg.Table)
	require.NoError(t, err)

	middle := &policypg.Cursor{At: base.Add(15000 * time.Second), ID: 15000}
	for name, f := range map[string]policypg.Filter{
		"a rule":                       {Rule: "rare"},
		"a rule and a cursor":          {Rule: "rare", Before: middle},
		"a rule, a cursor and a limit": {Rule: "rare", Before: middle, Limit: 3},
		"a rule, an effect and since":  {Rule: "rare", Effect: policy.Allow, Since: base.Add(time.Hour)},
	} {
		sql, args := f.Query()
		rows, err := pool.Query(ctx, "explain (costs off) "+sql, args...)
		require.NoErrorf(t, err, "%s", name)
		var plan []string
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			plan = append(plan, line)
		}
		require.NoError(t, rows.Err())
		text := strings.Join(plan, "\n")
		assert.Contains(t, text, "policy_decisions_rule_idx", "%s:\n%s", name, text)
		assert.NotContains(t, text, "Sort", "%s: the index must hand rows over in the order of the listing:\n%s", name, text)
	}

	// And it reads what was asked for.
	got, err := store.List(ctx, policypg.Filter{Rule: "rare", Before: middle})
	require.NoError(t, err)
	assert.Equal(t, []string{"12000", "9000", "6000", "3000"}, targets(got))
}
