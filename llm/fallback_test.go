package llm_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// quietLogger keeps the warnings a Fallback logs when it moves on out of the
// test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newFallback builds a chain that logs nowhere unless opts says otherwise.
func newFallback(t *testing.T, opts llm.FallbackOptions, models ...llm.Model) *llm.Fallback {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	f, err := llm.NewFallback(opts, models...)
	require.NoError(t, err)
	return f
}

func TestNewFallback(t *testing.T) {
	tests := []struct {
		name    string
		models  []llm.Model
		wantErr bool
	}{
		{name: "no models is an error", models: nil, wantErr: true},
		{name: "a nil model is an error", models: []llm.Model{newFakeModel(), nil}, wantErr: true},
		{name: "one model is a chain", models: []llm.Model{newFakeModel()}},
		{name: "several models are a chain", models: []llm.Model{newFakeModel(), newFakeModel()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := llm.NewFallback(llm.FallbackOptions{}, tt.models...)

			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, f)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, f)
		})
	}
}

func TestFallback_Generate(t *testing.T) {
	errBoom := errors.New("not a provider error")
	slow := fakeTransient(0)
	budget := fmt.Errorf("limit: %w", llm.ErrBudgetExceeded)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	// What an HTTP client's own timeout looks like: it wraps the deadline error
	// while the caller's context is still live.
	providerTimeout := &llm.Error{Provider: "fake", Err: fmt.Errorf("client timeout: %w", context.DeadlineExceeded), Retryable: true}
	providerCancel := &llm.Error{Provider: "fake", Err: fmt.Errorf("connection torn down: %w", context.Canceled)}
	usage := llm.Usage{InputTokens: 10, OutputTokens: 5}

	tests := []struct {
		name string
		ctx  context.Context
		opts llm.FallbackOptions
		// first and second are the steps of the first and second model.
		first, second fakeStep
		// wantSecondCalls is 0 when the chain must stop at the first model.
		wantSecondCalls int
		// wantModel is the model of the reply, or "" when the call must fail.
		wantModel string
		wantStop  llm.StopReason
		// wantErrIs lists errors the failed call's error must wrap.
		wantErrIs []error
	}{
		{
			name:      "the first model answering means the second is never called",
			first:     fakeAnswer("one", usage),
			second:    fakeAnswer("two", usage),
			wantModel: "one",
			wantStop:  llm.StopEnd,
		},
		{
			name:            "the first failing means the second answers",
			first:           fakeStep{err: fakeTransient(0)},
			second:          fakeAnswer("two", usage),
			wantSecondCalls: 1,
			wantModel:       "two",
			wantStop:        llm.StopEnd,
		},
		{
			name:            "any error moves on, not only a retryable one",
			first:           fakeStep{err: errBoom},
			second:          fakeAnswer("two", usage),
			wantSecondCalls: 1,
			wantModel:       "two",
			wantStop:        llm.StopEnd,
		},
		{
			name:            "a permanent provider failure moves on",
			first:           fakeStep{err: fakePermanent()},
			second:          fakeAnswer("two", usage),
			wantSecondCalls: 1,
			wantModel:       "two",
			wantStop:        llm.StopEnd,
		},
		{
			name:      "a cancelled context does not move on",
			ctx:       cancelled,
			first:     fakeStep{err: &llm.Error{Provider: "fake", Err: context.Canceled}},
			second:    fakeAnswer("two", usage),
			wantErrIs: []error{context.Canceled},
		},
		{
			name:      "an expired deadline of the caller's does not move on",
			ctx:       expired,
			first:     fakeStep{err: &llm.Error{Provider: "fake", Err: context.DeadlineExceeded}},
			second:    fakeAnswer("two", usage),
			wantErrIs: []error{context.DeadlineExceeded},
		},
		{
			name:            "a provider's own timeout moves on while the caller's context is live",
			first:           fakeStep{err: providerTimeout},
			second:          fakeAnswer("two", usage),
			wantSecondCalls: 1,
			wantModel:       "two",
			wantStop:        llm.StopEnd,
		},
		{
			name:            "so does a provider error that wraps a cancel the caller did not make",
			first:           fakeStep{err: providerCancel},
			second:          fakeAnswer("two", usage),
			wantSecondCalls: 1,
			wantModel:       "two",
			wantStop:        llm.StopEnd,
		},
		{
			name:      "a refused budget does not move on",
			first:     fakeStep{err: budget},
			second:    fakeAnswer("two", usage),
			wantErrIs: []error{llm.ErrBudgetExceeded},
		},
		{
			name: "ShouldFallback can refuse to move on",
			opts: llm.FallbackOptions{ShouldFallback: func(error) bool { return false }},
			// Moves on by default; here the caller's test says no.
			first:     fakeStep{err: slow},
			second:    fakeAnswer("two", usage),
			wantErrIs: []error{slow},
		},
		{
			name: "ShouldFallback replaces the default test, so a refused budget can move on",
			opts: llm.FallbackOptions{ShouldFallback: func(error) bool { return true }},
			// Does not move on by default; here the caller's test says yes.
			first:           fakeStep{err: budget},
			second:          fakeAnswer("two", usage),
			wantSecondCalls: 1,
			wantModel:       "two",
			wantStop:        llm.StopEnd,
		},
		{
			name:      "a cancelled context never moves on, whatever ShouldFallback says",
			ctx:       cancelled,
			opts:      llm.FallbackOptions{ShouldFallback: func(error) bool { return true }},
			first:     fakeStep{err: errBoom},
			second:    fakeAnswer("two", usage),
			wantErrIs: []error{errBoom},
		},
		{
			name:      "a refusal is an answer without OnRefusal",
			first:     fakeRefusal("one", usage),
			second:    fakeAnswer("two", usage),
			wantModel: "one",
			wantStop:  llm.StopRefusal,
		},
		{
			name:            "a refusal moves on with OnRefusal",
			opts:            llm.FallbackOptions{OnRefusal: true},
			first:           fakeRefusal("one", usage),
			second:          fakeAnswer("two", usage),
			wantSecondCalls: 1,
			wantModel:       "two",
			wantStop:        llm.StopEnd,
		},
		{
			name:      "OnRefusal leaves a normal answer alone",
			opts:      llm.FallbackOptions{OnRefusal: true},
			first:     fakeAnswer("one", usage),
			second:    fakeAnswer("two", usage),
			wantModel: "one",
			wantStop:  llm.StopEnd,
		},
		{
			name:            "when the last model refuses too, its refusal is the reply",
			opts:            llm.FallbackOptions{OnRefusal: true},
			first:           fakeRefusal("one", usage),
			second:          fakeRefusal("two", usage),
			wantSecondCalls: 1,
			wantModel:       "two",
			wantStop:        llm.StopRefusal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, second := newFakeModel(tt.first), newFakeModel(tt.second)
			f := newFallback(t, tt.opts, first, second)
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}

			resp, err := f.Generate(ctx, fakeRequest("asked"))

			assert.Equal(t, 1, first.Calls())
			assert.Equal(t, tt.wantSecondCalls, second.Calls())
			if tt.wantModel == "" {
				require.Error(t, err)
				assert.Nil(t, resp)
				for _, want := range tt.wantErrIs {
					assert.ErrorIs(t, err, want)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantModel, resp.Model)
			assert.Equal(t, tt.wantStop, resp.Stop)
		})
	}
}

