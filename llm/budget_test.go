package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// worstUsage is the most a request can use as the design counts it: its
// estimated input and the reply bound maxOut.
func worstUsage(req llm.Request, maxOut int) llm.Usage {
	return llm.Usage{InputTokens: llm.EstimateInputTokens(req), OutputTokens: int64(maxOut)}
}

// mustCost prices usage on model or fails the test.
func mustCost(t *testing.T, model string, u llm.Usage) int64 {
	t.Helper()
	c, err := wrapperPrices().Cost(model, u)
	require.NoError(t, err)
	return c
}

// roomFor returns budgets with room for n worst cases of req and no more:
// once by tokens and once by cost.
func roomFor(t *testing.T, req llm.Request, maxOut int, n int64) []struct {
	name string
	opts llm.BudgetOptions
} {
	t.Helper()
	worst := worstUsage(req, maxOut)
	return []struct {
		name string
		opts llm.BudgetOptions
	}{
		{name: "by tokens", opts: llm.BudgetOptions{MaxTokens: n * worst.Total()}},
		{name: "by cost", opts: llm.BudgetOptions{MaxCostMicros: n * mustCost(t, req.Model, worst), Prices: wrapperPrices()}},
	}
}

func TestNewBudgeted(t *testing.T) {
	tests := []struct {
		name    string
		opts    llm.BudgetOptions
		wantErr bool
	}{
		{name: "the zero value works", opts: llm.BudgetOptions{}},
		{name: "a token limit alone", opts: llm.BudgetOptions{MaxTokens: 1000}},
		{name: "a cost limit with prices", opts: llm.BudgetOptions{MaxCostMicros: 500, Prices: wrapperPrices()}},
		{name: "prices alone", opts: llm.BudgetOptions{Prices: wrapperPrices()}},
		{name: "a cost limit without prices is an error", opts: llm.BudgetOptions{MaxCostMicros: 500}, wantErr: true},
		{name: "a cost limit with an empty table is an error", opts: llm.BudgetOptions{MaxCostMicros: 500, Prices: llm.Prices{}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := llm.NewBudgeted(newFakeModel(), tt.opts)

			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, b)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, llm.Spend{}, b.Spent())
		})
	}
}

