package llm_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// meterClock returns the times it was built with, one per reading. A Metered
// reads the clock once as a call starts and once as it ends.
func meterClock(times ...time.Time) func() time.Time {
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if len(times) == 0 {
			return time.Time{}
		}
		t := times[0]
		times = times[1:]
		return t
	}
}

// recorder collects the records a Metered emits.
type recorder struct {
	mu      sync.Mutex
	records []llm.CallRecord
}

func (r *recorder) record(_ context.Context, c llm.CallRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, c)
}

func (r *recorder) all() []llm.CallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.CallRecord(nil), r.records...)
}

func TestMetered_Generate(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	used := llm.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 50}
	boom := errors.New("transport down")
	attempts := fakeAnswer("model-b", llm.Usage{InputTokens: 400, OutputTokens: 100})
	attempts.resp.Attempts = []llm.Attempt{
		{Model: "model-a", Usage: llm.Usage{InputTokens: 600, OutputTokens: 10}},
		{Model: "model-b", Usage: llm.Usage{InputTokens: 400, OutputTokens: 100}},
	}
	partlyListed := fakeAnswer("model-b", llm.Usage{InputTokens: 400, OutputTokens: 100})
	partlyListed.resp.Attempts = []llm.Attempt{
		{Model: "model-a", Usage: llm.Usage{InputTokens: 600, OutputTokens: 10}},
		{Model: "model-z", Usage: llm.Usage{InputTokens: 400, OutputTokens: 100}},
	}
	noModel := fakeAnswer("", used)

	tests := []struct {
		name   string
		prices llm.Prices
		step   fakeStep
		req    llm.Request
		want   llm.CallRecord
	}{
		{
			name:   "a priced answer",
			prices: wrapperPrices(),
			step:   fakeAnswer("model-a", used),
			req:    fakeRequest("model-a"),
			want: llm.CallRecord{
				Model: "model-a", Usage: used, Stop: llm.StopEnd, Priced: true,
				CostMicros: mustCost(t, "model-a", used),
			},
		},
		{
			name:   "a model the table lacks is counted in tokens and not priced",
			prices: wrapperPrices(),
			step:   fakeAnswer("model-z", used),
			req:    fakeRequest("model-z"),
			want:   llm.CallRecord{Model: "model-z", Usage: used, Stop: llm.StopEnd},
		},
		{
			name: "with no table nothing is priced",
			step: fakeAnswer("model-a", used),
			req:  fakeRequest("model-a"),
			want: llm.CallRecord{Model: "model-a", Usage: used, Stop: llm.StopEnd},
		},
		{
			name:   "the model that answered, not the one asked for",
			prices: wrapperPrices(),
			step:   fakeAnswer("model-b", used),
			req:    fakeRequest("model-a"),
			want: llm.CallRecord{
				Model: "model-b", Usage: used, Stop: llm.StopEnd, Priced: true,
				CostMicros: mustCost(t, "model-b", used),
			},
		},
		{
			name:   "a reply that names no model is recorded, and priced, as the one asked for",
			prices: wrapperPrices(),
			step:   noModel,
			req:    fakeRequest("model-a"),
			want: llm.CallRecord{
				Model: "model-a", Usage: used, Stop: llm.StopEnd, Priced: true,
				CostMicros: mustCost(t, "model-a", used),
			},
		},
		{
			name:   "a dated id the table lacks is priced at the model asked for",
			prices: wrapperPrices(),
			step:   fakeAnswer("model-a-20251001", used),
			req:    fakeRequest("model-a"),
			want: llm.CallRecord{
				Model: "model-a-20251001", Usage: used, Stop: llm.StopEnd, Priced: true,
				CostMicros: mustCost(t, "model-a", used),
			},
		},
		{
			name:   "a reply that names no model, to a request that named none, cannot be priced",
			prices: wrapperPrices(),
			step:   noModel,
			req:    fakeRequest(""),
			want:   llm.CallRecord{Usage: used, Stop: llm.StopEnd},
		},
		{
			name:   "a refusal is a reply: billed, with its stop reason",
			prices: wrapperPrices(),
			step:   fakeRefusal("model-a", used),
			req:    fakeRequest("model-a"),
			want: llm.CallRecord{
				Model: "model-a", Usage: used, Stop: llm.StopRefusal, Priced: true,
				CostMicros: mustCost(t, "model-a", used),
			},
		},
		{
			name:   "every attempt is counted and priced by its own model",
			prices: wrapperPrices(),
			step:   attempts,
			req:    fakeRequest("model-a"),
			want: llm.CallRecord{
				Model: "model-b", Stop: llm.StopEnd, Priced: true,
				Usage: llm.Usage{InputTokens: 1000, OutputTokens: 110},
				CostMicros: mustCost(t, "model-a", llm.Usage{InputTokens: 600, OutputTokens: 10}) +
					mustCost(t, "model-b", llm.Usage{InputTokens: 400, OutputTokens: 100}),
			},
		},
		{
			name:   "an attempt on a model the table lacks is priced at the model asked for",
			prices: wrapperPrices(),
			step:   partlyListed,
			req:    fakeRequest("model-a"),
			want: llm.CallRecord{
				Model: "model-b", Stop: llm.StopEnd, Priced: true,
				Usage: llm.Usage{InputTokens: 1000, OutputTokens: 110},
				CostMicros: mustCost(t, "model-a", llm.Usage{InputTokens: 600, OutputTokens: 10}) +
					mustCost(t, "model-a", llm.Usage{InputTokens: 400, OutputTokens: 100}),
			},
		},
		{
			name:   "and with no model asked for, one attempt on a model the table lacks leaves the call not priced",
			prices: wrapperPrices(),
			step:   partlyListed,
			req:    fakeRequest(""),
			want: llm.CallRecord{
				Model: "model-b", Stop: llm.StopEnd,
				Usage: llm.Usage{InputTokens: 1000, OutputTokens: 110},
			},
		},
		{
			name:   "a failed call records the model asked for and the error",
			prices: wrapperPrices(),
			step:   fakeStep{err: boom},
			req:    fakeRequest("model-a"),
			want:   llm.CallRecord{Model: "model-a", Err: boom},
		},
		{
			name:   "a failed call that named no model records none",
			prices: wrapperPrices(),
			step:   fakeStep{err: boom},
			req:    fakeRequest(""),
			want:   llm.CallRecord{Err: boom},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := newFakeModel(tt.step)
			var rec recorder
			m := llm.NewMetered(inner, llm.MeterOptions{
				Prices: tt.prices,
				Record: rec.record,
				Now:    meterClock(t0, t0.Add(1500*time.Millisecond)),
			})

			resp, err := m.Generate(context.Background(), tt.req)

			if tt.want.Err != nil {
				require.ErrorIs(t, err, tt.want.Err)
				assert.Nil(t, resp)
			} else {
				require.NoError(t, err)
				assert.Same(t, tt.step.resp, resp, "the reply is passed on as it came")
			}
			tt.want.At = t0
			tt.want.Duration = 1500 * time.Millisecond
			assert.Equal(t, []llm.CallRecord{tt.want}, rec.all(), "one record per call, whether or not it failed")
		})
	}
}

