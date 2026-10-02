package llm_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/retry"
)

// fastBackoff keeps the jitter retry.Do adds between attempts under a
// millisecond, so a test that waits for anything waits for the wrapper.
var fastBackoff = retry.Options{BaseDelay: time.Microsecond, MaxDelay: time.Millisecond}

func TestRetrying_Generate(t *testing.T) {
	boom := errors.New("not a provider error")
	tests := []struct {
		name  string
		opts  llm.RetryOptions
		steps []fakeStep
		// wantCalls is how many times the inner model must be called.
		wantCalls int
		// wantModel is the answering model, or "" when the call must fail.
		wantModel string
		// wantAttempts is retry.Error.Attempts for a failed call.
		wantAttempts int
	}{
		{
			name:      "a retryable failure then success is two calls",
			steps:     []fakeStep{{err: fakeTransient(0)}, fakeAnswer("m", llm.Usage{})},
			wantCalls: 2,
			wantModel: "m",
		},
		{
			name:         "a failure that is not retryable is one call",
			steps:        []fakeStep{{err: fakePermanent()}, fakeAnswer("m", llm.Usage{})},
			wantCalls:    1,
			wantAttempts: 1,
		},
		{
			name:         "an error that is not from a provider is one call",
			steps:        []fakeStep{{err: boom}, fakeAnswer("m", llm.Usage{})},
			wantCalls:    1,
			wantAttempts: 1,
		},
		{
			name:         "three retryable failures give up after three",
			steps:        []fakeStep{{err: fakeTransient(0)}, {err: fakeTransient(0)}, {err: fakeTransient(0)}, fakeAnswer("m", llm.Usage{})},
			wantCalls:    3,
			wantAttempts: 3,
		},
		{
			name:         "MaxAttempts changes the limit",
			opts:         llm.RetryOptions{Retry: retry.Options{MaxAttempts: 2}},
			steps:        []fakeStep{{err: fakeTransient(0)}, {err: fakeTransient(0)}, fakeAnswer("m", llm.Usage{})},
			wantCalls:    2,
			wantAttempts: 2,
		},
		{
			name:      "the third attempt may be the one that answers",
			steps:     []fakeStep{{err: fakeTransient(0)}, {err: fakeTransient(0)}, fakeAnswer("m", llm.Usage{})},
			wantCalls: 3,
			wantModel: "m",
		},
		{
			name: "a Retryable of the caller's is replaced",
			opts: llm.RetryOptions{Retry: retry.Options{Retryable: func(error) bool { return false }}},
			// With the caller's test in force this would be one call.
			steps:     []fakeStep{{err: fakeTransient(0)}, fakeAnswer("m", llm.Usage{})},
			wantCalls: 2,
			wantModel: "m",
		},
		{
			name:      "a wrapped provider failure is still read as retryable",
			steps:     []fakeStep{{err: fmt.Errorf("sending: %w", fakeTransient(0))}, fakeAnswer("m", llm.Usage{})},
			wantCalls: 2,
			wantModel: "m",
		},
		{
			name:         "a refused budget is not retried",
			steps:        []fakeStep{{err: fmt.Errorf("%w: too big", llm.ErrBudgetExceeded)}, fakeAnswer("m", llm.Usage{})},
			wantCalls:    1,
			wantAttempts: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Retry.BaseDelay = fastBackoff.BaseDelay
			tt.opts.Retry.MaxDelay = fastBackoff.MaxDelay
			inner := newFakeModel(tt.steps...)
			r := llm.NewRetrying(inner, tt.opts)

			resp, err := r.Generate(context.Background(), fakeRequest("asked"))

			assert.Equal(t, tt.wantCalls, inner.Calls())
			if tt.wantModel != "" {
				require.NoError(t, err)
				assert.Equal(t, tt.wantModel, resp.Model)
				return
			}
			require.Error(t, err)
			assert.Nil(t, resp)
			var re *retry.Error
			require.ErrorAs(t, err, &re, "the error is retry.Do's")
			assert.Equal(t, tt.wantAttempts, re.Attempts)
		})
	}
}

func TestRetrying_GiveUpKeepsTheProviderError(t *testing.T) {
	last := fakeTransient(0)
	last.RequestID = "req-last"
	inner := newFakeModel(
		fakeStep{err: fakeTransient(0)}, fakeStep{err: fakeTransient(0)}, fakeStep{err: last},
	)
	r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

	_, err := r.Generate(context.Background(), fakeRequest(""))

	var le *llm.Error
	require.ErrorAs(t, err, &le, "errors.As still finds the *llm.Error")
	assert.Equal(t, "req-last", le.RequestID, "the error of the last attempt is the one kept")
	assert.ErrorIs(t, err, last)
	assert.True(t, llm.Retryable(err), "the caller may still read it as retryable")
}