func TestFallback_OnlyTheFirstModelIsNamed(t *testing.T) {
	one := newFakeModel(fakeStep{err: fakeTransient(0)})
	two := newFakeModel(fakeStep{err: fakeTransient(0)})
	three := newFakeModel(fakeAnswer("three", llm.Usage{}))
	f := newFallback(t, llm.FallbackOptions{}, one, two, three)
	req := fakeRequest("named")
	req.System = "be brief"

	resp, err := f.Generate(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, "three", resp.Model)
	require.Equal(t, 1, one.Calls())
	assert.Equal(t, req, one.Requests()[0], "the first model gets the request as given")
	for i, m := range []*fakeModel{two, three} {
		got := m.Requests()[0]
		assert.Empty(t, got.Model, "model %d after the first is not told a name that means something to another provider", i+2)
		want := req
		want.Model = ""
		assert.Equal(t, want, got, "and gets the rest of the request unchanged")
	}
	assert.Equal(t, "named", req.Model, "the caller's request is not changed")
}

func TestFallback_AllFailing(t *testing.T) {
	e1, e2, e3 := fakeTransient(0), fakePermanent(), errors.New("transport down")
	f := newFallback(t, llm.FallbackOptions{},
		newFakeModel(fakeStep{err: e1}), newFakeModel(fakeStep{err: e2}), newFakeModel(fakeStep{err: e3}))

	resp, err := f.Generate(context.Background(), fakeRequest(""))

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, e1)
	assert.ErrorIs(t, err, e2)
	assert.ErrorIs(t, err, e3)
	var le *llm.Error
	assert.ErrorAs(t, err, &le, "errors.As finds a provider error")
	for _, text := range []string{"slow down", "bad request", "transport down"} {
		assert.Contains(t, err.Error(), text, "the message carries each failure")
	}
}

