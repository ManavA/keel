package policy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/policy"
)

var errDisk = errors.New("disk full")

// failingRecorder cannot write.
type failingRecorder struct {
	mu    sync.Mutex
	calls int
}

func (f *failingRecorder) Record(context.Context, policy.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return errDisk
}

// flakyRecorder fails for the calls its pattern marks and records the rest.
type flakyRecorder struct {
	inner *policy.MemoryRecorder
	fail  []bool
	n     int
}

func (f *flakyRecorder) Record(ctx context.Context, rec policy.Record) error {
	i := f.n
	f.n++
	if i < len(f.fail) && f.fail[i] {
		return errDisk
	}
	return f.inner.Record(ctx, rec)
}

// ctxRecorder keeps the context it was called with.
type ctxRecorder struct{ ctx context.Context }

func (c *ctxRecorder) Record(ctx context.Context, _ policy.Record) error {
	c.ctx = ctx
	return nil
}

// captureRecorder keeps exactly what it is handed, without copying it.
type captureRecorder struct {
	mu   sync.Mutex
	recs []policy.Record
}

func (c *captureRecorder) Record(_ context.Context, rec policy.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, rec)
	return nil
}

// ctxAwareRecorder is a recorder that, like a database's, gives up on a
// context that is done.
type ctxAwareRecorder struct{ inner *policy.MemoryRecorder }

func (c ctxAwareRecorder) Record(ctx context.Context, rec policy.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.inner.Record(ctx, rec)
}

// panicRecorder panics.
type panicRecorder struct{ value any }

func (p panicRecorder) Record(context.Context, policy.Record) error { panic(p.value) }

// deciderPolicy has a rule for each effect, so a test can ask for any.
func deciderPolicy() policy.Policy {
	return policy.Policy{
		Version: "2026-10-02",
		Default: policy.Allow,
		Rules: []policy.Rule{
			{Name: "sends ask", Effect: policy.Ask, When: policy.Match{Kinds: []string{"send"}}},
			{Name: "deletes are blocked", Effect: policy.Block, When: policy.Match{Kinds: []string{"delete"}}},
		},
	}
}

var decideTime = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

func TestNewDecider(t *testing.T) {
	t.Run("refuses an invalid policy", func(t *testing.T) {
		d, err := policy.NewDecider(policy.Policy{Rules: []policy.Rule{{Effect: policy.Allow}}}, policy.Options{})
		require.Error(t, err)
		assert.Nil(t, d)
		assert.Contains(t, err.Error(), "no name")
		assert.Contains(t, err.Error(), "policy: new decider")
	})

	t.Run("refuses a condition that can never hold", func(t *testing.T) {
		for name, p := range map[string]policy.Policy{
			"no attribute": {Rules: []policy.Rule{{Name: "r", Effect: policy.Block, When: policy.Match{Attrs: []policy.Cond{{Op: policy.OpEq, Value: 1}}}}}},
			"eq no value":  {Rules: []policy.Rule{{Name: "r", Effect: policy.Block, When: policy.Match{Attrs: []policy.Cond{{Attr: "x", Op: policy.OpEq}}}}}},
			"empty kind":   {Rules: []policy.Rule{{Name: "r", Effect: policy.Block, When: policy.Match{Kinds: []string{""}}}}},
		} {
			d, err := policy.NewDecider(p, policy.Options{})
			require.Error(t, err, name)
			assert.Nil(t, d, name)
			assert.Contains(t, err.Error(), `rule 0 ("r")`, name)
		}
	})

	t.Run("refuses a default that is not an effect", func(t *testing.T) {
		d, err := policy.NewDecider(policy.Policy{Default: "deny"}, policy.Options{})
		require.Error(t, err)
		assert.Nil(t, d)
	})

	t.Run("accepts a valid policy", func(t *testing.T) {
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{})
		require.NoError(t, err)
		require.NotNil(t, d)
	})

	t.Run("accepts the empty policy, which blocks everything", func(t *testing.T) {
		d, err := policy.NewDecider(policy.Policy{}, policy.Options{})
		require.NoError(t, err)
		got, err := d.Decide(t.Context(), policy.Action{Kind: "read"})
		require.NoError(t, err)
		assert.Equal(t, policy.Block, got.Effect)
	})
}