func TestMetered_ReturnsTheModelsErrorAsItIs(t *testing.T) {
	failure := fakePermanent()
	m := llm.NewMetered(newFakeModel(fakeStep{err: failure}), llm.MeterOptions{})

	_, err := m.Generate(context.Background(), fakeRequest(""))

	assert.Same(t, failure, err)
}

func TestMetered_Totals(t *testing.T) {
	var rec recorder
	m := llm.NewMetered(newFakeModel(
		fakeAnswer("model-a", llm.Usage{InputTokens: 1000, OutputTokens: 200}),
		fakeAnswer("model-z", llm.Usage{InputTokens: 300, OutputTokens: 40}),
		fakeStep{err: fakeTransient(0)},
		fakeRefusal("model-b", llm.Usage{InputTokens: 80, OutputTokens: 5}),
	), llm.MeterOptions{Prices: wrapperPrices(), Record: rec.record})
	require.Equal(t, llm.Spend{}, m.Totals(), "nothing before the first call")

	for _, model := range []string{"model-a", "model-z", "model-a", "model-b"} {
		_, _ = m.Generate(context.Background(), fakeRequest(model))
	}

	want := llm.Spend{
		// A failed call is a call, and used no tokens and cost nothing.
		Calls:  4,
		Tokens: 1200 + 340 + 85,
		// model-z has no price, so its tokens are counted and its cost is not.
		CostMicros: mustCost(t, "model-a", llm.Usage{InputTokens: 1000, OutputTokens: 200}) +
			mustCost(t, "model-b", llm.Usage{InputTokens: 80, OutputTokens: 5}),
	}
	assert.Equal(t, want, m.Totals())
	var sum llm.Spend
	for _, r := range rec.all() {
		sum.Calls++
		sum.Tokens += r.Usage.Total()
		sum.CostMicros += r.CostMicros
	}
	assert.Equal(t, want, sum, "the totals are the sum of the records")
}