func TestFallback_StopsAtTheFirstErrorThatDoesNotMoveOn(t *testing.T) {
	// The first model fails and the chain moves on; the second is refused by
	// its budget. The third is never reached, and the error says both.
	first, second, third := fakeTransient(0), fmt.Errorf("limit: %w", llm.ErrBudgetExceeded), fakeAnswer("three", llm.Usage{})
	m3 := newFakeModel(third)
	f := newFallback(t, llm.FallbackOptions{},
		newFakeModel(fakeStep{err: first}), newFakeModel(fakeStep{err: second}), m3)

	_, err := f.Generate(context.Background(), fakeRequest(""))

	assert.ErrorIs(t, err, llm.ErrBudgetExceeded)
	assert.ErrorIs(t, err, first, "the earlier failure is not lost")
	assert.Zero(t, m3.Calls())
}

// A refusal that was moved past was billed. If no later model answers, the
// call returns that refusal as its reply, so the budget and the meter see what
// was paid for and a caller that tries the step again does not pay again for a
// refusal nobody saw. The failures that followed are logged.
func TestFallback_ARefusalIsTheReplyWhenNoLaterModelAnswers(t *testing.T) {
	refusedOne := llm.Usage{InputTokens: 100, OutputTokens: 7}
	refusedTwo := llm.Usage{InputTokens: 100, OutputTokens: 9}
	tests := []struct {
		name  string
		opts  llm.FallbackOptions
		steps []fakeStep
		// cancelOnCall is the number of the call that ends the caller's context.
		cancelOnCall int
		wantModel    string
		// wantAttempts is empty when only one model's refusal was billed.
		wantAttempts []llm.Attempt
		// wantLogged are texts the failures must have left in the log.
		wantLogged []string
	}{
		{
			name:       "a failure after the refusal",
			steps:      []fakeStep{fakeRefusal("one", refusedOne), {err: fakeTransient(0)}},
			wantModel:  "one",
			wantLogged: []string{"slow down"},
		},
		{
			name:       "two refusals and then a failure: the last refusal, with both attempts",
			steps:      []fakeStep{fakeRefusal("one", refusedOne), fakeRefusal("two", refusedTwo), {err: fakeTransient(0)}},
			wantModel:  "two",
			wantLogged: []string{"slow down"},
			wantAttempts: []llm.Attempt{
				{Model: "one", Usage: refusedOne}, {Model: "two", Usage: refusedTwo},
			},
		},
		{
			name:       "several failures after the refusal are each logged",
			steps:      []fakeStep{fakeRefusal("one", refusedOne), {err: fakeTransient(0)}, {err: errors.New("transport down")}},
			wantModel:  "one",
			wantLogged: []string{"slow down", "transport down"},
		},
		{
			name:       "a failure that does not move on, such as a refused budget",
			steps:      []fakeStep{fakeRefusal("one", refusedOne), {err: fmt.Errorf("limit: %w", llm.ErrBudgetExceeded)}},
			wantModel:  "one",
			wantLogged: []string{"limit"},
		},
		{
			name:       "a failure the caller's ShouldFallback refuses to move past",
			opts:       llm.FallbackOptions{ShouldFallback: func(error) bool { return false }},
			steps:      []fakeStep{fakeRefusal("one", refusedOne), {err: fakeTransient(0)}},
			wantModel:  "one",
			wantLogged: []string{"slow down"},
		},
		{
			name:         "the caller's context ending during the later call",
			steps:        []fakeStep{fakeRefusal("one", refusedOne), {err: &llm.Error{Provider: "fake", Err: context.Canceled}}},
			cancelOnCall: 2,
			wantModel:    "one",
			wantLogged:   []string{"context canceled"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tt.opts.OnRefusal = true
			tt.opts.Logger = slog.New(slog.NewTextHandler(&buf, nil))
			ctx := context.Background()
			steps := append([]fakeStep(nil), tt.steps...)
			if tt.cancelOnCall > 0 {
				var cancelCtx context.CancelFunc
				ctx, cancelCtx = context.WithCancel(ctx)
				defer cancelCtx()
				steps[tt.cancelOnCall-1].onCall = cancelCtx
			}
			models := make([]llm.Model, len(steps))
			fakes := make([]*fakeModel, len(steps))
			for i, step := range steps {
				fakes[i] = newFakeModel(step)
				models[i] = fakes[i]
			}
			f := newFallback(t, tt.opts, models...)

			resp, err := f.Generate(ctx, fakeRequest("asked"))

			require.NoError(t, err, "the refusal is the reply")
			assert.Equal(t, tt.wantModel, resp.Model)
			assert.Equal(t, llm.StopRefusal, resp.Stop)
			assert.Equal(t, tt.wantAttempts, resp.Attempts)
			for _, text := range tt.wantLogged {
				assert.Contains(t, buf.String(), text, "the later failure is logged")
			}
			assert.Contains(t, buf.String(), "level=WARN")
		})
	}
}