func TestBudgeted_Generate(t *testing.T) {
	const maxOut = 100
	req := fakeRequest("model-a")
	req.MaxTokens = maxOut
	worst := worstUsage(req, maxOut)
	worstTokens := worst.Total()
	worstCost := mustCost(t, "model-a", worst)
	real := llm.Usage{InputTokens: 30, OutputTokens: 20}
	realCost := mustCost(t, "model-a", real)

	unnamed := req
	unnamed.Model = ""
	unpriced := req
	unpriced.Model = "model-z"

	tests := []struct {
		name string
		opts llm.BudgetOptions
		req  llm.Request
		// wantCalls is 1 when the inner model must have been called.
		wantCalls int
		wantErr   error
		wantSpent llm.Spend
	}{
		{
			name:      "a call that fits goes through and Spent rises by the real usage",
			opts:      llm.BudgetOptions{MaxTokens: worstTokens},
			req:       req,
			wantCalls: 1,
			wantSpent: llm.Spend{Calls: 1, Tokens: real.Total()},
		},
		{
			name:    "a worst case one token over the limit is refused",
			opts:    llm.BudgetOptions{MaxTokens: worstTokens - 1},
			req:     req,
			wantErr: llm.ErrBudgetExceeded,
		},
		{
			name:      "by cost, a worst case that fits goes through and Spent holds its price",
			opts:      llm.BudgetOptions{MaxCostMicros: worstCost, Prices: wrapperPrices()},
			req:       req,
			wantCalls: 1,
			wantSpent: llm.Spend{Calls: 1, Tokens: real.Total(), CostMicros: realCost},
		},
		{
			name:    "by cost, a worst case one micro over the limit is refused",
			opts:    llm.BudgetOptions{MaxCostMicros: worstCost - 1, Prices: wrapperPrices()},
			req:     req,
			wantErr: llm.ErrBudgetExceeded,
		},
		{
			name:    "with both limits, the cost can refuse what the tokens allow",
			opts:    llm.BudgetOptions{MaxTokens: worstTokens * 10, MaxCostMicros: worstCost - 1, Prices: wrapperPrices()},
			req:     req,
			wantErr: llm.ErrBudgetExceeded,
		},
		{
			name:    "with both limits, the tokens can refuse what the cost allows",
			opts:    llm.BudgetOptions{MaxTokens: worstTokens - 1, MaxCostMicros: worstCost * 10, Prices: wrapperPrices()},
			req:     req,
			wantErr: llm.ErrBudgetExceeded,
		},
		{
			name:      "a request naming no model is priced as BudgetOptions.Model",
			opts:      llm.BudgetOptions{MaxCostMicros: mustCost(t, "model-b", worst), Prices: wrapperPrices(), Model: "model-b"},
			req:       unnamed,
			wantCalls: 1,
			wantSpent: llm.Spend{Calls: 1, Tokens: real.Total(), CostMicros: realCost},
		},
		{
			name:    "the same request is refused when the default model would cost more",
			opts:    llm.BudgetOptions{MaxCostMicros: mustCost(t, "model-b", worst), Prices: wrapperPrices(), Model: "model-a"},
			req:     unnamed,
			wantErr: llm.ErrBudgetExceeded,
		},
		{
			name:    "a request for a model the table does not list is ErrNoPrice",
			opts:    llm.BudgetOptions{MaxCostMicros: 1 << 40, Prices: wrapperPrices()},
			req:     unpriced,
			wantErr: llm.ErrNoPrice,
		},
		{
			name:    "a request naming no model, with no default, cannot be priced",
			opts:    llm.BudgetOptions{MaxCostMicros: 1 << 40, Prices: wrapperPrices()},
			req:     unnamed,
			wantErr: llm.ErrNoPrice,
		},
		{
			name:      "without a cost limit an unlisted model is not refused",
			opts:      llm.BudgetOptions{MaxTokens: worstTokens, Prices: wrapperPrices()},
			req:       unpriced,
			wantCalls: 1,
			wantSpent: llm.Spend{Calls: 1, Tokens: real.Total(), CostMicros: realCost},
		},
		{
			name:      "no limits at all lets everything through and still counts",
			opts:      llm.BudgetOptions{},
			req:       req,
			wantCalls: 1,
			wantSpent: llm.Spend{Calls: 1, Tokens: real.Total()},
		},
		{
			name:    "a negative limit refuses every call",
			opts:    llm.BudgetOptions{MaxTokens: -1},
			req:     req,
			wantErr: llm.ErrBudgetExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := newFakeModel(fakeAnswer("model-a", real))
			b, err := llm.NewBudgeted(inner, tt.opts)
			require.NoError(t, err)

			resp, err := b.Generate(context.Background(), tt.req)

			assert.Equal(t, tt.wantCalls, inner.Calls())
			assert.Equal(t, tt.wantSpent, b.Spent())
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, resp)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "model-a", resp.Model)
			assert.Equal(t, real, resp.Usage)
		})
	}
}

func TestBudgeted_ARefusalIsNotAFailureOfTheCall(t *testing.T) {
	// A refusal is a reply and is billed, so it counts against the budget.
	req := fakeRequest("model-a")
	req.MaxTokens = 50
	usage := llm.Usage{InputTokens: 12, OutputTokens: 3}
	b, err := llm.NewBudgeted(newFakeModel(fakeRefusal("model-a", usage)), llm.BudgetOptions{MaxTokens: 10_000})
	require.NoError(t, err)

	resp, err := b.Generate(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, llm.StopRefusal, resp.Stop)
	assert.Equal(t, llm.Spend{Calls: 1, Tokens: usage.Total()}, b.Spent())
}

