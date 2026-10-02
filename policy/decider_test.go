package policy

// This file is in package policy, not policy_test, for one case: the zero
// Options record in memory, and the log is not otherwise reachable.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errDisk = errors.New("disk full")

// failingRecorder cannot write.
type failingRecorder struct {
	mu    sync.Mutex
	calls int
}

func (f *failingRecorder) Record(context.Context, Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return errDisk
}

// flakyRecorder fails for the calls its pattern marks and records the rest.
type flakyRecorder struct {
	inner *MemoryRecorder
	fail  []bool
	n     int
}

func (f *flakyRecorder) Record(ctx context.Context, rec Record) error {
	i := f.n
	f.n++
	if i < len(f.fail) && f.fail[i] {
		return errDisk
	}
	return f.inner.Record(ctx, rec)
}

// ctxRecorder keeps the context it was called with.
type ctxRecorder struct{ ctx context.Context }

func (c *ctxRecorder) Record(ctx context.Context, _ Record) error {
	c.ctx = ctx
	return nil
}

// deciderPolicy has a rule for each effect, so a test can ask for any.
func deciderPolicy() Policy {
	return Policy{
		Version: "2026-10-02",
		Default: Allow,
		Rules: []Rule{
			{Name: "sends ask", Effect: Ask, When: Match{Kinds: []string{"send"}}},
			{Name: "deletes are blocked", Effect: Block, When: Match{Kinds: []string{"delete"}}},
		},
	}
}

var decideTime = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

func TestNewDecider(t *testing.T) {
	t.Run("refuses an invalid policy", func(t *testing.T) {
		d, err := NewDecider(Policy{Rules: []Rule{{Effect: Allow}}}, Options{})
		require.Error(t, err)
		assert.Nil(t, d)
		assert.Contains(t, err.Error(), "no name")
		assert.Contains(t, err.Error(), "policy: new decider")
	})

	t.Run("refuses a condition that can never hold", func(t *testing.T) {
		for name, p := range map[string]Policy{
			"no attribute": {Rules: []Rule{{Name: "r", Effect: Block, When: Match{Attrs: []Cond{{Op: OpEq, Value: 1}}}}}},
			"eq no value":  {Rules: []Rule{{Name: "r", Effect: Block, When: Match{Attrs: []Cond{{Attr: "x", Op: OpEq}}}}}},
			"empty kind":   {Rules: []Rule{{Name: "r", Effect: Block, When: Match{Kinds: []string{""}}}}},
		} {
			d, err := NewDecider(p, Options{})
			require.Error(t, err, name)
			assert.Nil(t, d, name)
			assert.Contains(t, err.Error(), `rule 0 ("r")`, name)
		}
	})

	t.Run("refuses a default that is not an effect", func(t *testing.T) {
		d, err := NewDecider(Policy{Default: "deny"}, Options{})
		require.Error(t, err)
		assert.Nil(t, d)
	})

	t.Run("accepts a valid policy", func(t *testing.T) {
		d, err := NewDecider(deciderPolicy(), Options{})
		require.NoError(t, err)
		require.NotNil(t, d)
	})

	t.Run("accepts the empty policy, which blocks everything", func(t *testing.T) {
		d, err := NewDecider(Policy{}, Options{})
		require.NoError(t, err)
		got, err := d.Decide(t.Context(), Action{Kind: "read"})
		require.NoError(t, err)
		assert.Equal(t, Block, got.Effect)
	})
}