func TestFallback_AttemptsAccountForARefusalThatWasBilled(t *testing.T) {
	refusedUsage := llm.Usage{InputTokens: 100, OutputTokens: 7}
	answeredUsage := llm.Usage{InputTokens: 120, OutputTokens: 40}
	refusal := fakeRefusal("model-a", refusedUsage)
	answer := fakeAnswer("model-b", answeredUsage)
	f := newFallback(t, llm.FallbackOptions{OnRefusal: true}, newFakeModel(refusal), newFakeModel(answer))

	resp, err := f.Generate(context.Background(), fakeRequest(""))

	require.NoError(t, err)
	assert.Equal(t, "model-b", resp.Model)
	assert.Equal(t, answeredUsage, resp.Usage, "Usage is still what the answering model was billed")
	assert.Equal(t, []llm.Attempt{
		{Model: "model-a", Usage: refusedUsage},
		{Model: "model-b", Usage: answeredUsage},
	}, resp.Attempts, "every billed attempt is listed, so a price table sees both")
	assert.Nil(t, answer.resp.Attempts, "the inner model's own reply is not changed")

	cost, err := wrapperPrices().CostOf(resp)
	require.NoError(t, err)
	wantA, _ := wrapperPrices().Cost("model-a", refusedUsage)
	wantB, _ := wrapperPrices().Cost("model-b", answeredUsage)
	assert.Equal(t, wantA+wantB, cost)
}

func TestFallback_TheLastRefusalCarriesEveryRefusedAttempt(t *testing.T) {
	// Every model refuses: the last refusal is the reply, and all the attempts
	// that were billed on the way are in it.
	u1, u2, u3 := llm.Usage{InputTokens: 11}, llm.Usage{InputTokens: 22}, llm.Usage{InputTokens: 33}
	f := newFallback(t, llm.FallbackOptions{OnRefusal: true},
		newFakeModel(fakeRefusal("one", u1)), newFakeModel(fakeRefusal("two", u2)), newFakeModel(fakeRefusal("three", u3)))

	resp, err := f.Generate(context.Background(), fakeRequest(""))

	require.NoError(t, err)
	assert.Equal(t, "three", resp.Model)
	assert.Equal(t, llm.StopRefusal, resp.Stop)
	assert.Equal(t, u3, resp.Usage, "Usage is still the last model's")
	assert.Equal(t, []llm.Attempt{{Model: "one", Usage: u1}, {Model: "two", Usage: u2}, {Model: "three", Usage: u3}}, resp.Attempts)
}