func TestDecider_RecordsEachDecisionOnceWithTheVersionAndTheInjectedTime(t *testing.T) {
	tests := []struct {
		name   string
		action policy.Action
		want   policy.Decision
	}{
		{
			name: "an allow by default", action: policy.Action{Kind: "read", Target: "doc:1"},
			want: policy.Decision{Effect: policy.Allow, Rule: policy.RuleDefault, Index: -1},
		},
		{
			name: "an ask", action: policy.Action{Kind: "send", Target: "email:ap@example.com", Attrs: map[string]any{"external": true}},
			want: policy.Decision{Effect: policy.Ask, Rule: "sends ask", Index: 0, Matched: []string{"sends ask"}},
		},
		{
			name: "a block", action: policy.Action{Kind: "delete", Target: "doc:2"},
			want: policy.Decision{Effect: policy.Block, Rule: "deletes are blocked", Index: 1, Matched: []string{"deletes are blocked"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := policy.NewMemoryRecorder()
			d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: rec, Now: func() time.Time { return decideTime }})
			require.NoError(t, err)

			got, err := d.Decide(t.Context(), tt.action)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			assert.Equal(t, []policy.Record{{At: decideTime, Action: tt.action, Decision: tt.want, Version: "2026-10-02"}}, rec.Records())
		})
	}

	t.Run("each of several decisions is recorded once, in order, at the time of its own call", func(t *testing.T) {
		rec := policy.NewMemoryRecorder()
		now := decideTime
		clock := func() time.Time {
			defer func() { now = now.Add(time.Second) }()
			return now
		}
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: rec, Now: clock})
		require.NoError(t, err)

		for _, k := range []string{"read", "send", "delete"} {
			_, err := d.Decide(t.Context(), policy.Action{Kind: k})
			require.NoError(t, err)
		}

		got := rec.Records()
		require.Len(t, got, 3)
		for i, k := range []string{"read", "send", "delete"} {
			assert.Equal(t, k, got[i].Action.Kind)
			assert.Equal(t, decideTime.Add(time.Duration(i)*time.Second), got[i].At)
		}
	})

	t.Run("the decision is the policy's own", func(t *testing.T) {
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{})
		require.NoError(t, err)
		for _, a := range []policy.Action{{Kind: "read"}, {Kind: "send"}, {Kind: "delete"}} {
			got, err := d.Decide(t.Context(), a)
			require.NoError(t, err)
			assert.Equal(t, deciderPolicy().Decide(a), got)
		}
	})

	t.Run("the caller's context reaches the recorder", func(t *testing.T) {
		type key struct{}
		rec := &ctxRecorder{}
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: rec})
		require.NoError(t, err)

		ctx := context.WithValue(t.Context(), key{}, "here")
		_, err = d.Decide(ctx, policy.Action{Kind: "read"})
		require.NoError(t, err)
		require.NotNil(t, rec.ctx)
		assert.Equal(t, "here", rec.ctx.Value(key{}))
	})

	t.Run("what could not be told is in the record", func(t *testing.T) {
		p := policy.Policy{Version: "v", Rules: []policy.Rule{{
			Name: "asks above 200", Effect: policy.Ask,
			When: policy.Match{Attrs: []policy.Cond{{Attr: "amount", Op: policy.OpGt, Value: json.Number("200")}}},
		}}}
		rec := policy.NewMemoryRecorder()
		d, err := policy.NewDecider(p, policy.Options{Recorder: rec, Now: func() time.Time { return decideTime }})
		require.NoError(t, err)

		got, err := d.Decide(t.Context(), policy.Action{Kind: "pay", Attrs: map[string]any{"amount": "1250"}})
		require.NoError(t, err)
		assert.Equal(t, policy.Ask, got.Effect)
		assert.Equal(t, []string{"amount"}, got.Uncertain)

		recs := rec.Records()
		require.Len(t, recs, 1)
		assert.Equal(t, []string{"amount"}, recs[0].Decision.Uncertain)
		out, err := json.Marshal(recs[0].Decision)
		require.NoError(t, err)
		assert.Contains(t, string(out), `"uncertain":["amount"]`)
	})
}

