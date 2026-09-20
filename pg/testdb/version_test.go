package testdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveImage(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		env      string
		want     string
	}{
		{name: "empty uses the default", want: DefaultImage},
		{name: "version from the environment", env: "17", want: "postgres:17-alpine"},
		{name: "explicit image wins over the environment", explicit: "postgis/postgis:16-3.4", env: "17", want: "postgis/postgis:16-3.4"},
		{name: "surrounding spaces are ignored", env: " 17 ", want: "postgres:17-alpine"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvVersion, tt.env)
			assert.Equal(t, tt.want, resolveImage(tt.explicit))
		})
	}
}