func TestFallback_AttemptsKeepTheAnsweringModelsOwn(t *testing.T) {
	// A provider that already lists its attempts (a server-side fallback)
	// keeps them, after the refusals this chain moved past.
	own := []llm.Attempt{{Model: "model-b", Usage: llm.Usage{InputTokens: 9}}, {Model: "model-c", Usage: llm.Usage{InputTokens: 3}}}
	answer := fakeAnswer("model-c", llm.Usage{InputTokens: 3})
	answer.resp.Attempts = own
	f := newFallback(t, llm.FallbackOptions{OnRefusal: true},
		newFakeModel(fakeRefusal("model-a", llm.Usage{InputTokens: 5})), newFakeModel(answer))

	resp, err := f.Generate(context.Background(), fakeRequest(""))

	require.NoError(t, err)
	assert.Equal(t, append([]llm.Attempt{{Model: "model-a", Usage: llm.Usage{InputTokens: 5}}}, own...), resp.Attempts)
}

func TestFallback_AnswerWithNothingBeforeItIsReturnedAsIs(t *testing.T) {
	answer := fakeAnswer("one", llm.Usage{InputTokens: 1})
	f := newFallback(t, llm.FallbackOptions{OnRefusal: true}, newFakeModel(answer), newFakeModel())

	resp, err := f.Generate(context.Background(), fakeRequest(""))

	require.NoError(t, err)
	assert.Same(t, answer.resp, resp)
	assert.Empty(t, resp.Attempts, "one model ran, so there is one attempt and the list stays empty")
}