func TestBudgeted_ReplayedTurnsCountAtTheirProviderForm(t *testing.T) {
	// A turn kept in its provider's form can be far longer than its text, and
	// the estimate counts it so; the budget must then refuse what a text-only
	// count would have let through.
	plain := fakeRequest("model-a")
	plain.MaxTokens = 100
	replayed := plain
	replayed.Messages = append(append([]llm.Message(nil), plain.Messages...), llm.Message{
		Role: llm.RoleAssistant, Text: "done",
		Opaque: &llm.Opaque{Provider: "fake", Data: json.RawMessage(`"` + strings.Repeat("x", 3000) + `"`)},
	})
	limit := worstUsage(plain, 100).Total() + 50
	require.Greater(t, worstUsage(replayed, 100).Total(), limit, "the fixture: the replayed turn does not fit the room")

	b, err := llm.NewBudgeted(newFakeModel(fakeAnswer("model-a", llm.Usage{InputTokens: 1})), llm.BudgetOptions{MaxTokens: limit})
	require.NoError(t, err)

	_, err = b.Generate(context.Background(), replayed)
	require.ErrorIs(t, err, llm.ErrBudgetExceeded)
	_, err = b.Generate(context.Background(), plain)
	require.NoError(t, err)
}

func TestBudgeted_RefusalMessageSaysWhy(t *testing.T) {
	b, err := llm.NewBudgeted(newFakeModel(), llm.BudgetOptions{MaxTokens: 10})
	require.NoError(t, err)

	_, err = b.Generate(context.Background(), fakeRequest("model-a"))

	require.ErrorIs(t, err, llm.ErrBudgetExceeded)
	assert.Contains(t, err.Error(), "10", "the limit is in the message")
	assert.NotContains(t, err.Error(), "summarise the batch", "the request's text is not")
}

func TestBudgeted_SpendingLeavesLessRoom(t *testing.T) {
	req := fakeRequest("model-a")
	req.MaxTokens = 100
	worstTokens := worstUsage(req, 100).Total()
	real := llm.Usage{InputTokens: 30, OutputTokens: 20}
	// Room for one worst case, and not for a second beside what the first used.
	inner := newFakeModelFunc(func(int, llm.Request) fakeStep { return fakeAnswer("model-a", real) })
	b, err := llm.NewBudgeted(inner, llm.BudgetOptions{MaxTokens: worstTokens + real.Total() - 1})
	require.NoError(t, err)

	_, err = b.Generate(context.Background(), req)
	require.NoError(t, err, "the first call fits an empty budget")
	spentAfterOne := b.Spent()

	resp, err := b.Generate(context.Background(), req)

	require.ErrorIs(t, err, llm.ErrBudgetExceeded, "the second no longer fits beside what the first used")
	assert.Nil(t, resp)
	assert.Equal(t, 1, inner.Calls())
	assert.Equal(t, spentAfterOne, b.Spent(), "a refused call changes nothing")
}

func TestBudgeted_NoCallStartsOnceTheBudgetIsSpent(t *testing.T) {
	req := fakeRequest("model-a")
	req.MaxTokens = 10
	// One call uses exactly the limit, so the budget is spent.
	real := llm.Usage{InputTokens: 60, OutputTokens: 40}
	inner := newFakeModelFunc(func(int, llm.Request) fakeStep { return fakeAnswer("model-a", real) })
	b, err := llm.NewBudgeted(inner, llm.BudgetOptions{MaxTokens: real.Total()})
	require.NoError(t, err)
	small := worstUsage(req, 10).Total()
	require.Less(t, small, real.Total(), "the fixture: a request whose worst case is far under what is left")

	_, err = b.Generate(context.Background(), req)
	require.NoError(t, err)
	_, err = b.Generate(context.Background(), req)

	require.ErrorIs(t, err, llm.ErrBudgetExceeded)
	assert.Equal(t, 1, inner.Calls())
	assert.Equal(t, real.Total(), b.Spent().Tokens)
}