func TestRetrying_RetryAfter(t *testing.T) {
	t.Run("a Retry-After is waited", func(t *testing.T) {
		inner := newFakeModel(fakeStep{err: fakeTransient(80 * time.Millisecond)}, fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		start := time.Now()
		_, err := r.Generate(context.Background(), fakeRequest(""))

		require.NoError(t, err)
		// The backoff above cannot add 80 milliseconds, so this is the wait.
		assert.GreaterOrEqual(t, time.Since(start), 80*time.Millisecond)
		assert.Equal(t, 2, inner.Calls())
	})

	t.Run("a Retry-After is capped at MaxRetryAfter", func(t *testing.T) {
		// An hour asked for, 30 milliseconds allowed. If the cap were not
		// applied the context would end first and the error would say so.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		inner := newFakeModel(fakeStep{err: fakeTransient(time.Hour)}, fakeStep{err: fakeTransient(time.Hour)})
		r := llm.NewRetrying(inner, llm.RetryOptions{
			Retry:         retry.Options{MaxAttempts: 2, BaseDelay: fastBackoff.BaseDelay, MaxDelay: fastBackoff.MaxDelay},
			MaxRetryAfter: 30 * time.Millisecond,
		})

		start := time.Now()
		_, err := r.Generate(ctx, fakeRequest(""))

		took := time.Since(start)
		require.Error(t, err)
		assert.NotErrorIs(t, err, context.DeadlineExceeded)
		var re *retry.Error
		require.ErrorAs(t, err, &re)
		assert.Equal(t, 2, re.Attempts)
		assert.GreaterOrEqual(t, took, 30*time.Millisecond, "the capped wait was made")
		assertPrompt(t, took, "two attempts and one capped wait")
	})

	t.Run("there is no wait after the last attempt", func(t *testing.T) {
		// The last attempt asks for an hour. Waiting it out, or even the
		// default cap of a minute, would run the context out first.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		inner := newFakeModel(fakeStep{err: fakeTransient(0)}, fakeStep{err: fakeTransient(time.Hour)})
		r := llm.NewRetrying(inner, llm.RetryOptions{
			Retry: retry.Options{MaxAttempts: 2, BaseDelay: fastBackoff.BaseDelay, MaxDelay: fastBackoff.MaxDelay},
		})

		start := time.Now()
		_, err := r.Generate(ctx, fakeRequest(""))

		require.Error(t, err)
		assert.NotErrorIs(t, err, context.DeadlineExceeded)
		var le *llm.Error
		require.ErrorAs(t, err, &le)
		assert.Equal(t, time.Hour, le.RetryAfter, "the error is the provider's, with its Retry-After")
		assertPrompt(t, time.Since(start), "the call")
		assert.Equal(t, 2, inner.Calls())
	})

	t.Run("a failure that is not retryable is not waited on", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		permanent := fakePermanent()
		permanent.RetryAfter = time.Hour
		inner := newFakeModel(fakeStep{err: permanent})
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		start := time.Now()
		_, err := r.Generate(ctx, fakeRequest(""))

		require.ErrorIs(t, err, permanent)
		assert.NotErrorIs(t, err, context.DeadlineExceeded)
		assertPrompt(t, time.Since(start), "the call")
	})

	t.Run("a Retry-After is waited on a stream too", func(t *testing.T) {
		inner := newFakeModel(fakeStep{err: fakeTransient(60 * time.Millisecond)}, fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		start := time.Now()
		_, err := r.Stream(context.Background(), fakeRequest(""), func(llm.Delta) error { return nil })

		require.NoError(t, err)
		assert.GreaterOrEqual(t, time.Since(start), 60*time.Millisecond)
	})
}

func TestRetrying_Context(t *testing.T) {
	t.Run("a cancelled context never reaches the model", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		inner := newFakeModel(fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		resp, err := r.Generate(ctx, fakeRequest(""))

		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, resp)
		assert.Zero(t, inner.Calls())
	})

	t.Run("cancelled while waiting out a Retry-After", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		inner := newFakeModel(
			fakeStep{err: fakeTransient(time.Hour), onCall: cancel},
			fakeAnswer("m", llm.Usage{}),
		)
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff, MaxRetryAfter: 2 * time.Hour})

		start := time.Now()
		_, err := r.Generate(ctx, fakeRequest(""))

		require.ErrorIs(t, err, context.Canceled)
		assert.False(t, llm.Retryable(err), "a cancelled call is not retryable, whatever the provider said")
		assert.Equal(t, 1, inner.Calls(), "no second attempt after the cancel")
		assertPrompt(t, time.Since(start), "the call")
	})

	t.Run("cancelled while backing off", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		inner := newFakeModel(
			fakeStep{err: fakeTransient(0), onCall: cancel},
			fakeAnswer("m", llm.Usage{}),
		)
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: retry.Options{BaseDelay: time.Hour, MaxDelay: time.Hour}})

		start := time.Now()
		_, err := r.Generate(ctx, fakeRequest(""))

		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, inner.Calls())
		assertPrompt(t, time.Since(start), "the call")
	})

	t.Run("a stream is stopped by a cancelled context too", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		inner := newFakeModel(fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		_, err := r.Stream(ctx, fakeRequest(""), func(llm.Delta) error { return nil })

		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, inner.Calls())
	})
}

