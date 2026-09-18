package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryPasswordResetStoreConsumeValid(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPasswordResetStore()
	hash := HashVerificationToken("raw-token")
	require.NoError(t, store.Create(ctx, "user_1", hash, time.Now().Add(time.Hour)))

	userID, err := store.ConsumeValid(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, "user_1", userID)
}

func TestMemoryPasswordResetStoreCannotConsumeTwice(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPasswordResetStore()
	hash := HashVerificationToken("raw-token")
	require.NoError(t, store.Create(ctx, "user_1", hash, time.Now().Add(time.Hour)))

	_, err := store.ConsumeValid(ctx, hash)
	require.NoError(t, err)
	_, err = store.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, ErrPasswordResetTokenInvalid)
}

func TestMemoryPasswordResetStoreExpired(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPasswordResetStore()
	hash := HashVerificationToken("raw-token")
	require.NoError(t, store.Create(ctx, "user_1", hash, time.Now().Add(-time.Minute)))

	_, err := store.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, ErrPasswordResetTokenInvalid)
}

func TestMemoryPasswordResetStoreInvalidateForUser(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPasswordResetStore()
	hash := HashVerificationToken("raw-token")
	require.NoError(t, store.Create(ctx, "user_1", hash, time.Now().Add(time.Hour)))
	require.NoError(t, store.InvalidateForUser(ctx, "user_1"))

	_, err := store.ConsumeValid(ctx, hash)
	assert.ErrorIs(t, err, ErrPasswordResetTokenInvalid)
}

func TestForgotPasswordAlwaysReturns200(t *testing.T) {
	h := newTestService(t)
	require.NoError(t, h.users.Create(context.Background(), func() *User {
		hash, err := HashPassword("hunter2hunter2")
		require.NoError(t, err)
		return &User{Email: "known@example.com", PasswordHash: hash}
	}()))

	known := doJSON(t, http.HandlerFunc(h.ForgotPassword), "/forgot-password", ForgotPasswordRequest{Email: "known@example.com"})
	unknown := doJSON(t, http.HandlerFunc(h.ForgotPassword), "/forgot-password", ForgotPasswordRequest{Email: "unknown@example.com"})

	assert.Equal(t, http.StatusOK, known.Code)
	assert.Equal(t, http.StatusOK, unknown.Code)
	assert.Equal(t, known.Body.String(), unknown.Body.String())
}

func TestForgotPasswordSendsAResetEmailForAKnownAccount(t *testing.T) {
	h := newTestService(t)
	require.NoError(t, h.users.Create(context.Background(), func() *User {
		hash, err := HashPassword("hunter2hunter2")
		require.NoError(t, err)
		return &User{Email: "known@example.com", PasswordHash: hash}
	}()))

	doJSON(t, http.HandlerFunc(h.ForgotPassword), "/forgot-password", ForgotPasswordRequest{Email: "known@example.com"})

	emailer := h.emailer.(*recordingEmailer)
	require.Len(t, emailer.sentTo, 1)
	assert.Equal(t, "known@example.com", emailer.sentTo[0])
	assert.Equal(t, "password-reset", emailer.kinds[0])
}

func TestResetPasswordFlow(t *testing.T) {
	h := newTestService(t)
	u := &User{Email: "reset@example.com", PasswordHash: mustHash(t, "oldpassword1")}
	require.NoError(t, h.users.Create(context.Background(), u))

	raw, err := GenerateVerificationToken()
	require.NoError(t, err)
	require.NoError(t, h.passwordResets.Create(context.Background(), u.ID, HashVerificationToken(raw), time.Now().Add(time.Hour)))

	rec := doJSON(t, http.HandlerFunc(h.ResetPassword), "/reset-password", ResetPasswordRequest{
		Token: raw, NewPassword: "newpassword1",
	})
	require.Equal(t, http.StatusOK, rec.Code)

	got, err := h.users.GetByEmail(context.Background(), "reset@example.com")
	require.NoError(t, err)
	assert.True(t, ComparePassword(got.PasswordHash, "newpassword1"))
	assert.False(t, ComparePassword(got.PasswordHash, "oldpassword1"))
}

func TestResetPasswordRejectsUnknownToken(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.ResetPassword), "/reset-password", ResetPasswordRequest{
		Token: "never-issued", NewPassword: "newpassword1",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestResetPasswordRejectsShortPassword(t *testing.T) {
	h := newTestService(t)
	rec := doJSON(t, http.HandlerFunc(h.ResetPassword), "/reset-password", ResetPasswordRequest{
		Token: "whatever", NewPassword: "short",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := HashPassword(password)
	require.NoError(t, err)
	return hash
}