func TestBudgeted_AReservationIsReleasedWhenTheCallEnds(t *testing.T) {
	req := fakeRequest("model-a")
	req.MaxTokens = 100
	tiny := llm.Usage{InputTokens: 1}
	// Room for two worst cases. A reservation that were kept would leave room
	// for one call, then none.
	for _, room := range roomFor(t, req, 100, 2) {
		t.Run(room.name, func(t *testing.T) {
			inner := newFakeModelFunc(func(int, llm.Request) fakeStep { return fakeAnswer("model-a", tiny) })
			b, err := llm.NewBudgeted(inner, room.opts)
			require.NoError(t, err)

			for i := range 4 {
				_, err := b.Generate(context.Background(), req)
				require.NoError(t, err, "call %d", i+1)
			}

			assert.Equal(t, int64(4), b.Spent().Calls)
			assert.Equal(t, int64(4), b.Spent().Tokens)
		})
	}
}

func TestBudgeted_AFailedCallAddsNothing(t *testing.T) {
	req := fakeRequest("model-a")
	req.MaxTokens = 100
	boom := errors.New("transport down")
	// A model that returns a partial reply along with its error: the error
	// decides, and the reply is not charged.
	partial := fakeAnswer("model-a", llm.Usage{InputTokens: 77, OutputTokens: 9})
	partial.err = fakeTransient(0)
	// Room for exactly one worst case, so a failure that kept its reservation,
	// or charged it, would refuse the last call.
	for _, room := range roomFor(t, req, 100, 1) {
		t.Run(room.name, func(t *testing.T) {
			inner := newFakeModel(
				fakeStep{err: boom},
				fakeStep{err: fakeTransient(0)},
				partial,
				fakeAnswer("model-a", llm.Usage{InputTokens: 5}),
			)
			b, err := llm.NewBudgeted(inner, room.opts)
			require.NoError(t, err)

			_, err = b.Generate(context.Background(), req)
			require.ErrorIs(t, err, boom, "the model's error is returned as it is")
			_, err = b.Generate(context.Background(), req)
			var le *llm.Error
			require.ErrorAs(t, err, &le)
			_, err = b.Generate(context.Background(), req)
			require.Error(t, err)
			assert.Equal(t, llm.Spend{}, b.Spent(), "three failures cost the budget nothing")

			_, err = b.Generate(context.Background(), req)

			require.NoError(t, err)
			assert.Equal(t, int64(1), b.Spent().Calls)
			assert.Equal(t, int64(5), b.Spent().Tokens)
		})
	}
}

func TestBudgeted_AReservationIsReleasedWhenTheModelPanics(t *testing.T) {
	req := fakeRequest("model-a")
	req.MaxTokens = 100
	worstTokens := worstUsage(req, 100).Total()
	inner := newFakeModel(
		fakeStep{panicWith: "model blew up"},
		fakeAnswer("model-a", llm.Usage{InputTokens: 5}),
	)
	b, err := llm.NewBudgeted(inner, llm.BudgetOptions{MaxTokens: worstTokens})
	require.NoError(t, err)

	assert.PanicsWithValue(t, "model blew up", func() { _, _ = b.Generate(context.Background(), req) })
	_, err = b.Generate(context.Background(), req)

	require.NoError(t, err, "a panic in the model must not leave the budget held for good")
	assert.Equal(t, llm.Spend{Calls: 1, Tokens: 5}, b.Spent())
}

