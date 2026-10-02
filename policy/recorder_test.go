package policy_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/policy"
)

var recordTime = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

func record(kind string) policy.Record {
	return policy.Record{
		At:       recordTime,
		Action:   policy.Action{Kind: kind, Target: "t:" + kind, Attrs: map[string]any{"external": true}},
		Decision: policy.Decision{Effect: policy.Ask, Rule: "r", Index: 2, Matched: []string{"r", "s"}},
		Version:  "v1",
	}
}

func TestMemoryRecorder_KeepsRecordsOldestFirst(t *testing.T) {
	m := policy.NewMemoryRecorder()
	assert.Empty(t, m.Records())

	for _, k := range []string{"read", "send", "delete"} {
		require.NoError(t, m.Record(t.Context(), record(k)))
	}

	got := m.Records()
	require.Len(t, got, 3)
	assert.Equal(t, record("read"), got[0])
	assert.Equal(t, record("send"), got[1])
	assert.Equal(t, record("delete"), got[2])
}

func TestMemoryRecorder_CopiesWhatItKeepsAndWhatItReturns(t *testing.T) {
	t.Run("a record changed after it was recorded is not changed in the log", func(t *testing.T) {
		m := policy.NewMemoryRecorder()
		rec := record("send")
		require.NoError(t, m.Record(t.Context(), rec))

		rec.Action.Attrs["external"] = false
		rec.Action.Attrs["added"] = 1
		rec.Decision.Matched[0] = "changed"
		rec.Version = "v2"

		got := m.Records()
		require.Len(t, got, 1)
		assert.Equal(t, record("send"), got[0])
	})

	t.Run("a record read back and changed is not changed in the log", func(t *testing.T) {
		m := policy.NewMemoryRecorder()
		require.NoError(t, m.Record(t.Context(), record("send")))

		got := m.Records()
		require.Len(t, got, 1)
		got[0].Action.Attrs["external"] = false
		got[0].Decision.Matched[0] = "changed"
		got[0].Version = "v2"
		got[0] = policy.Record{}

		assert.Equal(t, []policy.Record{record("send")}, m.Records())
	})
}

// tagged is a struct an attribute might hold, with a list inside it.
type tagged struct {
	Name string
	Tags []string
}

// A nested list or object the caller holds a reference to is as much a part of
// the record as the attribute map is.
func TestMemoryRecorder_CopiesNestedValuesToo(t *testing.T) {
	nested := func() policy.Record {
		rec := record("send")
		tags := []string{"p", "q"}
		rec.Action.Attrs = map[string]any{
			"list":    []any{"x", []any{"y", "z"}},
			"obj":     map[string]any{"k": []string{"v"}, "m": map[string]any{"deep": []int{1, 2}}},
			"typed":   []string{"a", "b"},
			"array":   [2]int{1, 2},
			"lists":   [1][]string{{"m", "n"}},
			"pointer": &tags,
			"struct":  tagged{Name: "n", Tags: []string{"s", "t"}},
			"pstruct": &tagged{Name: "m", Tags: []string{"u", "v"}},
			"scalar":  1,
		}
		rec.Decision.Uncertain = []string{"list"}
		return rec
	}

	t.Run("a nested value changed after it was recorded is not changed in the log", func(t *testing.T) {
		m := policy.NewMemoryRecorder()
		rec := nested()
		require.NoError(t, m.Record(t.Context(), rec))

		rec.Action.Attrs["list"].([]any)[0] = "changed"
		rec.Action.Attrs["list"].([]any)[1].([]any)[0] = "changed"
		rec.Action.Attrs["obj"].(map[string]any)["k"].([]string)[0] = "changed"
		rec.Action.Attrs["obj"].(map[string]any)["m"].(map[string]any)["deep"].([]int)[0] = 99
		rec.Action.Attrs["typed"].([]string)[0] = "changed"
		(*rec.Action.Attrs["pointer"].(*[]string))[0] = "changed"
		rec.Action.Attrs["lists"].([1][]string)[0][0] = "changed"
		rec.Action.Attrs["struct"].(tagged).Tags[0] = "changed"
		rec.Action.Attrs["pstruct"].(*tagged).Tags[0] = "changed"
		rec.Action.Attrs["pstruct"].(*tagged).Name = "changed"
		rec.Decision.Uncertain[0] = "changed"

		got := m.Records()
		require.Len(t, got, 1)
		assert.Equal(t, nested(), got[0])
	})

	t.Run("a nested value read back and changed is not changed in the log", func(t *testing.T) {
		m := policy.NewMemoryRecorder()
		require.NoError(t, m.Record(t.Context(), nested()))

		got := m.Records()
		require.Len(t, got, 1)
		got[0].Action.Attrs["list"].([]any)[1].([]any)[0] = "changed"
		got[0].Action.Attrs["obj"].(map[string]any)["m"].(map[string]any)["deep"].([]int)[1] = 99
		(*got[0].Action.Attrs["pointer"].(*[]string))[1] = "changed"
		got[0].Action.Attrs["pstruct"].(*tagged).Tags[1] = "changed"
		got[0].Decision.Uncertain[0] = "changed"

		again := m.Records()
		require.Len(t, again, 1)
		assert.Equal(t, nested(), again[0])
	})

	t.Run("two reads share nothing", func(t *testing.T) {
		m := policy.NewMemoryRecorder()
		require.NoError(t, m.Record(t.Context(), nested()))
		a, b := m.Records(), m.Records()
		a[0].Action.Attrs["list"].([]any)[0] = "changed"
		assert.Equal(t, "x", b[0].Action.Attrs["list"].([]any)[0])
	})
}

