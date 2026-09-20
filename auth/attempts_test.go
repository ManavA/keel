package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postLoginAs posts a login body to the full Router with the request arriving
// from the given client IP, so both the per-IP limiter and the per-account
// throttle see the attempt the way production traffic presents it.
func postLoginAs(t *testing.T, router http.Handler, email, password, ip string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/login", strings.NewReader(body))
	req.RemoteAddr = ip + ":54321"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestLoginPerAccountThrottleEngagesAcrossIPs is the acceptance test for issue
// 27: a spray that stays under the per-IP limiter (few attempts per IP, many
// IPs, one account) must still trip the per-account throttle, while another
// account stays usable, and every attempt must leave an audit row.
func TestLoginPerAccountThrottleEngagesAcrossIPs(t *testing.T) {
	h := newTestService(t)
	ctx := context.Background()

	victimHash, err := HashPassword("victim-password-1")
	require.NoError(t, err)
	require.NoError(t, h.users.Create(ctx, &User{Email: "victim@example.com", PasswordHash: victimHash}))

	bystanderHash, err := HashPassword("bystander-password-1")
	require.NoError(t, err)
	require.NoError(t, h.users.Create(ctx, &User{Email: "bystander@example.com", PasswordHash: bystanderHash}))

	router := h.Router()

	// Spray the victim account from a fresh IP per attempt: 10.0.x.1 addresses
	// never repeat, so the 15/minute per-IP limiter cannot be what stops this.
	var rec *httptest.ResponseRecorder
	sprayed := DefaultAccountRateLimitRequests + 2
	for i := 0; i < sprayed; i++ {
		rec = postLoginAs(t, router, "victim@example.com", "wrong-password", fmt.Sprintf("10.1.%d.1", i))
		if i < DefaultAccountRateLimitRequests {
			require.Equal(t, http.StatusUnauthorized, rec.Code, "attempt %d must fail as bad credentials, not throttled", i+1)
		}
	}
	require.Equal(t, http.StatusTooManyRequests, rec.Code,
		"the spray must trip the per-account throttle even though every IP stayed far under its own limit")

	// The throttle is per account: a different account from a fresh IP logs in
	// normally.
	rec = postLoginAs(t, router, "bystander@example.com", "bystander-password-1", "10.2.0.1")
	require.Equal(t, http.StatusOK, rec.Code, "throttling one account must not affect another")

	// Every attempt left a row: the victim's failures plus the bystander's
	// success.
	victimAttempts, err := h.attempts.Recent(ctx, "victim@example.com", 1000)
	require.NoError(t, err)
	require.Len(t, victimAttempts, sprayed, "each login attempt must be recorded")
	for _, a := range victimAttempts {
		assert.False(t, a.Success)
		assert.NotEmpty(t, a.IP)
		assert.False(t, a.At.IsZero())
	}

	bystanderAttempts, err := h.attempts.Recent(ctx, "bystander@example.com", 10)
	require.NoError(t, err)
	require.NotEmpty(t, bystanderAttempts)
	assert.True(t, bystanderAttempts[0].Success, "a successful login must be recorded too")

	failures, err := h.attempts.FailuresSince(ctx, "victim@example.com", time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, failures, sprayed)
}

// TestLoginThrottleAnswersUnknownEmailIdentically pins the anti-enumeration
// property: hammering an unregistered address trips the same throttle with a
// byte-identical 429, so the throttle cannot be used to learn which emails
// exist.
func TestLoginThrottleAnswersUnknownEmailIdentically(t *testing.T) {
	h := newTestService(t)
	ctx := context.Background()

	hash, err := HashPassword("real-password-1")
	require.NoError(t, err)
	require.NoError(t, h.users.Create(ctx, &User{Email: "known@example.com", PasswordHash: hash}))

	router := h.Router()

	var knownLast, unknownLast *httptest.ResponseRecorder
	for i := 0; i < DefaultAccountRateLimitRequests+1; i++ {
		knownLast = postLoginAs(t, router, "known@example.com", "wrong-password", fmt.Sprintf("10.3.%d.1", i))
		unknownLast = postLoginAs(t, router, "nosuchuser@example.com", "wrong-password", fmt.Sprintf("10.4.%d.1", i))
	}
	require.Equal(t, http.StatusTooManyRequests, knownLast.Code)
	require.Equal(t, http.StatusTooManyRequests, unknownLast.Code)
	assert.Equal(t, knownLast.Body.String(), unknownLast.Body.String(),
		"the throttle response must not depend on whether the email is registered")
}

// TestLoginSuccessIsRecorded checks the audit side in isolation: one good
// login leaves exactly one success row carrying the client IP.
func TestLoginSuccessIsRecorded(t *testing.T) {
	h := newTestService(t)
	ctx := context.Background()

	hash, err := HashPassword("good-password-1")
	require.NoError(t, err)
	require.NoError(t, h.users.Create(ctx, &User{Email: "audited@example.com", PasswordHash: hash}))

	rec := postLoginAs(t, h.Router(), "audited@example.com", "good-password-1", "203.0.113.7")
	require.Equal(t, http.StatusOK, rec.Code)

	attempts, err := h.attempts.Recent(ctx, "audited@example.com", 10)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.True(t, attempts[0].Success)
	assert.Equal(t, "audited@example.com", attempts[0].Email)
	assert.Equal(t, "203.0.113.7", attempts[0].IP)
}

// TestMemoryAttemptStoreCountsFailuresInsideTheWindow pins the store contract
// the throttle depends on: only failures count, only inside the window, newest
// first.
func TestMemoryAttemptStoreCountsFailuresInsideTheWindow(t *testing.T) {
	store := NewMemoryAttemptStore()
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, store.Record(ctx, LoginAttempt{Email: "w@example.com", Success: false, IP: "1.1.1.1", At: now.Add(-time.Hour)}))
	require.NoError(t, store.Record(ctx, LoginAttempt{Email: "w@example.com", Success: false, IP: "1.1.1.2", At: now.Add(-time.Minute)}))
	require.NoError(t, store.Record(ctx, LoginAttempt{Email: "w@example.com", Success: true, IP: "1.1.1.3", At: now}))

	n, err := store.FailuresSince(ctx, "w@example.com", now.Add(-30*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only failures inside the window count")

	recent, err := store.Recent(ctx, "w@example.com", 2)
	require.NoError(t, err)
	require.Len(t, recent, 2)
	assert.True(t, recent[0].At.After(recent[1].At) || recent[0].At.Equal(recent[1].At),
		"recent attempts come back newest first")
	assert.True(t, recent[0].Success, "the newest attempt was the success")

	other, err := store.FailuresSince(ctx, "someone-else@example.com", now.Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, other)
}

// TestLoginResponseBodiesStayFixedStrings decodes nothing account-specific
// from any login outcome: the throttle's 429 joins the existing fixed-string
// contract rather than naming the account or the bucket.
func TestLoginResponseBodiesStayFixedStrings(t *testing.T) {
	h := newTestService(t)
	router := h.Router()

	rec := postLoginAs(t, router, "victim@example.com", "wrong", "10.5.0.1")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "invalid credentials\n", rec.Body.String())

	for i := 0; i < DefaultAccountRateLimitRequests; i++ {
		postLoginAs(t, router, "throttled@example.com", "wrong", fmt.Sprintf("10.6.%d.1", i))
	}
	rec = postLoginAs(t, router, "throttled@example.com", "wrong", "10.6.99.1")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "too many requests\n", rec.Body.String(),
		"the throttle answers a fixed string carrying no account data")
}