func TestBudgeted_UsageOfEveryAttemptCounts(t *testing.T) {
	req := fakeRequest("model-a")
	req.MaxTokens = 100
	a := llm.Usage{InputTokens: 100, OutputTokens: 10}
	bUsage := llm.Usage{InputTokens: 50, OutputTokens: 70}
	answer := fakeAnswer("model-b", bUsage)
	answer.resp.Attempts = []llm.Attempt{{Model: "model-a", Usage: a}, {Model: "model-b", Usage: bUsage}}
	b, err := llm.NewBudgeted(newFakeModel(answer), llm.BudgetOptions{
		MaxCostMicros: 1 << 40, Prices: wrapperPrices(),
	})
	require.NoError(t, err)

	_, err = b.Generate(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, llm.Spend{
		Calls:      1,
		Tokens:     a.Total() + bUsage.Total(),
		CostMicros: mustCost(t, "model-a", a) + mustCost(t, "model-b", bUsage),
	}, b.Spent(), "a model that ran and was set aside was billed, and is counted")
}

func TestBudgeted_AReplyThePricesCannotPriceIsChargedTheWorstCase(t *testing.T) {
	// The request names model-a, which is priced; the reply names a model the
	// table has never heard of. The call has been made and cannot be priced,
	// so the budget charges what it reserved rather than nothing.
	req := fakeRequest("model-a")
	req.MaxTokens = 100
	real := llm.Usage{InputTokens: 30, OutputTokens: 20}
	b, err := llm.NewBudgeted(newFakeModel(fakeAnswer("model-z", real)), llm.BudgetOptions{
		MaxCostMicros: 1 << 40, Prices: wrapperPrices(),
	})
	require.NoError(t, err)

	_, err = b.Generate(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, llm.Spend{
		Calls:      1,
		Tokens:     real.Total(),
		CostMicros: mustCost(t, "model-a", worstUsage(req, 100)),
	}, b.Spent())
}

func TestBudgeted_DefaultMaxTokens(t *testing.T) {
	req := fakeRequest("model-a") // sets no MaxTokens
	est := llm.EstimateInputTokens(req)
	tests := []struct {
		name    string
		opts    llm.BudgetOptions
		wantErr bool
	}{
		{name: "16000 when the request and the options say nothing, which just fits", opts: llm.BudgetOptions{MaxTokens: est + 16_000}},
		{name: "and one under that is refused", opts: llm.BudgetOptions{MaxTokens: est + 15_999}, wantErr: true},
		{name: "the option replaces it, which just fits", opts: llm.BudgetOptions{MaxTokens: est + 500, DefaultMaxTokens: 500}},
		{name: "and one under the option is refused", opts: llm.BudgetOptions{MaxTokens: est + 499, DefaultMaxTokens: 500}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := newFakeModel(fakeAnswer("model-a", llm.Usage{InputTokens: 1}))
			b, err := llm.NewBudgeted(inner, tt.opts)
			require.NoError(t, err)

			_, err = b.Generate(context.Background(), req)

			if tt.wantErr {
				require.ErrorIs(t, err, llm.ErrBudgetExceeded)
				assert.Zero(t, inner.Calls())
				return
			}
			require.NoError(t, err)
		})
	}

	t.Run("a request's own MaxTokens beats the default", func(t *testing.T) {
		own := req
		own.MaxTokens = 10
		b, err := llm.NewBudgeted(newFakeModel(fakeAnswer("model-a", llm.Usage{InputTokens: 1})),
			llm.BudgetOptions{MaxTokens: est + 10})

		require.NoError(t, err)
		_, err = b.Generate(context.Background(), own)
		require.NoError(t, err)
	})
}

