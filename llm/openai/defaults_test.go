package openai

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The defaults the options document: nothing else shows them, since a test
// that overrode each would not see what the zero value does.
func TestNew_Defaults(t *testing.T) {
	c, err := New(Options{Model: "m"})
	require.NoError(t, err)

	assert.Equal(t, DefaultBaseURL, c.baseURL)
	assert.Equal(t, "system", c.systemRole)
	assert.Equal(t, 10*time.Minute, c.http.Timeout)
	assert.Same(t, slog.Default(), c.log)
	assert.Zero(t, c.maxTokens, "no bound unless one is set")
	assert.Empty(t, c.apiKey, "no key, no bearer token")
}