func TestFallback_Stream(t *testing.T) {
	errStop := errors.New("caller stopped")
	budget := fmt.Errorf("limit: %w", llm.ErrBudgetExceeded)
	usage := llm.Usage{InputTokens: 10, OutputTokens: 5}
	answerWith := func(model string) fakeStep {
		s := fakeAnswer(model, usage)
		s.deltas = fakeDeltas()
		return s
	}
	tests := []struct {
		name          string
		opts          llm.FallbackOptions
		first, second fakeStep
		// stopAfter is the number of deltas after which the callback fails.
		stopAfter       int
		wantSecondCalls int
		wantDeltas      []llm.Delta
		wantModel       string
		wantErrIs       error
		wantProvider    bool
	}{
		{
			name:       "the first model streaming means the second is never called",
			first:      answerWith("one"),
			second:     answerWith("two"),
			wantDeltas: fakeDeltas(),
			wantModel:  "one",
		},
		{
			name:            "a failure before the first delta moves on",
			first:           fakeStep{err: fakeTransient(0)},
			second:          answerWith("two"),
			wantSecondCalls: 1,
			wantDeltas:      fakeDeltas(),
			wantModel:       "two",
		},
		{
			name:         "a failure after a delta does not move on",
			first:        fakeStep{deltas: fakeDeltas()[:1], err: fakeTransient(0)},
			second:       answerWith("two"),
			wantDeltas:   fakeDeltas()[:1],
			wantProvider: true,
		},
		{
			name:       "an error from the callback stops the stream and is returned",
			first:      answerWith("one"),
			second:     answerWith("two"),
			stopAfter:  1,
			wantDeltas: fakeDeltas()[:1],
			wantErrIs:  errStop,
		},
		{
			name:       "a refused budget does not move on",
			first:      fakeStep{err: budget},
			second:     answerWith("two"),
			wantErrIs:  llm.ErrBudgetExceeded,
			wantDeltas: nil,
		},
		{
			name: "a provider's own timeout before the first delta moves on",
			first: fakeStep{err: &llm.Error{
				Provider: "fake", Err: fmt.Errorf("client timeout: %w", context.DeadlineExceeded), Retryable: true,
			}},
			second:          answerWith("two"),
			wantSecondCalls: 1,
			wantDeltas:      fakeDeltas(),
			wantModel:       "two",
		},
		{
			name:            "a refusal before any delta moves on with OnRefusal",
			opts:            llm.FallbackOptions{OnRefusal: true},
			first:           fakeRefusal("one", usage),
			second:          answerWith("two"),
			wantSecondCalls: 1,
			wantDeltas:      fakeDeltas(),
			wantModel:       "two",
		},
		{
			name:            "a refusal and then a failure before any delta: the refusal is the reply",
			opts:            llm.FallbackOptions{OnRefusal: true},
			first:           fakeRefusal("one", usage),
			second:          fakeStep{err: fakeTransient(0)},
			wantSecondCalls: 1,
			wantModel:       "one",
		},
		{
			name:            "a refusal and then a stream that fails after a delta: still the refusal is the reply",
			opts:            llm.FallbackOptions{OnRefusal: true},
			first:           fakeRefusal("one", usage),
			second:          fakeStep{deltas: fakeDeltas()[:1], err: fakeTransient(0)},
			wantSecondCalls: 1,
			wantDeltas:      fakeDeltas()[:1],
			wantModel:       "one",
		},
		{
			name: "a refusal after a delta is returned, since the caller has already seen it",
			opts: llm.FallbackOptions{OnRefusal: true},
			first: func() fakeStep {
				s := fakeRefusal("one", usage)
				s.deltas = fakeDeltas()[:1]
				return s
			}(),
			second:     answerWith("two"),
			wantDeltas: fakeDeltas()[:1],
			wantModel:  "one",
		},
		{
			name:      "a refusal is an answer without OnRefusal",
			first:     fakeRefusal("one", usage),
			second:    answerWith("two"),
			wantModel: "one",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, second := newFakeModel(tt.first), newFakeModel(tt.second)
			f := newFallback(t, tt.opts, first, second)
			var got []llm.Delta
			fn := collectDeltas(&got)
			if tt.stopAfter > 0 {
				fn = func(d llm.Delta) error {
					got = append(got, d)
					if len(got) >= tt.stopAfter {
						return errStop
					}
					return nil
				}
			}

			resp, err := f.Stream(context.Background(), fakeRequest("asked"), fn)

			assert.Equal(t, 1, first.Calls())
			require.Equal(t, tt.wantSecondCalls, second.Calls())
			assert.Equal(t, tt.wantDeltas, got, "the callback sees one model's deltas, never a mix")
			if tt.wantSecondCalls > 0 {
				assert.Empty(t, second.Requests()[0].Model)
			}
			if tt.wantErrIs != nil || tt.wantProvider {
				require.Error(t, err)
				assert.Nil(t, resp)
				if tt.wantProvider {
					var le *llm.Error
					assert.ErrorAs(t, err, &le)
				}
				if tt.wantErrIs != nil {
					assert.ErrorIs(t, err, tt.wantErrIs)
				}
				if tt.stopAfter > 0 {
					assert.Same(t, errStop, err, "the callback's own error, as it returned it")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantModel, resp.Model)
		})
	}
}

// The callback's error is the caller's own signal to stop, not a failure of a
// model: it comes back as the caller returned it, not joined with anything,
// not wrapped by the provider, and not moved past.
func TestFallback_TheCallbacksErrorComesBackUntouched(t *testing.T) {
	errStop := errors.New("caller stopped")
	looksRetryable := fakeTransient(0)
	tests := []struct {
		name    string
		cbErr   error
		wrapped bool
		// refusedFirst puts a refusal that was moved past ahead of the stream.
		refusedFirst bool
	}{
		{name: "a plain error", cbErr: errStop},
		{name: "an error that looks like a failure to move on from", cbErr: looksRetryable},
		{name: "an error the provider wrapped on the way out", cbErr: errStop, wrapped: true},
		{name: "after a refusal that was moved past", cbErr: errStop, refusedFirst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := fakeAnswer("one", llm.Usage{})
			step.deltas = fakeDeltas()
			step.wrapCallbackError = tt.wrapped
			models := []llm.Model{newFakeModel(step)}
			next := newFakeModel(fakeAnswer("two", llm.Usage{}))
			if tt.refusedFirst {
				models = []llm.Model{newFakeModel(fakeRefusal("zero", llm.Usage{})), models[0]}
			}
			models = append(models, next)
			f := newFallback(t, llm.FallbackOptions{OnRefusal: true}, models...)

			resp, err := f.Stream(context.Background(), fakeRequest(""), func(llm.Delta) error { return tt.cbErr })

			assert.Nil(t, resp)
			assert.Same(t, tt.cbErr, err)
			assert.Zero(t, next.Calls(), "no later model takes over")
		})
	}
}

