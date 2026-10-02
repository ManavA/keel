package llm_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// fakeStep is one outcome a fakeModel can play for a call.
type fakeStep struct {
	resp *llm.Response
	err  error
	// deltas are handed to a Stream's callback before it returns resp and err.
	// Generate ignores them.
	deltas []llm.Delta
	// hold keeps the call open this long, or until its context ends.
	hold time.Duration
	// onCall runs as the call starts, before anything else.
	onCall func()
	// panicWith, when set, is panicked with instead of returning.
	panicWith any
	// wrapCallbackError makes a Stream return its callback's error wrapped, as
	// a provider that adds its own context would, so a test can see a wrapper
	// give the caller's own error back.
	wrapCallbackError bool
}

// errFakeRanOut is what a fakeModel returns for a call beyond its steps. It is
// not retryable, so a wrapper that calls too often fails the test instead of
// looping.
var errFakeRanOut = errors.New("fake: no step for this call")

// fakeModel plays one step per call and records every request it was given.
// It stands in for a provider: the wrappers under test are the only thing that
// decides what a caller sees.
type fakeModel struct {
	mu     sync.Mutex
	step   func(call int, req llm.Request) fakeStep
	reqs   []llm.Request
	active int
	peak   int
}

var _ llm.Model = (*fakeModel)(nil)

// newFakeModel plays steps in order, one per call.
func newFakeModel(steps ...fakeStep) *fakeModel {
	return newFakeModelFunc(func(call int, _ llm.Request) fakeStep {
		if call > len(steps) {
			return fakeStep{err: errFakeRanOut}
		}
		return steps[call-1]
	})
}

// newFakeModelFunc chooses each step from the number of the call, counted from
// one, and the request.
func newFakeModelFunc(step func(call int, req llm.Request) fakeStep) *fakeModel {
	return &fakeModel{step: step}
}

func (f *fakeModel) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return f.run(ctx, req, nil)
}

func (f *fakeModel) Stream(ctx context.Context, req llm.Request, fn func(llm.Delta) error) (*llm.Response, error) {
	return f.run(ctx, req, fn)
}

func (f *fakeModel) run(ctx context.Context, req llm.Request, fn func(llm.Delta) error) (*llm.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	call := len(f.reqs)
	f.active++
	f.peak = max(f.peak, f.active)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	step := f.step(call, req)
	if step.onCall != nil {
		step.onCall()
	}
	if step.panicWith != nil {
		panic(step.panicWith)
	}
	if step.hold > 0 {
		timer := time.NewTimer(step.hold)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			// The caller's own context error and no *llm.Error, which is what a
			// provider returns once the caller's context is done.
			return nil, ctx.Err()
		}
	}
	if fn != nil {
		for _, d := range step.deltas {
			if err := fn(d); err != nil {
				if step.wrapCallbackError {
					err = fmt.Errorf("fake: stream stopped: %w", err)
				}
				return nil, err
			}
		}
	}
	return step.resp, step.err
}

// Calls is how many calls the model has received.
func (f *fakeModel) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// Requests are the requests received, in order.
func (f *fakeModel) Requests() []llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.Request(nil), f.reqs...)
}

// Peak is the most calls that were open at once.
func (f *fakeModel) Peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// httpClientTimeout is the error net/http returns when an http.Client's own
// Timeout fires on a request whose context is still live. It is made by a real
// client against a local server that never answers, not described by hand.
func httpClientTimeout(t *testing.T) error {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := (&http.Client{Timeout: 20 * time.Millisecond}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the server answered, so there is no timeout to keep")
	}
	return err
}

// noReplyMessage is what every wrapper says of a model that returns neither a
// reply nor an error.
const noReplyMessage = "model returned neither a reply nor an error"

// fakeRequest is a small valid request naming model, which may be empty.
func fakeRequest(model string) llm.Request {
	return llm.Request{
		Model:    model,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "summarise the batch"}},
	}
}

// fakeAnswer is a step that ends a turn normally.
func fakeAnswer(model string, usage llm.Usage) fakeStep {
	return fakeStep{resp: &llm.Response{
		ID:      "resp-" + model,
		Model:   model,
		Message: llm.Message{Role: llm.RoleAssistant, Text: "done"},
		Stop:    llm.StopEnd,
		Usage:   usage,
	}}
}

// fakeRefusal is a step in which the model declines. A refusal is a reply and
// is billed, so it carries usage.
func fakeRefusal(model string, usage llm.Usage) fakeStep {
	return fakeStep{resp: &llm.Response{
		ID:      "refusal-" + model,
		Model:   model,
		Message: llm.Message{Role: llm.RoleAssistant},
		Stop:    llm.StopRefusal,
		Usage:   usage,
		Refusal: &llm.Refusal{Category: "policy", Explanation: "declined"},
	}}
}

// fakeTransient is a failure a provider says may succeed later, asking for
// retryAfter before the next try.
func fakeTransient(retryAfter time.Duration) *llm.Error {
	return &llm.Error{
		Provider: "fake", Status: 429, Type: "rate_limit_error", Message: "slow down",
		RetryAfter: retryAfter, Retryable: true,
	}
}

// fakePermanent is a failure the same request will meet again.
func fakePermanent() *llm.Error {
	return &llm.Error{Provider: "fake", Status: 400, Type: "invalid_request_error", Message: "bad request"}
}

// fakeDeltas is two text increments, enough to tell a callback was called.
func fakeDeltas() []llm.Delta {
	return []llm.Delta{{Text: "one "}, {Text: "two"}}
}

// collectDeltas returns a callback that appends what it receives to *got.
func collectDeltas(got *[]llm.Delta) func(llm.Delta) error {
	return func(d llm.Delta) error {
		*got = append(*got, d)
		return nil
	}
}

// wrapperPrices is a made-up table in millionths of a dollar per million
// tokens: model-a costs $2 in and $10 out, model-b a quarter of that.
func wrapperPrices() llm.Prices {
	return llm.Prices{
		"model-a": {Input: 2_000_000, Output: 10_000_000},
		"model-b": {Input: 500_000, Output: 2_500_000},
	}
}

// promptly is how long a call that must not wait on anything may take. It is
// far above what such a call needs, so only a wait that should not happen can
// pass it.
const promptly = 3 * time.Second

// assertPrompt fails the test if d is not under promptly. It is how a test
// shows a wait that must not happen did not, without sleeping to find out.
func assertPrompt(t *testing.T, d time.Duration, what string) {
	t.Helper()
	if d >= promptly {
		t.Fatalf("%s took %v, which is not under %v", what, d, promptly)
	}
}