func TestMetered_TotalsAreKeptWithNoRecorder(t *testing.T) {
	m := llm.NewMetered(newFakeModel(fakeAnswer("model-a", llm.Usage{InputTokens: 10, OutputTokens: 5})), llm.MeterOptions{})

	_, err := m.Generate(context.Background(), fakeRequest("model-a"))

	require.NoError(t, err)
	assert.Equal(t, llm.Spend{Calls: 1, Tokens: 15}, m.Totals())
}

func TestMetered_Stream(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	used := llm.Usage{InputTokens: 70, OutputTokens: 30}
	errStop := errors.New("caller stopped")

	t.Run("a stream is one record, made when it ends", func(t *testing.T) {
		step := fakeAnswer("model-a", used)
		step.deltas = fakeDeltas()
		var rec recorder
		var seenAtDelta []int
		m := llm.NewMetered(newFakeModel(step), llm.MeterOptions{
			Prices: wrapperPrices(), Record: rec.record,
			Now: meterClock(t0, t0.Add(3*time.Second)),
		})
		var got []llm.Delta

		resp, err := m.Stream(context.Background(), fakeRequest("model-a"), func(d llm.Delta) error {
			seenAtDelta = append(seenAtDelta, len(rec.all()))
			got = append(got, d)
			return nil
		})

		require.NoError(t, err)
		assert.Same(t, step.resp, resp)
		assert.Equal(t, fakeDeltas(), got, "every delta is passed on")
		assert.Equal(t, []int{0, 0}, seenAtDelta, "nothing is recorded while the stream is still open")
		assert.Equal(t, []llm.CallRecord{{
			At: t0, Duration: 3 * time.Second, Model: "model-a", Usage: used, Stop: llm.StopEnd,
			Priced: true, CostMicros: mustCost(t, "model-a", used),
		}}, rec.all(), "Duration covers the whole stream")
		assert.Equal(t, llm.Spend{Calls: 1, Tokens: 100, CostMicros: mustCost(t, "model-a", used)}, m.Totals())
	})

	t.Run("a stream that fails after deltas is recorded with its error and no usage", func(t *testing.T) {
		failure := fakeTransient(0)
		var rec recorder
		m := llm.NewMetered(newFakeModel(fakeStep{deltas: fakeDeltas()[:1], err: failure}), llm.MeterOptions{
			Prices: wrapperPrices(), Record: rec.record,
			Now: meterClock(t0, t0.Add(time.Second)),
		})
		var got []llm.Delta

		resp, err := m.Stream(context.Background(), fakeRequest("model-a"), collectDeltas(&got))

		require.ErrorIs(t, err, failure)
		assert.Nil(t, resp)
		assert.Len(t, got, 1)
		assert.Equal(t, []llm.CallRecord{{At: t0, Duration: time.Second, Model: "model-a", Err: failure}}, rec.all())
		assert.Equal(t, llm.Spend{Calls: 1}, m.Totals())
	})

	t.Run("an error from the callback is returned and recorded", func(t *testing.T) {
		step := fakeAnswer("model-a", used)
		step.deltas = fakeDeltas()
		var rec recorder
		m := llm.NewMetered(newFakeModel(step), llm.MeterOptions{Record: rec.record})

		_, err := m.Stream(context.Background(), fakeRequest("model-a"), func(llm.Delta) error { return errStop })

		require.ErrorIs(t, err, errStop)
		records := rec.all()
		require.Len(t, records, 1)
		assert.ErrorIs(t, records[0].Err, errStop)
	})
}

