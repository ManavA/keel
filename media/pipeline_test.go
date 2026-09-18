package media

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeFetcher struct {
	mu    sync.Mutex
	calls int
	body  []byte
	err   error
}

func (f *fakeFetcher) Fetch(_ context.Context, _ string) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.body, f.err
}

func (f *fakeFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeStore struct {
	mu   sync.Mutex
	puts map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{puts: map[string][]byte{}} }

func (s *fakeStore) Put(_ context.Context, key string, body []byte, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts[key] = body
	return nil
}

type fakeRecorder struct {
	mu       sync.Mutex
	done     map[string]map[string]bool
	dropped  map[string]bool
	failNext bool // makes the next RecordVariant call fail, once
}

func newFakeRecorder() *fakeRecorder {
	return &fakeRecorder{done: map[string]map[string]bool{}, dropped: map[string]bool{}}
}

func (r *fakeRecorder) Done(_ context.Context, key string) (map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for k, v := range r.done[key] {
		out[k] = v
	}
	return out, nil
}

func (r *fakeRecorder) RecordVariant(_ context.Context, key, variant string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext {
		r.failNext = false
		return errors.New("simulated recorder failure")
	}
	if r.done[key] == nil {
		r.done[key] = map[string]bool{}
	}
	r.done[key][variant] = true
	return nil
}

func (r *fakeRecorder) RecordDropped(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropped[key] = true
	return nil
}

func deriveOne(key string) Deriver {
	return func(_ context.Context, body []byte) ([]DerivedVariant, error) {
		return []DerivedVariant{{Name: "large", Key: key, Body: body, ContentType: "application/octet-stream"}}, nil
	}
}

func deriveTwo(largeKey, thumbKey string) Deriver {
	return func(_ context.Context, body []byte) ([]DerivedVariant, error) {
		return []DerivedVariant{
			{Name: "large", Key: largeKey, Body: body, ContentType: "application/octet-stream"},
			{Name: "thumb", Key: thumbKey, Body: body, ContentType: "application/octet-stream"},
		}, nil
	}
}

func TestRun(t *testing.T) {
	ctx := context.Background()

	t.Run("skips an item whose expected variants are all already done", func(t *testing.T) {
		fetcher := &fakeFetcher{body: []byte("source")}
		st := newFakeStore()
		rec := newFakeRecorder()
		rec.done["item-1"] = map[string]bool{"large": true}

		items := []Item{{Key: "item-1", SourceURL: "https://example.test/1.jpg", ExpectedVariants: []string{"large"}}}
		results := Run(ctx, items, fetcher, deriveOne("item-1/large.jpg"), st, rec, Options{})

		require.Len(t, results, 1)
		require.True(t, results[0].Skipped)
		require.Equal(t, 0, fetcher.callCount(), "a fully-done item must not be fetched at all")
	})

	t.Run("a partially-done item re-derives everything but only re-Puts the variants not already done", func(t *testing.T) {
		fetcher := &fakeFetcher{body: []byte("source")}
		st := newFakeStore()
		rec := newFakeRecorder()
		rec.done["item-partial"] = map[string]bool{"large": true} // thumb is not done

		items := []Item{{
			Key:              "item-partial",
			SourceURL:        "https://example.test/p.jpg",
			ExpectedVariants: []string{"large", "thumb"},
		}}
		results := Run(ctx, items, fetcher, deriveTwo("item-partial/large.jpg", "item-partial/thumb.jpg"), st, rec, Options{})

		require.False(t, results[0].Skipped, "not every expected variant is done, so the item must not be skipped entirely")
		require.NotContains(t, st.puts, "item-partial/large.jpg", "an already-done variant must not be re-written to the store")
		require.Contains(t, st.puts, "item-partial/thumb.jpg", "the not-yet-done variant must still be stored")
		require.ElementsMatch(t, []string{"large", "thumb"}, results[0].Stored)
	})

	t.Run("an item with no ExpectedVariants is never skipped, even if Recorder reports variants done", func(t *testing.T) {
		fetcher := &fakeFetcher{body: []byte("source")}
		st := newFakeStore()
		rec := newFakeRecorder()
		rec.done["item-no-expected"] = map[string]bool{"large": true}

		items := []Item{{Key: "item-no-expected", SourceURL: "https://example.test/x.jpg"}}
		results := Run(ctx, items, fetcher, deriveOne("item-no-expected/large.jpg"), st, rec, Options{})

		require.False(t, results[0].Skipped, "with nothing to compare against, allDone must not report done")
		require.Equal(t, 1, fetcher.callCount())
	})

	t.Run("fetches, derives, stores and records a new item", func(t *testing.T) {
		fetcher := &fakeFetcher{body: []byte("source")}
		st := newFakeStore()
		rec := newFakeRecorder()

		items := []Item{{Key: "item-2", SourceURL: "https://example.test/2.jpg", ExpectedVariants: []string{"large"}}}
		results := Run(ctx, items, fetcher, deriveOne("item-2/large.jpg"), st, rec, Options{})

		require.Len(t, results, 1)
		require.NoError(t, results[0].Err)
		require.False(t, results[0].Skipped)
		require.Equal(t, []string{"large"}, results[0].Stored)
		require.Equal(t, 1, fetcher.callCount())
		require.Equal(t, []byte("source"), st.puts["item-2/large.jpg"])
		require.True(t, rec.done["item-2"]["large"])
	})

	t.Run("a permanent fetch error drops the item instead of failing it", func(t *testing.T) {
		fetcher := &fakeFetcher{err: errPermanentTest()}
		st := newFakeStore()
		rec := newFakeRecorder()

		items := []Item{{Key: "item-3", SourceURL: "https://example.test/3.jpg", ExpectedVariants: []string{"large"}}}
		results := Run(ctx, items, fetcher, deriveOne("item-3/large.jpg"), st, rec, Options{})

		require.True(t, results[0].Dropped)
		require.Nil(t, results[0].Err)
		require.True(t, rec.dropped["item-3"])
	})

	t.Run("a transient fetch error is reported as a retryable failure, not dropped", func(t *testing.T) {
		fetcher := &fakeFetcher{err: errors.New("connection reset")}
		st := newFakeStore()
		rec := newFakeRecorder()

		items := []Item{{Key: "item-4", SourceURL: "https://example.test/4.jpg", ExpectedVariants: []string{"large"}}}
		results := Run(ctx, items, fetcher, deriveOne("item-4/large.jpg"), st, rec, Options{})

		require.Error(t, results[0].Err)
		require.False(t, results[0].Dropped)
		require.False(t, rec.dropped["item-4"])
	})

	t.Run("a recorder failure after a successful store is a warning, not an item error", func(t *testing.T) {
		fetcher := &fakeFetcher{body: []byte("source")}
		st := newFakeStore()
		rec := newFakeRecorder()
		rec.failNext = true

		items := []Item{{Key: "item-5", SourceURL: "https://example.test/5.jpg", ExpectedVariants: []string{"large"}}}
		results := Run(ctx, items, fetcher, deriveOne("item-5/large.jpg"), st, rec, Options{})

		require.NoError(t, results[0].Err)
		require.Equal(t, []string{"large"}, results[0].Stored, "the variant landed even though recording it failed")
		require.Equal(t, []byte("source"), st.puts["item-5/large.jpg"])
	})

	t.Run("processes every item across a batch under bounded concurrency", func(t *testing.T) {
		fetcher := &fakeFetcher{body: []byte("x")}
		st := newFakeStore()
		rec := newFakeRecorder()

		var items []Item
		for i := 0; i < 20; i++ {
			items = append(items, Item{Key: itemKey(i), SourceURL: "https://example.test", ExpectedVariants: []string{"large"}})
		}
		results := Run(ctx, items, fetcher, deriveOne("v"), st, rec, Options{Concurrency: 3})
		require.Len(t, results, 20)
		for _, r := range results {
			require.NoError(t, r.Err)
		}
	})
}

func itemKey(i int) string {
	return "item-batch-" + string(rune('a'+i))
}

func errPermanentTest() error {
	return errors.Join(ErrPermanent, errors.New("404 from origin"))
}