func TestFallback_ANilReplyIsAFailureOfThatModel(t *testing.T) {
	// A model that returns neither a reply nor an error is broken. With
	// OnRefusal the chain would read its Stop, so it must be caught first.
	for _, onRefusal := range []bool{false, true} {
		t.Run(fmt.Sprintf("OnRefusal %v", onRefusal), func(t *testing.T) {
			opts := llm.FallbackOptions{OnRefusal: onRefusal}

			t.Run("the chain moves on", func(t *testing.T) {
				two := newFakeModel(fakeAnswer("two", llm.Usage{}))
				f := newFallback(t, opts, newFakeModel(fakeStep{}), two)

				resp, err := f.Generate(context.Background(), fakeRequest(""))
				require.NoError(t, err)
				assert.Equal(t, "two", resp.Model)

				resp, err = newFallback(t, opts, newFakeModel(fakeStep{}), newFakeModel(fakeAnswer("two", llm.Usage{}))).
					Stream(context.Background(), fakeRequest(""), func(llm.Delta) error { return nil })
				require.NoError(t, err)
				assert.Equal(t, "two", resp.Model)
			})

			t.Run("and when every model does it the call is an error", func(t *testing.T) {
				f := newFallback(t, opts, newFakeModel(fakeStep{}), newFakeModel(fakeStep{}))

				resp, err := f.Generate(context.Background(), fakeRequest(""))

				assert.Nil(t, resp)
				assert.ErrorContains(t, err, noReplyMessage)
			})
		})
	}
}

func TestFallback_StreamStopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second := newFakeModel(fakeAnswer("two", llm.Usage{}))
	f := newFallback(t, llm.FallbackOptions{},
		newFakeModel(fakeStep{err: &llm.Error{Provider: "fake", Err: context.Canceled}}), second)

	_, err := f.Stream(ctx, fakeRequest(""), func(llm.Delta) error { return nil })

	assert.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, second.Calls())
}

func TestFallback_LogsEachMoveOn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	f := newFallback(t, llm.FallbackOptions{Logger: logger},
		newFakeModel(fakeStep{err: fakeTransient(0)}), newFakeModel(fakeAnswer("two", llm.Usage{})))

	_, err := f.Generate(context.Background(), fakeRequest(""))
	require.NoError(t, err)

	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "llm: fallback")
	assert.Contains(t, buf.String(), "slow down", "the log carries the failure that was moved past")

	buf.Reset()
	f = newFallback(t, llm.FallbackOptions{Logger: logger}, newFakeModel(fakeAnswer("one", llm.Usage{})))
	_, err = f.Generate(context.Background(), fakeRequest(""))
	require.NoError(t, err)
	assert.Empty(t, buf.String(), "a first answer logs nothing")
}

func TestFallback_WithNoLoggerOptionItStillMovesOn(t *testing.T) {
	// With no Logger the chain logs through slog's default. Raising that
	// logger's level for the length of the test keeps it quiet, and the level
	// is put back exactly; the default logger itself is never replaced.
	previous := slog.SetLogLoggerLevel(slog.LevelError + 1)
	t.Cleanup(func() { slog.SetLogLoggerLevel(previous) })
	f, err := llm.NewFallback(llm.FallbackOptions{},
		newFakeModel(fakeStep{err: fakeTransient(0)}), newFakeModel(fakeAnswer("two", llm.Usage{})))
	require.NoError(t, err)

	resp, err := f.Generate(context.Background(), fakeRequest(""))

	require.NoError(t, err)
	assert.Equal(t, "two", resp.Model)
}