// A decision that could not be written down must not be acted on. DESIGN.md
// 5.6: the zero Decision's empty Effect is not Allow, and every caller treats
// an effect that is not one of the three as block.
func TestDecider_ARecordThatFailsGivesAnErrorAndNoAllow(t *testing.T) {
	tests := []struct {
		name   string
		action policy.Action
		// computed is what the policy decides for the action, to show that the
		// case would be an Allow, an Ask or a Block if the record had been written.
		computed policy.Effect
	}{
		{name: "an action the policy allows", action: policy.Action{Kind: "read"}, computed: policy.Allow},
		{name: "an action the policy asks about", action: policy.Action{Kind: "send"}, computed: policy.Ask},
		{name: "an action the policy blocks", action: policy.Action{Kind: "delete"}, computed: policy.Block},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.computed, deciderPolicy().Decide(tt.action).Effect, "the fixture")

			rec := &failingRecorder{}
			d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: rec, Logger: slog.New(slog.DiscardHandler)})
			require.NoError(t, err)

			got, err := d.Decide(t.Context(), tt.action)

			require.Error(t, err)
			assert.ErrorIs(t, err, errDisk)
			assert.NotEqual(t, policy.Allow, got.Effect)
			assert.False(t, got.Effect.Valid(), "an effect no caller can mistake for one of the three")
			assert.Equal(t, policy.Decision{}, got)
			assert.Equal(t, 1, rec.calls)
		})
	}

	t.Run("a failure does not spoil the next decision", func(t *testing.T) {
		mem := policy.NewMemoryRecorder()
		rec := &flakyRecorder{inner: mem, fail: []bool{false, true, false}}
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: rec, Logger: slog.New(slog.DiscardHandler)})
		require.NoError(t, err)

		first, err := d.Decide(t.Context(), policy.Action{Kind: "read"})
		require.NoError(t, err)
		assert.Equal(t, policy.Allow, first.Effect)

		second, err := d.Decide(t.Context(), policy.Action{Kind: "read"})
		require.ErrorIs(t, err, errDisk)
		assert.Equal(t, policy.Decision{}, second)

		third, err := d.Decide(t.Context(), policy.Action{Kind: "read"})
		require.NoError(t, err)
		assert.Equal(t, policy.Allow, third.Effect)
		assert.Len(t, mem.Records(), 2)
	})

	t.Run("a context that is done reaches a recorder that honours it, and gives no decision", func(t *testing.T) {
		mem := policy.NewMemoryRecorder()
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: ctxAwareRecorder{inner: mem}, Logger: slog.New(slog.DiscardHandler)})
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		got, err := d.Decide(ctx, policy.Action{Kind: "read"})
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, policy.Decision{}, got)
		assert.Empty(t, mem.Records())

		got, err = d.Decide(t.Context(), policy.Action{Kind: "read"})
		require.NoError(t, err)
		assert.Equal(t, policy.Allow, got.Effect)
	})

	t.Run("the in-memory recorder cannot block, so it does not look at the context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		mem := policy.NewMemoryRecorder()
		require.NoError(t, mem.Record(ctx, policy.Record{Action: policy.Action{Kind: "read"}}))
		assert.Len(t, mem.Records(), 1)

		d, err := policy.NewDecider(deciderPolicy(), policy.Options{})
		require.NoError(t, err)
		got, err := d.Decide(ctx, policy.Action{Kind: "read"})
		require.NoError(t, err)
		assert.Equal(t, policy.Allow, got.Effect)
	})

	t.Run("a recorder that panics takes Decide with it, and no decision comes back", func(t *testing.T) {
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: panicRecorder{value: "recorder failed"}})
		require.NoError(t, err)

		var got policy.Decision
		var gotErr error
		assert.PanicsWithValue(t, "recorder failed", func() {
			got, gotErr = d.Decide(t.Context(), policy.Action{Kind: "read"})
		})
		assert.Equal(t, policy.Decision{}, got, "an allow never reaches a caller that recovers")
		assert.NoError(t, gotErr)
	})
}

func TestDecider_LogsAFailedRecordWithoutTheActionsFacts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: &failingRecorder{}, Logger: logger})
	require.NoError(t, err)

	_, err = d.Decide(t.Context(), policy.Action{
		Kind:   "send",
		Target: "email:private.person@example.com",
		Attrs:  map[string]any{"body": "the secret passphrase"},
	})
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "level=ERROR")
	assert.Contains(t, out, "record policy decision")
	assert.Contains(t, out, "disk full")
	assert.Contains(t, out, `"sends ask"`)
	assert.Contains(t, out, "kind=send")
	assert.NotContains(t, out, "private.person")
	assert.NotContains(t, out, "secret passphrase")
}

