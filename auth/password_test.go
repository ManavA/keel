package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashAndComparePassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.NotEqual(t, "correct horse battery staple", hash)

	assert.True(t, ComparePassword(hash, "correct horse battery staple"))
	assert.False(t, ComparePassword(hash, "wrong password"))
}

func TestComparePasswordAgainstGarbageHash(t *testing.T) {
	assert.False(t, ComparePassword("not-a-bcrypt-hash", "anything"))
}

func TestBurnTimingEqualizerDoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() { burnTimingEqualizer("whatever") })
}

// TestHashPasswordUsesDefaultCost pins the bcrypt cost factor. bcrypt's cost
// is the ONLY thing standing between an offline attacker and a fast guess
// against a leaked hash; a silent regression to a lower cost (accidentally,
// or "to make tests faster") would not fail any functional test, since
// ComparePassword still round-trips correctly at any cost.
func TestHashPasswordUsesDefaultCost(t *testing.T) {
	hash, err := HashPassword("hunter2hunter2")
	require.NoError(t, err)

	cost, err := bcrypt.Cost([]byte(hash))
	require.NoError(t, err)
	assert.Equal(t, bcrypt.DefaultCost, cost)
}
