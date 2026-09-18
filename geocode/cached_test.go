package geocode

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeProvider struct {
	calls  int
	coords *Coordinates
	err    error
}

func (f *fakeProvider) Geocode(_ context.Context, _, _, _, _ string) (*Coordinates, error) {
	f.calls++
	return f.coords, f.err
}

type erroringStore struct {
	getErr, setErr error
}

func (s *erroringStore) Get(context.Context, string) (Coordinates, bool, error) {
	return Coordinates{}, false, s.getErr
}

func (s *erroringStore) Set(context.Context, string, Coordinates) error {
	return s.setErr
}

func TestCached_Geocode(t *testing.T) {
	ctx := context.Background()

	t.Run("a cache hit never calls the underlying provider", func(t *testing.T) {
		store := NewMemoryStore()
		want := Coordinates{Latitude: 1, Longitude: 2, Precision: PrecisionAddress}
		require.NoError(t, store.Set(ctx, NormalizeKey("1 Main St", "Oakland", "CA", "94601"), want))

		provider := &fakeProvider{}
		c := NewCached(provider, store, CachedOptions{})

		got, err := c.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)
		require.Equal(t, want, *got)
		require.Equal(t, 0, provider.calls)
	})

	t.Run("a cache miss calls the provider and populates the cache", func(t *testing.T) {
		store := NewMemoryStore()
		provider := &fakeProvider{coords: &Coordinates{Latitude: 1, Longitude: 2, Precision: PrecisionAddress}}
		c := NewCached(provider, store, CachedOptions{})

		got, err := c.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)
		require.Equal(t, *provider.coords, *got)
		require.Equal(t, 1, provider.calls)

		cached, ok, err := store.Get(ctx, NormalizeKey("1 Main St", "Oakland", "CA", "94601"))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, *provider.coords, cached)
	})

	t.Run("a provider miss (nil, nil) is never cached, so it is retried next time", func(t *testing.T) {
		store := NewMemoryStore()
		provider := &fakeProvider{coords: nil, err: nil}
		c := NewCached(provider, store, CachedOptions{})

		got, err := c.Geocode(ctx, "1 Main St", "Nowhere", "CA", "00000")
		require.NoError(t, err)
		require.Nil(t, got)

		_, ok, err := store.Get(ctx, NormalizeKey("1 Main St", "Nowhere", "CA", "00000"))
		require.NoError(t, err)
		require.False(t, ok, "an unresolved address must not be remembered as permanently unknown")
	})

	t.Run("a Store.Get failure falls through to the provider instead of failing the lookup", func(t *testing.T) {
		provider := &fakeProvider{coords: &Coordinates{Latitude: 5, Longitude: 6, Precision: PrecisionAddress}}
		c := NewCached(provider, &erroringStore{getErr: errors.New("db down")}, CachedOptions{})

		got, err := c.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)
		require.Equal(t, *provider.coords, *got)
		require.Equal(t, 1, provider.calls)
	})

	t.Run("a Store.Set failure does not fail a successful lookup", func(t *testing.T) {
		provider := &fakeProvider{coords: &Coordinates{Latitude: 5, Longitude: 6, Precision: PrecisionAddress}}
		c := NewCached(provider, &erroringStore{setErr: errors.New("db down")}, CachedOptions{})

		got, err := c.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
		require.NoError(t, err)
		require.Equal(t, *provider.coords, *got)
	})

	t.Run("a provider error is returned unchanged", func(t *testing.T) {
		provider := &fakeProvider{err: errors.New("upstream down")}
		c := NewCached(provider, NewMemoryStore(), CachedOptions{})

		_, err := c.Geocode(ctx, "1 Main St", "Oakland", "CA", "94601")
		require.Error(t, err)
	})
}
