package geocode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	t.Run("get on an empty store misses", func(t *testing.T) {
		_, ok, err := s.Get(ctx, "nowhere")
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("set then get round-trips the value", func(t *testing.T) {
		want := Coordinates{Latitude: 37.8, Longitude: -122.2, Precision: PrecisionAddress}
		require.NoError(t, s.Set(ctx, "key-a", want))

		got, ok, err := s.Get(ctx, "key-a")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, want, got)
	})

	t.Run("distinct keys do not collide", func(t *testing.T) {
		require.NoError(t, s.Set(ctx, "key-b", Coordinates{Latitude: 1}))
		require.NoError(t, s.Set(ctx, "key-c", Coordinates{Latitude: 2}))

		b, _, _ := s.Get(ctx, "key-b")
		c, _, _ := s.Get(ctx, "key-c")
		require.Equal(t, 1.0, b.Latitude)
		require.Equal(t, 2.0, c.Latitude)
	})
}

func TestNormalizeKey(t *testing.T) {
	tests := []struct {
		name           string
		a1, c1, s1, z1 string
		a2, c2, s2, z2 string
		wantSame       bool
	}{
		{
			name: "case and whitespace differences normalize to the same key",
			a1:   "123 Main St", c1: "Springfield", s1: "CA", z1: "94000",
			a2: "123   MAIN st", c2: "springfield", s2: "ca", z2: "94000",
			wantSame: true,
		},
		{
			name: "a different street number is a different key",
			a1:   "123 Main St", c1: "Springfield", s1: "CA", z1: "94000",
			a2: "124 Main St", c2: "Springfield", s2: "CA", z2: "94000",
			wantSame: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k1 := NormalizeKey(tt.a1, tt.c1, tt.s1, tt.z1)
			k2 := NormalizeKey(tt.a2, tt.c2, tt.s2, tt.z2)
			if tt.wantSame {
				require.Equal(t, k1, k2)
			} else {
				require.NotEqual(t, k1, k2)
			}
		})
	}
}
