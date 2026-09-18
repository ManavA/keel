package geocode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoopProvider_Geocode(t *testing.T) {
	coords, err := NoopProvider{}.Geocode(context.Background(), "1 Main St", "Oakland", "CA", "94601")
	require.NoError(t, err)
	require.Nil(t, coords, "NoopProvider must report unknown, never a fabricated (0,0) fix")
}