func TestMetered_ANilReplyIsRecordedAsAFailedCall(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream %v", stream), func(t *testing.T) {
			var rec recorder
			m := llm.NewMetered(newFakeModel(fakeStep{}), llm.MeterOptions{
				Prices: wrapperPrices(), Record: rec.record, Now: meterClock(t0, t0.Add(time.Second)),
			})

			var resp *llm.Response
			var err error
			if stream {
				resp, err = m.Stream(context.Background(), fakeRequest("model-a"), func(llm.Delta) error { return nil })
			} else {
				resp, err = m.Generate(context.Background(), fakeRequest("model-a"))
			}

			assert.Nil(t, resp)
			assert.ErrorContains(t, err, noReplyMessage)
			records := rec.all()
			require.Len(t, records, 1, "it was a call, and it failed")
			assert.Equal(t, "model-a", records[0].Model)
			assert.Equal(t, err, records[0].Err)
			assert.Equal(t, llm.Spend{Calls: 1}, m.Totals())
		})
	}
}

// The callback's error is the caller's own signal to stop. It comes back as
// the caller returned it even when the provider wrapped it, and the record of
// the call carries that same error.
func TestMetered_TheCallbacksErrorComesBackUntouched(t *testing.T) {
	errStop := errors.New("caller stopped")
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("provider wraps it: %v", wrapped), func(t *testing.T) {
			step := fakeAnswer("model-a", llm.Usage{InputTokens: 3})
			step.deltas = fakeDeltas()
			step.wrapCallbackError = wrapped
			var rec recorder
			m := llm.NewMetered(newFakeModel(step), llm.MeterOptions{Record: rec.record})

			resp, err := m.Stream(context.Background(), fakeRequest("model-a"), func(llm.Delta) error { return errStop })

			assert.Nil(t, resp)
			assert.Same(t, errStop, err)
			records := rec.all()
			require.Len(t, records, 1)
			assert.Same(t, errStop, records[0].Err)
		})
	}
}

func TestMetered_NowDefaultsToTimeNow(t *testing.T) {
	step := fakeAnswer("model-a", llm.Usage{InputTokens: 1})
	step.hold = 20 * time.Millisecond
	var rec recorder
	m := llm.NewMetered(newFakeModel(step), llm.MeterOptions{Record: rec.record})

	before := time.Now()
	_, err := m.Generate(context.Background(), fakeRequest("model-a"))
	after := time.Now()

	require.NoError(t, err)
	records := rec.all()
	require.Len(t, records, 1)
	assert.False(t, records[0].At.Before(before), "At is when the call started")
	assert.False(t, records[0].At.After(after))
	assert.GreaterOrEqual(t, records[0].Duration, 20*time.Millisecond, "Duration is how long the call took")
	assert.LessOrEqual(t, records[0].Duration, after.Sub(before))
}

func TestMetered_RecordGetsTheCallsContext(t *testing.T) {
	type key struct{}
	var got any
	m := llm.NewMetered(newFakeModel(fakeAnswer("model-a", llm.Usage{})), llm.MeterOptions{
		Record: func(ctx context.Context, _ llm.CallRecord) { got = ctx.Value(key{}) },
	})

	_, err := m.Generate(context.WithValue(context.Background(), key{}, "trace-1"), fakeRequest("model-a"))

	require.NoError(t, err)
	assert.Equal(t, "trace-1", got)
}

func TestMetered_RecordMayReadTotals(t *testing.T) {
	// Record runs with no lock held, so a recorder that reads Totals, or makes
	// a call of its own through the same Metered, does not deadlock it.
	done := make(chan llm.Spend, 1)
	var m *llm.Metered
	m = llm.NewMetered(newFakeModel(fakeAnswer("model-a", llm.Usage{InputTokens: 4})), llm.MeterOptions{
		Record: func(context.Context, llm.CallRecord) { done <- m.Totals() },
	})

	go func() { _, _ = m.Generate(context.Background(), fakeRequest("model-a")) }()

	select {
	case totals := <-done:
		assert.EqualValues(t, 4, totals.Tokens)
	case <-time.After(5 * time.Second):
		t.Fatal("Record deadlocked against Totals")
	}
}