func TestBudgeted_Stream(t *testing.T) {
	req := fakeRequest("model-a")
	req.MaxTokens = 100
	worstTokens := worstUsage(req, 100).Total()
	real := llm.Usage{InputTokens: 30, OutputTokens: 20}
	errStop := errors.New("caller stopped")

	t.Run("a stream that fits is forwarded and counted", func(t *testing.T) {
		step := fakeAnswer("model-a", real)
		step.deltas = fakeDeltas()
		inner := newFakeModel(step)
		b, err := llm.NewBudgeted(inner, llm.BudgetOptions{MaxTokens: worstTokens})
		require.NoError(t, err)
		var got []llm.Delta

		resp, err := b.Stream(context.Background(), req, collectDeltas(&got))

		require.NoError(t, err)
		assert.Equal(t, fakeDeltas(), got)
		assert.Equal(t, real, resp.Usage)
		assert.Equal(t, llm.Spend{Calls: 1, Tokens: real.Total()}, b.Spent())
	})

	t.Run("a stream that does not fit is refused before anything is sent", func(t *testing.T) {
		inner := newFakeModel(fakeAnswer("model-a", real))
		b, err := llm.NewBudgeted(inner, llm.BudgetOptions{MaxTokens: worstTokens - 1})
		require.NoError(t, err)
		called := false

		resp, err := b.Stream(context.Background(), req, func(llm.Delta) error { called = true; return nil })

		require.ErrorIs(t, err, llm.ErrBudgetExceeded)
		assert.Nil(t, resp)
		assert.Zero(t, inner.Calls())
		assert.False(t, called, "the callback is never called")
	})

	t.Run("a stream that fails after deltas adds nothing and returns its reservation", func(t *testing.T) {
		inner := newFakeModel(
			fakeStep{deltas: fakeDeltas()[:1], err: fakeTransient(0)},
			fakeAnswer("model-a", real),
		)
		b, err := llm.NewBudgeted(inner, llm.BudgetOptions{MaxTokens: worstTokens})
		require.NoError(t, err)
		var got []llm.Delta

		_, err = b.Stream(context.Background(), req, collectDeltas(&got))

		var le *llm.Error
		require.ErrorAs(t, err, &le)
		assert.Len(t, got, 1)
		assert.Equal(t, llm.Spend{}, b.Spent())
		_, err = b.Stream(context.Background(), req, collectDeltas(&got))
		require.NoError(t, err, "the budget has its room back")
	})

	t.Run("an error from the callback is returned and adds nothing", func(t *testing.T) {
		step := fakeAnswer("model-a", real)
		step.deltas = fakeDeltas()
		inner := newFakeModel(step)
		b, err := llm.NewBudgeted(inner, llm.BudgetOptions{MaxTokens: worstTokens})
		require.NoError(t, err)

		resp, err := b.Stream(context.Background(), req, func(llm.Delta) error { return errStop })

		require.ErrorIs(t, err, errStop)
		assert.Nil(t, resp)
		assert.Equal(t, llm.Spend{}, b.Spent())
	})

	t.Run("a stream is priced like a call", func(t *testing.T) {
		inner := newFakeModel(fakeAnswer("model-z", real))
		b, err := llm.NewBudgeted(inner, llm.BudgetOptions{MaxCostMicros: 1 << 40, Prices: wrapperPrices()})
		require.NoError(t, err)
		unpriced := req
		unpriced.Model = "model-z"

		_, err = b.Stream(context.Background(), unpriced, func(llm.Delta) error { return nil })

		require.ErrorIs(t, err, llm.ErrNoPrice)
		assert.Zero(t, inner.Calls())
	})
}

