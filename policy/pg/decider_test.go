package pg_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/policy"
	policypg "github.com/ManavA/keel/policy/pg"
)

// testPolicy is the policy of the design's JSON example: reading is allowed,
// sending and large payments ask, deleting is blocked, and anything else is.
func testPolicy() policy.Policy {
	return policy.Policy{
		Version: "2026-10-02",
		Rules: []policy.Rule{
			{Name: "Reading is allowed", Effect: policy.Allow, When: policy.Match{Kinds: []string{"read"}}},
			{Name: "Sending needs a person", Effect: policy.Ask, When: policy.Match{Kinds: []string{"send"}}},
			{Name: "Payment above the $200 limit", Effect: policy.Ask, When: policy.Match{
				Kinds: []string{"pay"},
				Attrs: []policy.Cond{{Attr: "amount", Op: policy.OpGt, Value: json.Number("200")}},
			}},
			{Name: "Deleting documents is never allowed", Effect: policy.Block, When: policy.Match{Kinds: []string{"delete"}}},
		},
	}
}

// ticking is a clock that moves one second at each reading, from base.
func ticking() func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Second)
	}
}

func TestDecider_OverTheStoreRecordsEachDecisionItMakes(t *testing.T) {
	store, pool := openStore(t)
	ctx := t.Context()
	d, err := policy.NewDecider(testPolicy(), policy.Options{Recorder: store, Now: ticking()})
	require.NoError(t, err)

	tests := []struct {
		name          string
		action        policy.Action
		wantAttrs     map[string]any
		wantEffect    policy.Effect
		wantRule      string
		wantIndex     int
		wantMatched   []string
		wantUncertain []string
	}{
		{
			name:       "an allowed read",
			action:     policy.Action{Kind: "read", Target: "doc:a"},
			wantAttrs:  map[string]any{},
			wantEffect: policy.Allow, wantRule: "Reading is allowed", wantIndex: 0,
			wantMatched: []string{"Reading is allowed"}, wantUncertain: []string{},
		},
		{
			name:       "a send that asks",
			action:     policy.Action{Kind: "send", Target: "email:ap@example.com", Attrs: map[string]any{"external": true, "cc": nil}},
			wantAttrs:  map[string]any{"external": true, "cc": nil},
			wantEffect: policy.Ask, wantRule: "Sending needs a person", wantIndex: 1,
			wantMatched: []string{"Sending needs a person"}, wantUncertain: []string{},
		},
		{
			name:       "a payment above the limit, as a number too large for a float64",
			action:     policy.Action{Kind: "pay", Attrs: map[string]any{"amount": json.Number("1180591620717411303424")}},
			wantAttrs:  map[string]any{"amount": json.Number("1180591620717411303424")},
			wantEffect: policy.Ask, wantRule: "Payment above the $200 limit", wantIndex: 2,
			wantMatched: []string{"Payment above the $200 limit"}, wantUncertain: []string{},
		},
		{
			name:       "a payment whose amount is a string, which cannot be told and asks",
			action:     policy.Action{Kind: "pay", Attrs: map[string]any{"amount": "1250"}},
			wantAttrs:  map[string]any{"amount": "1250"},
			wantEffect: policy.Ask, wantRule: "Payment above the $200 limit", wantIndex: 2,
			wantMatched: []string{"Payment above the $200 limit"}, wantUncertain: []string{"amount"},
		},
		{
			name:       "a payment below the limit, which no rule matches",
			action:     policy.Action{Kind: "pay", Attrs: map[string]any{"amount": 50}},
			wantAttrs:  map[string]any{"amount": json.Number("50")},
			wantEffect: policy.Block, wantRule: policy.RuleDefault, wantIndex: -1,
			wantMatched: []string{}, wantUncertain: []string{},
		},
		{
			name:       "a delete that is blocked",
			action:     policy.Action{Kind: "delete", Target: "doc:a"},
			wantAttrs:  map[string]any{},
			wantEffect: policy.Block, wantRule: "Deleting documents is never allowed", wantIndex: 3,
			wantMatched: []string{"Deleting documents is never allowed"}, wantUncertain: []string{},
		},
	}

	var want []policy.Record
	for i, tt := range tests {
		dec, err := d.Decide(ctx, tt.action)
		require.NoErrorf(t, err, "%s", tt.name)
		assert.Equal(t, tt.wantEffect, dec.Effect, tt.name)
		assert.Equal(t, tt.wantRule, dec.Rule, tt.name)

		// Newest first: each record goes in front of the ones before it.
		want = slices.Insert(want, 0, policy.Record{
			At:     base.Add(time.Duration(i+1) * time.Second),
			Action: policy.Action{Kind: tt.action.Kind, Target: tt.action.Target, Attrs: tt.wantAttrs},
			Decision: policy.Decision{
				Effect: tt.wantEffect, Rule: tt.wantRule, Index: tt.wantIndex,
				Matched: tt.wantMatched, Uncertain: tt.wantUncertain,
			},
			Version: "2026-10-02",
		})
	}

	got, err := store.List(ctx, policypg.Filter{})
	require.NoError(t, err)
	requireRecords(t, want, got)

	// Once each: a decision is one row.
	var rows int
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+policypg.Table).Scan(&rows))
	assert.Equal(t, len(tests), rows)
}