// A value that refers to itself cannot be copied to the end, so it is copied to
// a bound and the rest is shared: Record returns, instead of looping.
func TestMemoryRecorder_AValueThatRefersToItselfIsCopiedToABound(t *testing.T) {
	self := map[string]any{"name": "loop"}
	self["self"] = self
	rec := record("send")
	rec.Action.Attrs = map[string]any{"loop": self}

	m := policy.NewMemoryRecorder()
	require.NoError(t, m.Record(t.Context(), rec))
	got := m.Records()
	require.Len(t, got, 1)
	loop, ok := got[0].Action.Attrs["loop"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "loop", loop["name"])

	// Following the copy's own reference to itself, it comes back to the
	// original after the bound and not after a million steps.
	var current any = loop
	steps := 0
	for ; steps < 200; steps++ {
		next, ok := current.(map[string]any)
		require.True(t, ok)
		if reflect.ValueOf(next).Pointer() == reflect.ValueOf(self).Pointer() {
			break
		}
		current = next["self"]
	}
	assert.Greater(t, steps, 1, "the copy is a copy")
	assert.Less(t, steps, 200, "and it ends")
}

// A MemoryRecorder that was declared and not built with NewMemoryRecorder works,
// and keeps what a built one does.
func TestMemoryRecorder_TheZeroValueWorks(t *testing.T) {
	t.Run("a declared recorder", func(t *testing.T) {
		var m policy.MemoryRecorder
		assert.Empty(t, m.Records())
		require.NoError(t, m.Record(t.Context(), record("read")))
		require.NoError(t, m.Record(t.Context(), record("send")))
		assert.Equal(t, []policy.Record{record("read"), record("send")}, m.Records())
	})

	t.Run("a pointer to an empty one", func(t *testing.T) {
		m := &policy.MemoryRecorder{}
		require.NoError(t, m.Record(t.Context(), record("read")))
		assert.Equal(t, []policy.Record{record("read")}, m.Records())
	})

	t.Run("it keeps the most recent 1000, as a built one does", func(t *testing.T) {
		var m policy.MemoryRecorder
		for i := range 1250 {
			require.NoError(t, m.Record(t.Context(), policy.Record{Action: policy.Action{Kind: strconv.Itoa(i)}}))
		}
		got := m.Records()
		require.Len(t, got, 1000)
		assert.Equal(t, "250", got[0].Action.Kind)
		assert.Equal(t, "1249", got[999].Action.Kind)
	})

	t.Run("a Decider over it decides and records", func(t *testing.T) {
		d, err := policy.NewDecider(policy.Policy{Default: policy.Allow}, policy.Options{Recorder: &policy.MemoryRecorder{}})
		require.NoError(t, err)
		got, err := d.Decide(t.Context(), policy.Action{Kind: "read"})
		require.NoError(t, err)
		assert.Equal(t, policy.Allow, got.Effect)
	})

	t.Run("a Decider over it keeps what it recorded", func(t *testing.T) {
		m := &policy.MemoryRecorder{}
		d, err := policy.NewDecider(policy.Policy{Default: policy.Allow}, policy.Options{Recorder: m})
		require.NoError(t, err)
		for _, k := range []string{"read", "write", "send"} {
			_, err := d.Decide(t.Context(), policy.Action{Kind: k})
			require.NoError(t, err)
		}
		got := m.Records()
		require.Len(t, got, 3)
		assert.Equal(t, "write", got[1].Action.Kind)
	})

	t.Run("first records from many goroutines at once", func(t *testing.T) {
		m := &policy.MemoryRecorder{}
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				assert.NoError(t, m.Record(t.Context(), policy.Record{Action: policy.Action{Kind: strconv.Itoa(i)}}))
			}()
		}
		wg.Wait()
		assert.Len(t, m.Records(), 16)
	})
}