func TestMetered_ConcurrentCallsAreAllCounted(t *testing.T) {
	const calls = 50
	used := llm.Usage{InputTokens: 10, OutputTokens: 5}
	inner := newFakeModelFunc(func(call int, _ llm.Request) fakeStep {
		s := fakeAnswer("model-a", used)
		if call%5 == 0 {
			s = fakeStep{err: fakeTransient(0)}
		}
		s.hold = time.Millisecond
		return s
	})
	var rec recorder
	m := llm.NewMetered(inner, llm.MeterOptions{Prices: wrapperPrices(), Record: rec.record})

	var wg sync.WaitGroup
	for range calls {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = m.Generate(context.Background(), fakeRequest("model-a"))
		}()
		go func() {
			defer wg.Done()
			_ = m.Totals()
		}()
	}
	wg.Wait()

	const failed = calls / 5
	assert.Len(t, rec.all(), calls)
	assert.Equal(t, llm.Spend{
		Calls:      calls,
		Tokens:     (calls - failed) * used.Total(),
		CostMicros: (calls - failed) * mustCost(t, "model-a", used),
	}, m.Totals())
}

// TestWrappers_ComposeInTheDocumentedOrder builds the stack the package
// documents, outermost first: Budgeted, Metered, Fallback, then one Retrying
// around each provider.
func TestWrappers_ComposeInTheDocumentedOrder(t *testing.T) {
	req := fakeRequest("")
	req.MaxTokens = 100
	used := llm.Usage{InputTokens: 40, OutputTokens: 10}
	// The first provider is down for good; the second answers.
	down := newFakeModelFunc(func(int, llm.Request) fakeStep { return fakeStep{err: fakeTransient(0)} })
	up := newFakeModel(fakeAnswer("model-b", used))
	retryOpts := llm.RetryOptions{Retry: fastBackoff}
	chain := newFallback(t, llm.FallbackOptions{}, llm.NewRetrying(down, retryOpts), llm.NewRetrying(up, retryOpts))

	build := func(t *testing.T, limit int64) (*llm.Budgeted, *llm.Metered, *recorder) {
		t.Helper()
		var rec recorder
		meter := llm.NewMetered(chain, llm.MeterOptions{Prices: wrapperPrices(), Record: rec.record})
		budget, err := llm.NewBudgeted(meter, llm.BudgetOptions{MaxTokens: limit, Prices: wrapperPrices(), Model: "model-a"})
		require.NoError(t, err)
		return budget, meter, &rec
	}

	t.Run("each provider retries before the chain moves on, and the budget and meter see one call", func(t *testing.T) {
		budget, meter, rec := build(t, 1_000_000)

		resp, err := budget.Generate(context.Background(), req)

		require.NoError(t, err)
		assert.Equal(t, "model-b", resp.Model)
		assert.Equal(t, 3, down.Calls(), "the first provider is tried three times first")
		assert.Equal(t, 1, up.Calls())
		want := llm.Spend{Calls: 1, Tokens: used.Total(), CostMicros: mustCost(t, "model-b", used)}
		assert.Equal(t, want, budget.Spent(), "one call to the budget however many attempts it took")
		assert.Equal(t, want, meter.Totals(), "and one to the meter")
		records := rec.all()
		require.Len(t, records, 1)
		assert.Equal(t, "model-b", records[0].Model)
		assert.True(t, records[0].Priced)
	})

	t.Run("a call the budget refuses reaches no provider and the meter never sees it", func(t *testing.T) {
		budget, meter, rec := build(t, 1)
		downBefore, upBefore := down.Calls(), up.Calls()

		resp, err := budget.Generate(context.Background(), req)

		require.ErrorIs(t, err, llm.ErrBudgetExceeded)
		assert.Nil(t, resp)
		assert.Equal(t, downBefore, down.Calls())
		assert.Equal(t, upBefore, up.Calls())
		assert.Equal(t, llm.Spend{}, budget.Spent())
		assert.Empty(t, rec.all(), "the meter records what the budget let through, and this was not")
		assert.Equal(t, llm.Spend{}, meter.Totals())
	})
}
