package testdb

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The guard exists so a suite whose database-backed tests all skipped cannot
// read as green. Without this test the guard itself is unchecked.
func TestVerdict(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		uses     int64
		want     int
		wantSaid bool
	}{
		{name: "tests ran and passed", code: 0, uses: 3, want: 0},
		{name: "tests ran and failed", code: 1, uses: 3, want: 1},
		{
			name:     "passed having used the database not once",
			code:     0,
			uses:     0,
			want:     1,
			wantSaid: true,
		},
		{
			name: "already failing, no extra noise",
			code: 2,
			uses: 0,
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			assert.Equal(t, tt.want, verdict(tt.code, tt.uses, &stderr))
			if tt.wantSaid {
				assert.Contains(t, stderr.String(), "no test used")
			} else {
				assert.Empty(t, stderr.String())
			}
		})
	}
}
