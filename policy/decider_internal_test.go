package policy

// This file is in package policy, not policy_test, for the cases that need the
// Decider's own fields: with the zero Options the recorder is not reachable
// from outside, and what it keeps is bounded.

import (
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecider_ZeroOptionsRecordInMemory(t *testing.T) {
	d, err := NewDecider(Policy{Version: "2026-10-02", Default: Allow, Rules: []Rule{{Name: "sends ask", Effect: Ask, When: Match{Kinds: []string{"send"}}}}}, Options{})
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

// The log a Decider keeps for itself cannot be read by anyone, so it must not
// grow without end.
func TestDecider_TheDefaultRecorderKeepsTheMostRecentDecisions(t *testing.T) {
	t.Run("a bound of 1000 by default", func(t *testing.T) {
		d, err := NewDecider(Policy{Default: Allow}, Options{})
		require.NoError(t, err)
		mem := d.rec.(*MemoryRecorder)
		assert.Equal(t, 1000, mem.keep)
		assert.Equal(t, defaultMemoryRecords, mem.keep)
	})

	t.Run("the bound is set through the options", func(t *testing.T) {
		d, err := NewDecider(Policy{Default: Allow}, Options{MemoryRecords: 3})
		require.NoError(t, err)
		for i := range 8 {
			_, err := d.Decide(t.Context(), Action{Kind: "k" + strconv.Itoa(i)})
			require.NoError(t, err)
		}
		recs := d.rec.(*MemoryRecorder).Records()
		require.Len(t, recs, 3)
		assert.Equal(t, []string{"k5", "k6", "k7"}, kinds(recs), "the most recent, oldest first")
	})

	t.Run("zero or less is the default", func(t *testing.T) {
		for _, n := range []int{0, -1, -1000} {
			d, err := NewDecider(Policy{Default: Allow}, Options{MemoryRecords: n})
			require.NoError(t, err)
			assert.Equal(t, defaultMemoryRecords, d.rec.(*MemoryRecorder).keep, "MemoryRecords %d", n)
		}
	})

	t.Run("the bound has no effect on a recorder the caller supplies", func(t *testing.T) {
		mem := NewMemoryRecorder()
		d, err := NewDecider(Policy{Default: Allow}, Options{Recorder: mem, MemoryRecords: 1})
		require.NoError(t, err)
		for range 5 {
			_, err := d.Decide(t.Context(), Action{Kind: "k"})
			require.NoError(t, err)
		}
		assert.Len(t, mem.Records(), 5)
	})
}

func TestMemoryRecorder_KeepsTheMostRecentRecords(t *testing.T) {
	tests := []struct {
		name    string
		keep    int
		records int
		want    []string
	}{
		{name: "fewer than the bound", keep: 5, records: 3, want: []string{"k0", "k1", "k2"}},
		{name: "exactly the bound", keep: 3, records: 3, want: []string{"k0", "k1", "k2"}},
		{name: "one over", keep: 3, records: 4, want: []string{"k1", "k2", "k3"}},
		{name: "a whole turn over", keep: 3, records: 6, want: []string{"k3", "k4", "k5"}},
		{name: "a turn and a half over", keep: 4, records: 10, want: []string{"k6", "k7", "k8", "k9"}},
		{name: "a bound of one", keep: 1, records: 5, want: []string{"k4"}},
		{name: "none recorded", keep: 3, records: 0, want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMemoryRecorder(tt.keep)
			for i := range tt.records {
				require.NoError(t, m.Record(t.Context(), Record{Action: Action{Kind: "k" + strconv.Itoa(i)}}))
			}
			assert.Equal(t, tt.want, kinds(m.Records()))
			assert.LessOrEqual(t, len(m.recs), tt.keep, "what is held is bounded, not only what is read")
		})
	}

	t.Run("reading does not disturb what comes next", func(t *testing.T) {
		m := newMemoryRecorder(3)
		for i := range 5 {
			require.NoError(t, m.Record(t.Context(), Record{Action: Action{Kind: "k" + strconv.Itoa(i)}}))
			_ = m.Records()
		}
		require.NoError(t, m.Record(t.Context(), Record{Action: Action{Kind: "k5"}}))
		assert.Equal(t, []string{"k3", "k4", "k5"}, kinds(m.Records()))
	})

	t.Run("the public constructor has the default bound", func(t *testing.T) {
		m := NewMemoryRecorder()
		for i := range defaultMemoryRecords + 250 {
			require.NoError(t, m.Record(t.Context(), Record{Action: Action{Kind: strconv.Itoa(i)}}))
		}
		got := m.Records()
		require.Len(t, got, defaultMemoryRecords)
		assert.Equal(t, "250", got[0].Action.Kind)
		assert.Equal(t, strconv.Itoa(defaultMemoryRecords+249), got[len(got)-1].Action.Kind)
	})
}

// A MemoryRecorder that was not built with a bound, and one built with none,
// keep the default number and do not panic.
func TestMemoryRecorder_ANonPositiveBoundIsTheDefault(t *testing.T) {
	for name, m := range map[string]*MemoryRecorder{
		"the zero value": {},
		"zero":           newMemoryRecorder(0),
		"negative":       newMemoryRecorder(-3),
	} {
		t.Run(name, func(t *testing.T) {
			for i := range defaultMemoryRecords + 5 {
				require.NoError(t, m.Record(t.Context(), Record{Action: Action{Kind: strconv.Itoa(i)}}))
			}
			got := m.Records()
			require.Len(t, got, defaultMemoryRecords)
			assert.Equal(t, "5", got[0].Action.Kind)
			assert.Equal(t, strconv.Itoa(defaultMemoryRecords+4), got[len(got)-1].Action.Kind)
			assert.LessOrEqual(t, len(m.recs), defaultMemoryRecords, "what is held is bounded")
		})
	}
}

func kinds(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Action.Kind
	}
	return out
}