// A record too large to copy is refused, not stored with its inside shared.
func TestMemoryRecorder_RefusesARecordTooLargeToCopy(t *testing.T) {
	m := policy.NewMemoryRecorder()
	rec := record("send")
	rec.Action.Attrs = map[string]any{"deep": sharedChild(20)}

	started := time.Now()
	err := m.Record(t.Context(), rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "10000")
	assert.ErrorIs(t, err, policy.ErrUnrecordable, "no retry will make it smaller")
	assert.Less(t, time.Since(started), 2*time.Second, "the work is bounded, not only the depth")
	assert.Empty(t, m.Records())

	t.Run("and one at the bound is not", func(t *testing.T) {
		rec := record("send")
		rec.Action.Attrs = map[string]any{"list": make([]any, 4000)}
		require.NoError(t, m.Record(t.Context(), rec))
		assert.Len(t, m.Records(), 1)
	})
}

// sharedChild is a value in which every level holds the same child twice, so
// that copying it value by value doubles at every level.
func sharedChild(levels int) any {
	var v any = []any{"leaf"}
	for range levels {
		v = []any{v, v}
	}
	return v
}

func TestMemoryRecorder_IsSafeForConcurrentUse(t *testing.T) {
	const writers, each = 16, 50
	m := policy.NewMemoryRecorder()

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range each {
				rec := record(fmt.Sprintf("w%d-%d", w, i))
				assert.NoError(t, m.Record(t.Context(), rec))
			}
		}()
		go func() {
			defer wg.Done()
			for range each {
				_ = m.Records()
			}
		}()
	}
	wg.Wait()

	got := m.Records()
	require.Len(t, got, writers*each)

	// Each writer's own records are in the order it wrote them.
	next := make([]int, writers)
	for _, rec := range got {
		var w, i int
		_, err := fmt.Sscanf(rec.Action.Kind, "w%d-%d", &w, &i)
		require.NoError(t, err)
		assert.Equal(t, next[w], i, "writer %d", w)
		next[w] = i + 1
	}
}

func TestRecord_JSON(t *testing.T) {
	tests := []struct {
		name string
		rec  policy.Record
		want string
	}{
		{
			name: "every field",
			rec:  record("send"),
			want: `{"at":"2026-10-02T09:00:00Z","action":{"kind":"send","target":"t:send","attrs":{"external":true}},"decision":{"decision":"ask","rule":"r","index":2,"matched":["r","s"]},"version":"v1"}`,
		},
		{
			name: "a decision reached on something that could not be told",
			rec: policy.Record{
				At:       recordTime,
				Action:   policy.Action{Kind: "pay", Attrs: map[string]any{"amount": "1250"}},
				Decision: policy.Decision{Effect: policy.Ask, Rule: "r", Index: 0, Matched: []string{"r"}, Uncertain: []string{"amount"}},
			},
			want: `{"at":"2026-10-02T09:00:00Z","action":{"kind":"pay","attrs":{"amount":"1250"}},"decision":{"decision":"ask","rule":"r","index":0,"matched":["r"],"uncertain":["amount"]}}`,
		},
		{
			name: "the id a store gives a record it lists",
			rec: policy.Record{
				ID:       7,
				At:       recordTime,
				Action:   policy.Action{Kind: "read"},
				Decision: policy.Decision{Effect: policy.Allow, Rule: "r", Index: 0},
			},
			want: `{"id":7,"at":"2026-10-02T09:00:00Z","action":{"kind":"read"},"decision":{"decision":"allow","rule":"r","index":0}}`,
		},
		{
			name: "no version",
			rec: policy.Record{
				At:       recordTime,
				Action:   policy.Action{Kind: "read"},
				Decision: policy.Decision{Effect: policy.Block, Rule: policy.RuleDefault, Index: -1},
			},
			want: `{"at":"2026-10-02T09:00:00Z","action":{"kind":"read"},"decision":{"decision":"block","rule":"no rule matched","index":-1}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.rec)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestMemoryRecorder_KeepsNoIDOfItsOwn(t *testing.T) {
	// A record is handed to a recorder without an id that means anything, and the
	// in-memory one has none to give: what it returns carries none, whatever it
	// was handed.
	m := policy.NewMemoryRecorder()
	for _, id := range []int64{0, 9999, -3} {
		rec := record("send")
		rec.ID = id
		require.NoError(t, m.Record(t.Context(), rec))
	}
	got := m.Records()
	require.Len(t, got, 3)
	for _, rec := range got {
		assert.Zero(t, rec.ID)
	}
}

func TestErrUnrecordable(t *testing.T) {
	assert.Contains(t, policy.ErrUnrecordable.Error(), "policy:")
	assert.NotErrorIs(t, errors.New("disk full"), policy.ErrUnrecordable)
	assert.ErrorIs(t, fmt.Errorf("store: record: %w", policy.ErrUnrecordable), policy.ErrUnrecordable)
}