func TestDecider_RecordsEachDecisionOnceWithTheVersionAndTheInjectedTime(t *testing.T) {
	tests := []struct {
		name   string
		action Action
		want   Decision
	}{
		{
			name: "an allow by default", action: Action{Kind: "read", Target: "doc:1"},
			want: Decision{Effect: Allow, Rule: RuleDefault, Index: -1},
		},
		{
			name: "an ask", action: Action{Kind: "send", Target: "email:ap@example.com", Attrs: map[string]any{"external": true}},
			want: Decision{Effect: Ask, Rule: "sends ask", Index: 0, Matched: []string{"sends ask"}},
		},
		{
			name: "a block", action: Action{Kind: "delete", Target: "doc:2"},
			want: Decision{Effect: Block, Rule: "deletes are blocked", Index: 1, Matched: []string{"deletes are blocked"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := NewMemoryRecorder()
			d, err := NewDecider(deciderPolicy(), Options{Recorder: rec, Now: func() time.Time { return decideTime }})
			require.NoError(t, err)

			got, err := d.Decide(t.Context(), tt.action)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			assert.Equal(t, []Record{{At: decideTime, Action: tt.action, Decision: tt.want, Version: "2026-10-02"}}, rec.Records())
		})
	}

	t.Run("each of several decisions is recorded once, in order, at the time of its own call", func(t *testing.T) {
		rec := NewMemoryRecorder()
		now := decideTime
		clock := func() time.Time {
			defer func() { now = now.Add(time.Second) }()
			return now
		}
		d, err := NewDecider(deciderPolicy(), Options{Recorder: rec, Now: clock})
		require.NoError(t, err)

		for _, k := range []string{"read", "send", "delete"} {
			_, err := d.Decide(t.Context(), Action{Kind: k})
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
		d, err := NewDecider(deciderPolicy(), Options{})
		require.NoError(t, err)
		for _, a := range []Action{{Kind: "read"}, {Kind: "send"}, {Kind: "delete"}} {
			got, err := d.Decide(t.Context(), a)
			require.NoError(t, err)
			assert.Equal(t, deciderPolicy().Decide(a), got)
		}
	})

	t.Run("the caller's context reaches the recorder", func(t *testing.T) {
		type key struct{}
		rec := &ctxRecorder{}
		d, err := NewDecider(deciderPolicy(), Options{Recorder: rec})
		require.NoError(t, err)

		ctx := context.WithValue(t.Context(), key{}, "here")
		_, err = d.Decide(ctx, Action{Kind: "read"})
		require.NoError(t, err)
		require.NotNil(t, rec.ctx)
		assert.Equal(t, "here", rec.ctx.Value(key{}))
	})
}

func TestDecider_ZeroOptionsRecordInMemory(t *testing.T) {
	d, err := NewDecider(deciderPolicy(), Options{})
	require.NoError(t, err)

	before := time.Now()
	got, err := d.Decide(t.Context(), Action{Kind: "send"})
	after := time.Now()
	require.NoError(t, err)
	assert.Equal(t, Ask, got.Effect)

	mem, ok := d.rec.(*MemoryRecorder)
	require.True(t, ok, "the recorder is %T", d.rec)
	recs := mem.Records()
	require.Len(t, recs, 1)
	assert.Equal(t, "2026-10-02", recs[0].Version)
	assert.False(t, recs[0].At.Before(before) || recs[0].At.After(after), "the time is the clock's: %v", recs[0].At)
	assert.Same(t, slog.Default(), d.log)
}

// A decision that could not be written down must not be acted on. DESIGN.md
// 5.6: the zero Decision's empty Effect is not Allow, and every caller treats
// an effect that is not one of the three as block.
func TestDecider_ARecordThatFailsGivesAnErrorAndNoAllow(t *testing.T) {
	tests := []struct {
		name   string
		action Action
		// computed is what the policy decides for the action, to show that the
		// case would be an Allow, an Ask or a Block if the record had been written.
		computed Effect
	}{
		{name: "an action the policy allows", action: Action{Kind: "read"}, computed: Allow},
		{name: "an action the policy asks about", action: Action{Kind: "send"}, computed: Ask},
		{name: "an action the policy blocks", action: Action{Kind: "delete"}, computed: Block},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.computed, deciderPolicy().Decide(tt.action).Effect, "the fixture")

			rec := &failingRecorder{}
			d, err := NewDecider(deciderPolicy(), Options{Recorder: rec, Logger: slog.New(slog.DiscardHandler)})
			require.NoError(t, err)

			got, err := d.Decide(t.Context(), tt.action)

			require.Error(t, err)
			assert.ErrorIs(t, err, errDisk)
			assert.NotEqual(t, Allow, got.Effect)
			assert.False(t, got.Effect.Valid(), "an effect no caller can mistake for one of the three")
			assert.Equal(t, Decision{}, got)
			assert.Equal(t, 1, rec.calls)
		})
	}

	t.Run("a failure does not spoil the next decision", func(t *testing.T) {
		mem := NewMemoryRecorder()
		rec := &flakyRecorder{inner: mem, fail: []bool{false, true, false}}
		d, err := NewDecider(deciderPolicy(), Options{Recorder: rec, Logger: slog.New(slog.DiscardHandler)})
		require.NoError(t, err)

		first, err := d.Decide(t.Context(), Action{Kind: "read"})
		require.NoError(t, err)
		assert.Equal(t, Allow, first.Effect)

		second, err := d.Decide(t.Context(), Action{Kind: "read"})
		require.ErrorIs(t, err, errDisk)
		assert.Equal(t, Decision{}, second)

		third, err := d.Decide(t.Context(), Action{Kind: "read"})
		require.NoError(t, err)
		assert.Equal(t, Allow, third.Effect)
		assert.Len(t, mem.Records(), 2)
	})
}

func TestDecider_LogsAFailedRecordWithoutTheActionsFacts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	d, err := NewDecider(deciderPolicy(), Options{Recorder: &failingRecorder{}, Logger: logger})
	require.NoError(t, err)

	_, err = d.Decide(t.Context(), Action{
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

func TestDecider_IsImmutable(t *testing.T) {
	t.Run("changing the policy it was built from changes nothing", func(t *testing.T) {
		p := deciderPolicy()
		d, err := NewDecider(p, Options{})
		require.NoError(t, err)

		p.Rules[1].Effect = Allow
		p.Rules[0].When.Kinds[0] = "read"
		p.Rules = append(p.Rules[:0], Rule{Name: "all", Effect: Allow})
		p.Version = "other"

		got, err := d.Decide(t.Context(), Action{Kind: "delete"})
		require.NoError(t, err)
		assert.Equal(t, Block, got.Effect)
		assert.Equal(t, deciderPolicy(), d.Policy())
	})

	t.Run("changing the policy it returns changes nothing", func(t *testing.T) {
		d, err := NewDecider(deciderPolicy(), Options{})
		require.NoError(t, err)

		p := d.Policy()
		require.Len(t, p.Rules, 2)
		p.Rules[1].Effect = Allow
		p.Rules[0].When.Kinds[0] = "read"
		p.Default = Block

		assert.Equal(t, deciderPolicy(), d.Policy())
		got, err := d.Decide(t.Context(), Action{Kind: "delete"})
		require.NoError(t, err)
		assert.Equal(t, Block, got.Effect)
	})

	t.Run("it returns the rules it decides under", func(t *testing.T) {
		d, err := NewDecider(deciderPolicy(), Options{})
		require.NoError(t, err)
		assert.Equal(t, deciderPolicy(), d.Policy())
	})
}

func TestDecider_IsSafeForConcurrentUse(t *testing.T) {
	const goroutines, each = 16, 25
	rec := NewMemoryRecorder()
	d, err := NewDecider(deciderPolicy(), Options{Recorder: rec})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				got, err := d.Decide(t.Context(), Action{Kind: "send"})
				assert.NoError(t, err)
				assert.Equal(t, Ask, got.Effect)
			}
		}()
	}
	wg.Wait()
	assert.Len(t, rec.Records(), goroutines*each)
}