func TestDecider_OverTheStoreGivesAnErrorAndNoAllowWhenTheRecordCannotBeWritten(t *testing.T) {
	// Every case below decides an action the policy allows, so a Decider that
	// returned the computed decision when the record failed would return Allow
	// and fail the test.
	allowed := policy.Action{Kind: "read", Target: "doc:a"}

	tests := []struct {
		name  string
		store func(t *testing.T) (*policypg.Store, context.Context)
		check func(t *testing.T, err error)
	}{
		{
			name: "the pool has been closed",
			store: func(t *testing.T) (*policypg.Store, context.Context) {
				_, pool := openStore(t)
				pool.Close()
				return policypg.New(pool), t.Context()
			},
		},
		{
			name: "nothing is listening where the database was",
			store: func(t *testing.T) (*policypg.Store, context.Context) {
				return policypg.New(deadPool(t)), t.Context()
			},
		},
		{
			name: "the caller's context is already cancelled",
			store: func(t *testing.T) (*policypg.Store, context.Context) {
				store, _ := openStore(t)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return store, ctx
			},
			check: func(t *testing.T, err error) { assert.ErrorIs(t, err, context.Canceled) },
		},
		{
			name: "the database returns an error",
			store: func(t *testing.T) (*policypg.Store, context.Context) {
				return policypg.New(&fakeDB{err: &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}}), t.Context()
			},
		},
		{
			name: "the insert writes no row",
			store: func(t *testing.T) (*policypg.Store, context.Context) {
				return policypg.New(&fakeDB{tag: pgconn.NewCommandTag("INSERT 0 0")}), t.Context()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, ctx := tt.store(t)
			p := testPolicy()
			d, err := policy.NewDecider(p, policy.Options{Recorder: store, Now: ticking()})
			require.NoError(t, err)

			// What the policy itself says, so the test can tell the decision it
			// must not return.
			require.Equal(t, policy.Allow, p.Decide(allowed).Effect)

			dec, err := d.Decide(ctx, allowed)
			require.Error(t, err)
			assert.NotEqual(t, policy.Allow, dec.Effect, "a decision that could not be recorded is not an allow")
			assert.Equal(t, policy.Decision{}, dec)
			assert.Contains(t, err.Error(), "policy/pg")
			assert.NotErrorIs(t, err, policy.ErrUnrecordable, "the database may be back by the next try")
			if tt.check != nil {
				tt.check(t, err)
			}
		})
	}
}

func TestDecider_OverTheStoreIsNotAllowedAnActionWhoseAttributesCannotBeRecorded(t *testing.T) {
	// The decision for a NUL in a model-written value is made and then fails to
	// be recorded, so the Decider returns the error and no decision, and the log
	// holds nothing for it. The next action is decided as usual.
	store, pool := openStore(t)
	ctx := t.Context()
	d, err := policy.NewDecider(testPolicy(), policy.Options{Recorder: store, Now: ticking()})
	require.NoError(t, err)

	dec, err := d.Decide(ctx, policy.Action{Kind: "read", Attrs: map[string]any{"note": "a\x00b"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NUL")
	assert.ErrorIs(t, err, policy.ErrUnrecordable, "an agent that retries a failed step must not retry this one")
	assert.Equal(t, policy.Decision{}, dec)

	var rows int
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+policypg.Table).Scan(&rows))
	assert.Zero(t, rows)

	dec, err = d.Decide(ctx, policy.Action{Kind: "read", Attrs: map[string]any{"note": "fine"}})
	require.NoError(t, err)
	assert.Equal(t, policy.Allow, dec.Effect)
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+policypg.Table).Scan(&rows))
	assert.Equal(t, 1, rows)
}

// deadPool is a pool aimed at a port nothing is listening on: the database is
// unreachable, and connecting fails at once.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	pool, err := pgxpool.New(t.Context(), fmt.Sprintf("postgres://keel:keel@127.0.0.1:%d/keel?sslmode=disable&connect_timeout=5", port))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// The pool is lazy: it has contacted nothing, and the first statement fails.
	err = pool.Ping(t.Context())
	require.Error(t, err)
	assert.False(t, errors.Is(err, context.Canceled), "the test needs a refused connection, not a cancelled one")
	return pool
}
