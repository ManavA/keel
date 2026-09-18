package admin

import (
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeedCreatesAnAdminWithAHashedPassword(t *testing.T) {
	store := NewMemoryAdminStore()
	id, err := Seed(context.Background(), store, "owner@example.com", "hunter2hunter2", "Owner", "super_admin")
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	got, err := store.GetByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "owner@example.com", got.Email)
	assert.Equal(t, "super_admin", got.Role)
	assert.NotEqual(t, "hunter2hunter2", got.PasswordHash, "the password must never be stored in plaintext")
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(got.PasswordHash), []byte("hunter2hunter2")))
}

// TestSeedUsesDefaultBcryptCost pins the cost factor: a silent regression to
// a lower cost would pass every other test here, since ComparePassword-style
// round-tripping works at any cost — see auth's identical test for why this
// is worth pinning on its own.
func TestSeedUsesDefaultBcryptCost(t *testing.T) {
	store := NewMemoryAdminStore()
	id, err := Seed(context.Background(), store, "cost@example.com", "hunter2hunter2", "Owner", "admin")
	require.NoError(t, err)

	got, err := store.GetByID(context.Background(), id)
	require.NoError(t, err)

	cost, err := bcrypt.Cost([]byte(got.PasswordHash))
	require.NoError(t, err)
	assert.Equal(t, bcrypt.DefaultCost, cost)
}

func TestSeedRejectsADuplicateEmail(t *testing.T) {
	store := NewMemoryAdminStore()
	_, err := Seed(context.Background(), store, "dup@example.com", "hunter2hunter2", "First", "admin")
	require.NoError(t, err)

	_, err = Seed(context.Background(), store, "dup@example.com", "hunter2hunter2", "Second", "admin")
	assert.ErrorIs(t, err, ErrDuplicateEmail)
}
