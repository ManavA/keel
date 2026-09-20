package testdb_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ManavA/keel/pg/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Setting the version variable must start that major, and the test log must
// name the server version that came up.
func TestStartUsesVersionFromEnvironment(t *testing.T) {
	t.Setenv(testdb.EnvVersion, "17")

	var logs bytes.Buffer
	db := testdb.New(t, testdb.Options{Logf: func(format string, args ...any) {
		fmt.Fprintf(&logs, format+"\n", args...)
	}})

	assert.Equal(t, "postgres:17-alpine", db.Image)
	require.NotEmpty(t, db.ServerVersion, "Start must report the server version it started")
	assert.True(t, strings.HasPrefix(db.ServerVersion, "17."),
		"expected a Postgres 17 server, started %s", db.ServerVersion)
	assert.Contains(t, logs.String(), db.ServerVersion,
		"the test log must name the server version that was started")
}