// guardedPolicy has a block rule with a condition of every kind that holds a
// list or a value, so that a change to any of them through a shared slice
// would unblock what it blocks.
func guardedPolicy() policy.Policy {
	return policy.Policy{
		Version: "v1",
		Default: policy.Allow,
		Rules: []policy.Rule{{
			Name:   "risky visits are blocked",
			Effect: policy.Block,
			When: policy.Match{
				Kinds:  []string{"visit", "tour"},
				Target: "site:*",
				Attrs: []policy.Cond{
					{Attr: "region", Op: policy.OpIn, Value: []any{"north", "south"}},
					{Attr: "tag", Op: policy.OpIn, Value: []string{"a", "b"}},
					{Attr: "code", Op: policy.OpIn, Value: [2]int{7, 8}},
					{Attr: "level", Op: policy.OpGt, Value: json.Number("3")},
				},
			},
		}},
	}
}

func riskyVisit() policy.Action {
	return policy.Action{Kind: "visit", Target: "site:1", Attrs: map[string]any{"region": "north", "tag": "a", "code": 7, "level": 5}}
}

// tamper changes every list and value in p that a rule is made of, each to what
// would let riskyVisit through.
func tamper(t *testing.T, p policy.Policy) {
	t.Helper()
	w := &p.Rules[0].When
	w.Kinds[0] = "other"
	w.Kinds[1] = "other"
	w.Target = "elsewhere:*"
	w.Attrs[0].Value.([]any)[0] = "zzz"
	w.Attrs[1].Value.([]string)[0] = "zzz"
	w.Attrs[2].Value = [2]int{99, 99}
	w.Attrs[3].Value = json.Number("99")
	w.Attrs[0].Attr = "other"
	p.Rules[0].Effect = policy.Allow
	p.Rules[0].Name = "changed"
	p.Rules = append(p.Rules[:0], policy.Rule{Name: "all allowed", Effect: policy.Allow})
}

func TestDecider_IsImmutable(t *testing.T) {
	t.Run("changing the policy it was built from, lists and values included, changes nothing", func(t *testing.T) {
		p := guardedPolicy()
		d, err := policy.NewDecider(p, policy.Options{})
		require.NoError(t, err)
		require.Equal(t, policy.Block, mustDecide(t, d, riskyVisit()).Effect, "the fixture blocks")

		tamper(t, p)

		got := mustDecide(t, d, riskyVisit())
		assert.Equal(t, policy.Block, got.Effect)
		assert.Equal(t, "risky visits are blocked", got.Rule)
		assert.Equal(t, guardedPolicy(), d.Policy())
	})

	t.Run("changing the policy it returns, lists and values included, changes nothing", func(t *testing.T) {
		d, err := policy.NewDecider(guardedPolicy(), policy.Options{})
		require.NoError(t, err)

		p := d.Policy()
		tamper(t, p)

		assert.Equal(t, guardedPolicy(), d.Policy())
		assert.Equal(t, policy.Block, mustDecide(t, d, riskyVisit()).Effect)
	})

	t.Run("two copies from the Decider share nothing with each other", func(t *testing.T) {
		d, err := policy.NewDecider(guardedPolicy(), policy.Options{})
		require.NoError(t, err)
		a, b := d.Policy(), d.Policy()
		a.Rules[0].When.Attrs[0].Value.([]any)[0] = "zzz"
		assert.Equal(t, []any{"north", "south"}, b.Rules[0].When.Attrs[0].Value)
	})

	t.Run("a caller that keeps changing a list while the Decider decides races with nothing", func(t *testing.T) {
		p := guardedPolicy()
		list := p.Rules[0].When.Attrs[0].Value.([]any)
		d, err := policy.NewDecider(p, policy.Options{})
		require.NoError(t, err)

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
					list[0] = []string{"north", "zzz"}[i%2]
				}
			}
		}()
		for range 300 {
			assert.Equal(t, policy.Block, mustDecide(t, d, riskyVisit()).Effect)
		}
		close(stop)
		wg.Wait()
	})
}