// An http.Client's own timeout is a failure of the provider, not of the
// caller, even though it wraps the deadline error: the provider marks it
// retryable, and Retrying must act on that while the caller's context is live.
func TestRetrying_AProviderTimeoutIsRetried(t *testing.T) {
	timeout := &llm.Error{Provider: "fake", Err: httpClientTimeout(t), Retryable: true}
	require.ErrorIs(t, timeout, context.DeadlineExceeded, "the fixture: it reads as a deadline")

	t.Run("Generate", func(t *testing.T) {
		inner := newFakeModel(fakeStep{err: timeout}, fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		resp, err := r.Generate(context.Background(), fakeRequest(""))

		require.NoError(t, err)
		assert.Equal(t, "m", resp.Model)
		assert.Equal(t, 2, inner.Calls())
	})
	t.Run("Stream, before a delta", func(t *testing.T) {
		inner := newFakeModel(fakeStep{err: timeout}, fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		resp, err := r.Stream(context.Background(), fakeRequest(""), func(llm.Delta) error { return nil })

		require.NoError(t, err)
		assert.Equal(t, "m", resp.Model)
		assert.Equal(t, 2, inner.Calls())
	})
	t.Run("Generate, every attempt timing out gives up after three", func(t *testing.T) {
		inner := newFakeModelFunc(func(int, llm.Request) fakeStep { return fakeStep{err: timeout} })
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		_, err := r.Generate(context.Background(), fakeRequest(""))

		var re *retry.Error
		require.ErrorAs(t, err, &re)
		assert.Equal(t, 3, re.Attempts)
		assert.Equal(t, 3, inner.Calls())
	})
}

// The caller's own context ending is not a provider failure and is not retried,
// whatever shape the failure comes back in.
func TestRetrying_TheCallersContextEndingEndsTheLoopAtOnce(t *testing.T) {
	t.Run("a deadline reached during a call", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		// The call is held until the context ends and returns its error bare,
		// as a provider does for a caller who gave up.
		inner := newFakeModel(fakeStep{hold: time.Hour}, fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		start := time.Now()
		_, err := r.Generate(ctx, fakeRequest(""))

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.False(t, llm.Retryable(err))
		assert.Equal(t, 1, inner.Calls())
		assertPrompt(t, time.Since(start), "the call")
	})
	t.Run("a deadline reached during a stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		inner := newFakeModel(fakeStep{hold: time.Hour}, fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		_, err := r.Stream(ctx, fakeRequest(""), func(llm.Delta) error { return nil })

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 1, inner.Calls())
	})
	t.Run("a provider that still marks its cancelled call retryable", func(t *testing.T) {
		// The old shape: a retryable *llm.Error wrapping the caller's cancel.
		// The caller's context decides, so it is still not retried.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		old := &llm.Error{Provider: "fake", Retryable: true, Err: context.Canceled}
		inner := newFakeModel(fakeStep{err: old, onCall: cancel}, fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: retry.Options{BaseDelay: time.Hour, MaxDelay: time.Hour}})

		start := time.Now()
		_, err := r.Generate(ctx, fakeRequest(""))

		require.ErrorIs(t, err, context.Canceled)
		assert.False(t, llm.Retryable(err))
		assert.Equal(t, 1, inner.Calls())
		assertPrompt(t, time.Since(start), "the call")
	})
}

