package media

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeLister struct {
	items         []Item
	backlogBefore int
	backlogAfter  int
	callCount     int
	depthErr      error
}

func (l *fakeLister) Pending(_ context.Context, limit int) ([]Item, error) {
	if limit < len(l.items) {
		return l.items[:limit], nil
	}
	return l.items, nil
}

func (l *fakeLister) BacklogDepth(_ context.Context) (int, error) {
	if l.depthErr != nil {
		return 0, l.depthErr
	}
	l.callCount++
	if l.callCount == 1 {
		return l.backlogBefore, nil
	}
	return l.backlogAfter, nil
}

func TestRunFallback(t *testing.T) {
	ctx := context.Background()

	t.Run("reports backlog before and after, not just this run's counters", func(t *testing.T) {
		lister := &fakeLister{
			items:         []Item{{Key: "a", SourceURL: "https://example.test/a", ExpectedVariants: []string{"large"}}},
			backlogBefore: 42,
			backlogAfter:  41,
		}
		fetcher := &fakeFetcher{body: []byte("x")}
		st := newFakeStore()
		rec := newFakeRecorder()

		report, err := RunFallback(ctx, lister, fetcher, deriveOne("a/large.jpg"), st, rec, FallbackOptions{})
		require.NoError(t, err)
		require.Equal(t, 1, report.Attempted)
		require.Equal(t, 1, report.Succeeded)
		require.Equal(t, 42, report.BacklogBefore)
		require.Equal(t, 41, report.BacklogAfter)
	})

	t.Run("a Lister that cannot report depth after the sweep reports -1, not zero", func(t *testing.T) {
		lister := &fakeLister{backlogBefore: 5}
		lister.depthErr = nil
		// Force the second BacklogDepth call (the "after" call) to fail by
		// swapping in an erroring lister only for that call.
		wrapped := &onceThenErrorLister{fakeLister: lister}
		fetcher := &fakeFetcher{body: []byte("x")}
		st := newFakeStore()
		rec := newFakeRecorder()

		report, err := RunFallback(ctx, wrapped, fetcher, deriveOne("a/large.jpg"), st, rec, FallbackOptions{})
		require.NoError(t, err)
		require.Equal(t, -1, report.BacklogAfter, "unknown must not be reported as zero")
	})

	t.Run("a Lister that cannot report depth up front fails the sweep", func(t *testing.T) {
		lister := &fakeLister{depthErr: errors.New("db unavailable")}
		fetcher := &fakeFetcher{}
		st := newFakeStore()
		rec := newFakeRecorder()

		_, err := RunFallback(ctx, lister, fetcher, deriveOne("a/large.jpg"), st, rec, FallbackOptions{})
		require.Error(t, err)
	})
}

// onceThenErrorLister answers the first BacklogDepth call normally and every
// later one with an error, isolating the "after" measurement's failure path.
type onceThenErrorLister struct {
	*fakeLister
	calls int
}

func (l *onceThenErrorLister) BacklogDepth(ctx context.Context) (int, error) {
	l.calls++
	if l.calls == 1 {
		return l.backlogBefore, nil
	}
	return 0, errors.New("db unavailable on second call")
}