func mustDecide(t *testing.T, d *policy.Decider, a policy.Action) policy.Decision {
	t.Helper()
	got, err := d.Decide(t.Context(), a)
	require.NoError(t, err)
	return got
}

// What a Recorder is handed is a copy, in both directions.
func TestDecider_HandsTheRecorderACopy(t *testing.T) {
	p := policy.Policy{Rules: []policy.Rule{
		{Name: "a", Effect: policy.Allow},
		{Name: "b", Effect: policy.Ask},
	}}
	action := func() policy.Action {
		return policy.Action{Kind: "k", Target: "t", Attrs: map[string]any{
			"list": []any{"x", []any{"y"}}, "obj": map[string]any{"k": []string{"v"}}, "n": 1,
		}}
	}

	t.Run("a nested list the caller changes after Decide does not change what the recorder holds", func(t *testing.T) {
		rec := &captureRecorder{}
		d, err := policy.NewDecider(p, policy.Options{Recorder: rec})
		require.NoError(t, err)
		a := action()
		mustDecide(t, d, a)

		a.Attrs["list"].([]any)[0] = "changed"
		a.Attrs["list"].([]any)[1].([]any)[0] = "changed"
		a.Attrs["obj"].(map[string]any)["k"].([]string)[0] = "changed"
		a.Attrs["n"] = 2

		require.Len(t, rec.recs, 1)
		assert.Equal(t, action().Attrs, rec.recs[0].Action.Attrs)
	})

	t.Run("a record the recorder changes does not change the caller's action or decision", func(t *testing.T) {
		rec := &captureRecorder{}
		d, err := policy.NewDecider(p, policy.Options{Recorder: rec})
		require.NoError(t, err)
		a := action()
		got := mustDecide(t, d, a)

		require.Len(t, rec.recs, 1)
		rec.recs[0].Action.Attrs["list"].([]any)[0] = "changed"
		rec.recs[0].Action.Attrs["obj"].(map[string]any)["k"].([]string)[0] = "changed"
		rec.recs[0].Decision.Matched[0] = "changed"
		rec.recs[0].Decision.Effect = policy.Allow

		assert.Equal(t, action(), a)
		assert.Equal(t, []string{"a", "b"}, got.Matched)
		assert.Equal(t, policy.Ask, got.Effect)
	})
}