func TestRetrying_Stream(t *testing.T) {
	tests := []struct {
		name  string
		steps []fakeStep
		fn    func(*[]llm.Delta) func(llm.Delta) error
		// wantDeltas are the deltas the caller's callback must have seen.
		wantDeltas []llm.Delta
		wantCalls  int
		// wantErr is what the call must fail with, and wantProvider that it
		// must fail with the provider's *llm.Error; with neither it must answer
		// as wantModel.
		wantErr      error
		wantProvider bool
		wantModel    string
	}{
		{
			name:       "a stream that failed before any delta is retried",
			steps:      []fakeStep{{err: fakeTransient(0)}, {deltas: fakeDeltas(), resp: fakeAnswer("m", llm.Usage{}).resp}},
			fn:         collectDeltas,
			wantDeltas: fakeDeltas(),
			wantCalls:  2,
			wantModel:  "m",
		},
		{
			name: "a stream that has delivered a delta is not retried",
			steps: []fakeStep{
				{deltas: fakeDeltas()[:1], err: fakeTransient(0)},
				{deltas: fakeDeltas(), resp: fakeAnswer("m", llm.Usage{}).resp},
			},
			fn:           collectDeltas,
			wantDeltas:   fakeDeltas()[:1],
			wantCalls:    1,
			wantProvider: true,
		},
		{
			name: "a failure before a delta is retried but one after it is not",
			steps: []fakeStep{
				{err: fakeTransient(0)},
				{deltas: fakeDeltas()[:1], err: fakeTransient(0)},
				{deltas: fakeDeltas(), resp: fakeAnswer("m", llm.Usage{}).resp},
			},
			fn:           collectDeltas,
			wantDeltas:   fakeDeltas()[:1],
			wantCalls:    2,
			wantProvider: true,
		},
		{
			name:         "a failure that is not retryable is one call",
			steps:        []fakeStep{{err: fakePermanent()}, fakeAnswer("m", llm.Usage{})},
			fn:           collectDeltas,
			wantDeltas:   nil,
			wantCalls:    1,
			wantProvider: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := newFakeModel(tt.steps...)
			r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})
			var got []llm.Delta

			resp, err := r.Stream(context.Background(), fakeRequest(""), tt.fn(&got))

			assert.Equal(t, tt.wantCalls, inner.Calls())
			assert.Equal(t, tt.wantDeltas, got, "a delta is never delivered twice")
			if tt.wantErr == nil && !tt.wantProvider {
				require.NoError(t, err)
				assert.Equal(t, tt.wantModel, resp.Model)
				return
			}
			require.Error(t, err)
			assert.Nil(t, resp)
			if tt.wantProvider {
				var le *llm.Error
				assert.ErrorAs(t, err, &le)
			}
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			}
		})
	}
}

// The callback's error is the caller's own signal to stop, not a failure of
// the model, so it comes back as the caller returned it: not wrapped in a
// *retry.Error, not wrapped by the provider, not retried however it reads.
func TestRetrying_TheCallbacksErrorComesBackUntouched(t *testing.T) {
	errStop := errors.New("caller stopped")
	looksRetryable := fakeTransient(0)
	tests := []struct {
		name    string
		cbErr   error
		wrapped bool
	}{
		{name: "a plain error", cbErr: errStop},
		{name: "an error that looks retryable", cbErr: looksRetryable},
		{name: "an error the provider wrapped on the way out", cbErr: errStop, wrapped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := fakeAnswer("m", llm.Usage{})
			step.deltas = fakeDeltas()
			step.wrapCallbackError = tt.wrapped
			inner := newFakeModel(step, step)
			r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})
			var got []llm.Delta

			resp, err := r.Stream(context.Background(), fakeRequest(""), func(d llm.Delta) error {
				got = append(got, d)
				return tt.cbErr
			})

			assert.Nil(t, resp)
			assert.Same(t, tt.cbErr, err, "the very error the callback returned")
			assert.Len(t, got, 1, "the stream stopped at once")
			assert.Equal(t, 1, inner.Calls(), "and was not retried")
		})
	}
}

func TestRetrying_ANilReplyIsAnError(t *testing.T) {
	// A model that returns neither a reply nor an error is broken, not
	// transient: it is reported, not retried, and never handed back as a nil
	// reply with a nil error.
	t.Run("Generate", func(t *testing.T) {
		inner := newFakeModel(fakeStep{}, fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		resp, err := r.Generate(context.Background(), fakeRequest(""))

		assert.Nil(t, resp)
		assert.ErrorContains(t, err, noReplyMessage)
		assert.Equal(t, 1, inner.Calls())
	})
	t.Run("Stream", func(t *testing.T) {
		inner := newFakeModel(fakeStep{}, fakeAnswer("m", llm.Usage{}))
		r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})

		resp, err := r.Stream(context.Background(), fakeRequest(""), func(llm.Delta) error { return nil })

		assert.Nil(t, resp)
		assert.ErrorContains(t, err, noReplyMessage)
		assert.Equal(t, 1, inner.Calls())
	})
}

func TestRetrying_PassesTheRequestThrough(t *testing.T) {
	inner := newFakeModel(fakeStep{err: fakeTransient(0)}, fakeAnswer("m", llm.Usage{}))
	r := llm.NewRetrying(inner, llm.RetryOptions{Retry: fastBackoff})
	req := fakeRequest("asked")
	req.System = "be brief"

	_, err := r.Generate(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, []llm.Request{req, req}, inner.Requests(), "each attempt sends the request as given")
}
