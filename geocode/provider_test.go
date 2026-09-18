package geocode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCoordinates_Approximate(t *testing.T) {
	tests := []struct {
		name      string
		precision string
		want      bool
	}{
		{name: "an exact address is not approximate", precision: PrecisionAddress, want: false},
		{name: "a postal centroid is approximate", precision: PrecisionPostcode, want: true},
		{name: "a place centroid is approximate", precision: PrecisionPlace, want: true},
		{name: "an unrecognised precision is treated as approximate", precision: "", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Coordinates{Precision: tt.precision}
			require.Equal(t, tt.want, c.Approximate())
		})
	}
}