// The copy of a policy or an action is bounded in work, not only in depth: a
// value that shares a child twice at every level would otherwise double at each
// level. Past a few thousand values NewDecider refuses the policy, and Decide
// returns an error and no decision, as when a record cannot be written.
func TestDecider_RefusesWhatIsTooLargeToCopy(t *testing.T) {
	flat := func(n int) []any {
		list := make([]any, n)
		for i := range list {
			list[i] = "v" + strconv.Itoa(i)
		}
		return list
	}
	inRule := func(name string, values []any) policy.Rule {
		return policy.Rule{Name: name, Effect: policy.Block, When: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: policy.OpIn, Value: values}}}}
	}

	t.Run("an action that shares a child at every level gets an error and no decision", func(t *testing.T) {
		rec := &captureRecorder{}
		var logs bytes.Buffer
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: rec, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
		require.NoError(t, err)
		action := policy.Action{Kind: "read", Target: "doc:private", Attrs: map[string]any{"deep": sharedChild(20), "secret": "hunter2"}}
		require.Equal(t, policy.Allow, deciderPolicy().Decide(action).Effect, "the fixture is an action the policy allows")

		started := time.Now()
		got, err := d.Decide(t.Context(), action)
		elapsed := time.Since(started)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "policy:")
		assert.Contains(t, err.Error(), "10000")
		assert.Equal(t, policy.Decision{}, got)
		assert.False(t, got.Effect.Valid())
		assert.Empty(t, rec.recs, "nothing was recorded")
		assert.Less(t, elapsed, 2*time.Second, "the work is bounded, not only the depth")
		assert.Contains(t, logs.String(), "level=ERROR")
		assert.NotContains(t, logs.String(), "hunter2")
		assert.NotContains(t, logs.String(), "doc:private")
	})

	t.Run("the same action without the sharing is decided and recorded", func(t *testing.T) {
		rec := &captureRecorder{}
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: rec})
		require.NoError(t, err)
		got, err := d.Decide(t.Context(), policy.Action{Kind: "read", Attrs: map[string]any{"list": flat(5000)}})
		require.NoError(t, err)
		assert.Equal(t, policy.Allow, got.Effect)
		assert.Len(t, rec.recs, 1)
	})

	t.Run("a flat list too large for a record is refused too", func(t *testing.T) {
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: &captureRecorder{}, Logger: slog.New(slog.DiscardHandler)})
		require.NoError(t, err)
		got, err := d.Decide(t.Context(), policy.Action{Kind: "read", Attrs: map[string]any{"list": flat(20000)}})
		require.Error(t, err)
		assert.Equal(t, policy.Decision{}, got)
	})

	t.Run("NewDecider refuses a policy with more values in its conditions than can be copied", func(t *testing.T) {
		d, err := policy.NewDecider(policy.Policy{Rules: []policy.Rule{inRule("big", flat(20000))}}, policy.Options{})
		require.Error(t, err)
		assert.Nil(t, d)
		assert.Contains(t, err.Error(), "policy: new decider")
		assert.Contains(t, err.Error(), "10000")
	})

	t.Run("the bound is on the whole policy, not on each condition", func(t *testing.T) {
		rules := []policy.Rule{inRule("a", flat(4000)), inRule("b", flat(4000)), inRule("c", flat(4000))}
		d, err := policy.NewDecider(policy.Policy{Rules: rules}, policy.Options{})
		require.Error(t, err)
		assert.Nil(t, d)
		d, err = policy.NewDecider(policy.Policy{Rules: rules[:2]}, policy.Options{})
		require.NoError(t, err)
		assert.Len(t, d.Policy().Rules, 2, "and what was accepted can be copied out")
	})

	// A value counts once, a list once and each element once; an interface is
	// the value inside it. So the attributes map and a list of n strings are
	// n+2 values, and a condition's list of n strings is n+1.
	t.Run("the bound for an action is exact", func(t *testing.T) {
		d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: &captureRecorder{}, Logger: slog.New(slog.DiscardHandler)})
		require.NoError(t, err)
		_, err = d.Decide(t.Context(), policy.Action{Kind: "read", Attrs: map[string]any{"list": make([]string, 9998)}})
		assert.NoError(t, err, "10000 values")
		got, err := d.Decide(t.Context(), policy.Action{Kind: "read", Attrs: map[string]any{"list": make([]string, 9999)}})
		assert.Error(t, err, "10001 values")
		assert.Equal(t, policy.Decision{}, got)
	})

	t.Run("the bound for a policy is exact", func(t *testing.T) {
		strings := func(n int) []string {
			list := make([]string, n)
			for i := range list {
				list[i] = "v" + strconv.Itoa(i)
			}
			return list
		}
		rule := func(n int) policy.Policy {
			return policy.Policy{Rules: []policy.Rule{{Name: "r", Effect: policy.Block, When: policy.Match{
				Attrs: []policy.Cond{{Attr: "a", Op: policy.OpIn, Value: strings(n)}},
			}}}}
		}
		_, err := policy.NewDecider(rule(9999), policy.Options{})
		assert.NoError(t, err, "10000 values")
		_, err = policy.NewDecider(rule(10000), policy.Options{})
		assert.Error(t, err, "10001 values")
	})

	t.Run("a policy of ordinary size is accepted", func(t *testing.T) {
		d, err := policy.NewDecider(policy.Policy{Rules: []policy.Rule{inRule("ok", flat(5000))}}, policy.Options{})
		require.NoError(t, err)
		assert.Equal(t, policy.Block, mustDecide(t, d, policy.Action{Kind: "k", Attrs: map[string]any{"a": "v4999"}}).Effect)
	})
}

func TestDecider_IsSafeForConcurrentUse(t *testing.T) {
	const goroutines, each = 16, 25
	rec := policy.NewMemoryRecorder()
	d, err := policy.NewDecider(deciderPolicy(), policy.Options{Recorder: rec})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				got, err := d.Decide(t.Context(), policy.Action{Kind: "send"})
				assert.NoError(t, err)
				assert.Equal(t, policy.Ask, got.Effect)
			}
		}()
	}
	wg.Wait()
	assert.Len(t, rec.Records(), goroutines*each)
}
