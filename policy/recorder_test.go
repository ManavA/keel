package policy_test

import (
	"encoding/json"
	"fmt"
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
