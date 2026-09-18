package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSecret is at least minSecretLength bytes, for tests that need a
// Service to build successfully.
const testSecret = "test-secret-at-least-16-bytes"

func newTestService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService(Options{
		Users:  NewMemoryAdminStore(),
		Secret: testSecret,
	})
	require.NoError(t, err)
	return s
}

func TestNewServiceRequiresUsers(t *testing.T) {
	_, err := NewService(Options{Secret: testSecret})
	assert.Error(t, err)
}

func TestNewServiceRequiresSecret(t *testing.T) {
	_, err := NewService(Options{Users: NewMemoryAdminStore()})
	assert.Error(t, err)
}

func TestNewServiceRejectsAShortSecret(t *testing.T) {
	_, err := NewService(Options{Users: NewMemoryAdminStore(), Secret: "too-short"})
	assert.Error(t, err)
}

func TestNewServiceDefaults(t *testing.T) {
	s, err := NewService(Options{Users: NewMemoryAdminStore(), Secret: testSecret})
	require.NoError(t, err)
	assert.Equal(t, DefaultTokenTTL, s.session.ttl)
	assert.Equal(t, 5, s.loginRateLimit.Requests)
	assert.NotNil(t, s.log)
}
