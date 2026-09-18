package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryVerificationStoreConsumeValid(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVerificationStore()

	raw, err := GenerateVerificationToken()
	require.NoError(t, err)
	hash := HashVerificationToken(raw)

	require.NoError(t, store.Create(ctx, "user_1", hash, time.Now().Add(time.Hour)))

	userID, err := store.ConsumeValid(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, "user_1", userID)
}

func TestMemoryVerificationStoreCannotConsumeTwice(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVerificationStore()
	hash := HashVerificationToken("raw-token")
	require.NoError(t, store.Create(ctx, "user_1", hash, time.Now().Add(time.Hour)))

	_, err := store.ConsumeValid(ctx, hash)
	require.NoError(t, err)

	_, err = store.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, ErrVerificationTokenInvalid)
}

func TestMemoryVerificationStoreExpired(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVerificationStore()
	hash := HashVerificationToken("raw-token")
	require.NoError(t, store.Create(ctx, "user_1", hash, time.Now().Add(-time.Minute)))

	_, err := store.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, ErrVerificationTokenInvalid)
}

func TestMemoryVerificationStoreUnknownToken(t *testing.T) {
	store := NewMemoryVerificationStore()
	_, err := store.ConsumeValid(context.Background(), "never-issued")
	assert.ErrorIs(t, err, ErrVerificationTokenInvalid)
}

func TestMemoryVerificationStoreInvalidateForUser(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVerificationStore()
	hash := HashVerificationToken("raw-token")
	require.NoError(t, store.Create(ctx, "user_1", hash, time.Now().Add(time.Hour)))

	require.NoError(t, store.InvalidateForUser(ctx, "user_1"))

	_, err := store.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, ErrVerificationTokenInvalid)
}

func TestHashVerificationTokenIsDeterministicAndDistinct(t *testing.T) {
	assert.Equal(t, HashVerificationToken("abc"), HashVerificationToken("abc"))
	assert.NotEqual(t, HashVerificationToken("abc"), HashVerificationToken("abd"))
}

func TestGenerateVerificationTokenIsUnique(t *testing.T) {
	a, err := GenerateVerificationToken()
	require.NoError(t, err)
	b, err := GenerateVerificationToken()
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	assert.Len(t, a, 64) // 32 bytes hex-encoded
}