// TestBudgeted_ConcurrentCallsNeverPassTheLimit is the case that must fail
// when the budget is wrong. Fifty calls start together against a budget that
// fits ten. A check made before the calls and settled after them lets all
// fifty through; the reservation is what keeps it to ten.
func TestBudgeted_ConcurrentCallsNeverPassTheLimit(t *testing.T) {
	const (
		calls  = 50
		fit    = 10
		maxOut = 1000
	)
	req := fakeRequest("model-a")
	req.MaxTokens = maxOut
	// A call uses exactly its worst case, so ten calls use exactly the limit
	// and an eleventh would pass it.
	worst := worstUsage(req, maxOut)
	perCall := worst.Total()
	perCallCost := mustCost(t, "model-a", worst)

	newInner := func() *fakeModel {
		// The hold keeps the calls open together, which is what lets a check
		// that does not reserve be fooled.
		return newFakeModelFunc(func(int, llm.Request) fakeStep {
			s := fakeAnswer("model-a", worst)
			s.hold = 50 * time.Millisecond
			return s
		})
	}

	// race starts calls goroutines at once and returns how many were let
	// through and refused, and the highest Spent any observer saw.
	race := func(t *testing.T, b *llm.Budgeted) (ok, refused int64, highest int64) {
		t.Helper()
		var okN, refusedN, high atomic.Int64
		start, done := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		for range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := b.Generate(context.Background(), req)
				switch {
				case err == nil:
					okN.Add(1)
				case errors.Is(err, llm.ErrBudgetExceeded):
					refusedN.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		var observer sync.WaitGroup
		observer.Add(1)
		go func() {
			defer observer.Done()
			for {
				select {
				case <-done:
					return
				default:
					if tokens := b.Spent().Tokens; tokens > high.Load() {
						high.Store(tokens)
					}
					runtime.Gosched()
				}
			}
		}()
		close(start)
		wg.Wait()
		close(done)
		observer.Wait()
		return okN.Load(), refusedN.Load(), high.Load()
	}

	t.Run("the same load with no budget does pass the limit", func(t *testing.T) {
		inner := newInner()
		b, err := llm.NewBudgeted(inner, llm.BudgetOptions{})
		require.NoError(t, err)

		ok, refused, _ := race(t, b)

		assert.EqualValues(t, calls, ok)
		assert.Zero(t, refused)
		assert.Greater(t, b.Spent().Tokens, int64(fit)*perCall, "the fixture can overspend: fifty calls use more than ten fit")
		assert.Greater(t, inner.Peak(), fit, "and the calls overlap enough for a check that does not reserve to be fooled")
	})

	tests := []struct {
		name string
		opts llm.BudgetOptions
	}{
		{name: "by tokens", opts: llm.BudgetOptions{MaxTokens: fit * perCall}},
		{name: "by cost", opts: llm.BudgetOptions{MaxCostMicros: fit * perCallCost, Prices: wrapperPrices()}},
		{name: "by both", opts: llm.BudgetOptions{MaxTokens: fit * perCall, MaxCostMicros: fit * perCallCost, Prices: wrapperPrices()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := newInner()
			b, err := llm.NewBudgeted(inner, tt.opts)
			require.NoError(t, err)

			ok, refused, highest := race(t, b)

			spent := b.Spent()
			assert.EqualValues(t, fit, ok, "exactly as many calls as fit are let through")
			assert.EqualValues(t, calls-fit, refused)
			assert.Equal(t, fit, inner.Calls(), "a refused call never reaches the model")
			assert.LessOrEqual(t, inner.Peak(), fit, "the reservations hold the calls in flight to what fits")
			assert.Equal(t, llm.Spend{Calls: fit, Tokens: fit * perCall, CostMicros: spent.CostMicros}, spent)
			assert.LessOrEqual(t, highest, int64(fit)*perCall, "no observer ever saw the limit passed")
			if tt.opts.MaxCostMicros > 0 {
				assert.Equal(t, int64(fit)*perCallCost, spent.CostMicros)
			}
		})
	}
}

func TestBudgeted_SpentIsSafeToReadWhileCallsAreInFlight(t *testing.T) {
	req := fakeRequest("model-a")
	req.MaxTokens = 10
	inner := newFakeModelFunc(func(int, llm.Request) fakeStep {
		s := fakeAnswer("model-a", llm.Usage{InputTokens: 1, OutputTokens: 1})
		s.hold = time.Millisecond
		return s
	})
	b, err := llm.NewBudgeted(inner, llm.BudgetOptions{})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := b.Generate(context.Background(), req)
			assert.NoError(t, err)
		}()
		go func() {
			defer wg.Done()
			_ = b.Spent()
		}()
	}
	wg.Wait()

	assert.Equal(t, llm.Spend{Calls: 20, Tokens: 40}, b.Spent())
}
