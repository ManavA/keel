package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newComposedTestService(t *testing.T) *Service {
	t.Helper()
	h, err := NewService(Options{
		Sources:  []Source{SourceLocal, SourceFirebase},
		Users:    NewMemoryUserStore(),
		Verifier: &fakeIDTokenVerifier{},
	})
	require.NoError(t, err)
	return h
}

func TestExchangeIDTokenLinksToExistingAccountWithVerifiedEmail(t *testing.T) {
	h := newComposedTestService(t)
	local := &User{Email: "shared@example.com", PasswordHash: mustHash(t, "hunter2hunter2")}
	require.NoError(t, h.users.Create(context.Background(), local))
	require.NoError(t, h.SetIDTokenVerifier(&fakeIDTokenVerifier{
		identity: Identity{UID: "ext-uid-shared", Email: "shared@example.com", EmailVerified: true},
	}))

	rec := doJSON(t, http.HandlerFunc(h.ExchangeIDToken), "/exchange", ExchangeIDTokenRequest{IDToken: "valid"})
	require.Equal(t, http.StatusOK, rec.Code)

	linked, err := h.users.GetByExternalUID(context.Background(), "ext-uid-shared")
	require.NoError(t, err)
	assert.Equal(t, local.ID, linked.ID, "the identity token must link to the EXISTING local account, not create a second one")

	got, err := h.users.GetByID(context.Background(), local.ID)
	require.NoError(t, err)
	assert.True(t, got.EmailVerified)
}

// TestExchangeIDTokenLinksByEmailCaseInsensitively is the regression test
// for a linking bypass specific to the in-memory store (a citext-backed pg
// store never had this bug): an identity provider's email claim arrives
// capitalized however that provider chose, and comparing it as an exact
// string against a local account registered in lowercase would fail to
// find it — forking a second account for the same person instead of
// linking to the one that already exists.
func TestExchangeIDTokenLinksByEmailCaseInsensitively(t *testing.T) {
	h := newComposedTestService(t)
	local := &User{Email: "mixed-case@example.com", PasswordHash: mustHash(t, "hunter2hunter2")}
	require.NoError(t, h.users.Create(context.Background(), local))
	require.NoError(t, h.SetIDTokenVerifier(&fakeIDTokenVerifier{
		identity: Identity{UID: "ext-uid-case", Email: "Mixed-Case@Example.com", EmailVerified: true},
	}))

	rec := doJSON(t, http.HandlerFunc(h.ExchangeIDToken), "/exchange", ExchangeIDTokenRequest{IDToken: "valid"})
	require.Equal(t, http.StatusOK, rec.Code)

	linked, err := h.users.GetByExternalUID(context.Background(), "ext-uid-case")
	require.NoError(t, err)
	assert.Equal(t, local.ID, linked.ID, "a case-different email claim must still link to the existing account")
}

// An unverified email claim must never link to an existing account.
// Colliding with one that already exists therefore fails closed (409, the
// same answer Signup gives a duplicate email) instead of either linking
// on an unproven claim or silently creating a second account with the same
// address.
func TestExchangeIDTokenFailsClosedOnUnverifiedEmailCollision(t *testing.T) {
	h := newComposedTestService(t)
	local := &User{Email: "unverified-link@example.com", PasswordHash: mustHash(t, "hunter2hunter2")}
	require.NoError(t, h.users.Create(context.Background(), local))
	require.NoError(t, h.SetIDTokenVerifier(&fakeIDTokenVerifier{
		identity: Identity{UID: "ext-uid-unverified", Email: "unverified-link@example.com", EmailVerified: false},
	}))

	rec := doJSON(t, http.HandlerFunc(h.ExchangeIDToken), "/exchange", ExchangeIDTokenRequest{IDToken: "valid"})
	assert.Equal(t, http.StatusConflict, rec.Code)

	_, err := h.users.GetByExternalUID(context.Background(), "ext-uid-unverified")
	assert.ErrorIs(t, err, ErrUserNotFound, "no account should have been created")

	stillUnlinked, err := h.users.GetByID(context.Background(), local.ID)
	require.NoError(t, err)
	assert.Empty(t, stillUnlinked.ExternalUID)
}

func TestSanitizeDisplayName(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantName    string
		wantDropped bool
	}{
		{name: "ordinary name passes through", raw: "Jane Doe", wantName: "Jane Doe"},
		{name: "surrounding space trimmed", raw: "  Jane Doe  ", wantName: "Jane Doe"},
		{name: "internal whitespace collapsed", raw: "Jane    Doe", wantName: "Jane Doe"},
		{name: "tabs and newlines become spaces", raw: "Jane\tDoe\nJr", wantName: "Jane Doe Jr"},
		{name: "control characters stripped", raw: "Jane\x00\x01Doe", wantName: "JaneDoe"},
		{name: "empty input stays empty, not dropped", raw: "", wantName: "", wantDropped: false},
		{name: "only control characters is dropped", raw: "\x00\x01\x02", wantName: "", wantDropped: true},
		{
			name:     "over-long name is clamped",
			raw:      stringOfLength(300, 'a'),
			wantName: stringOfLength(200, 'a'),
		},
		{
			name:     "over-long multi-byte name is clamped by rune, not by byte",
			raw:      stringOfRune(300, 'é'), // 2 bytes each in UTF-8
			wantName: stringOfRune(200, 'é'),
		},
		{
			name:     "zero-width space is stripped, not just control characters",
			raw:      "Jane" + string(rune(0x200B)) + "Doe",
			wantName: "JaneDoe",
		},
		{
			name:     "a bidi override is stripped",
			raw:      "Jane" + string(rune(0x202E)) + "Doe",
			wantName: "JaneDoe",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotDropped := sanitizeDisplayName(tt.raw)
			assert.Equal(t, tt.wantName, gotName)
			assert.Equal(t, tt.wantDropped, gotDropped)
		})
	}
}

func stringOfLength(n int, r byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}

func stringOfRune(n int, r rune) string {
	runes := make([]rune, n)
	for i := range runes {
		runes[i] = r
	}
	return string(runes)
}

func TestExchangeIDTokenLogsAndDropsAnUnusableDisplayName(t *testing.T) {
	h := newComposedTestService(t)
	require.NoError(t, h.SetIDTokenVerifier(&fakeIDTokenVerifier{
		identity: Identity{UID: "ext-uid-badname", Email: "badname@example.com", Name: "\x00\x01\x02", EmailVerified: true},
	}))

	rec := doJSON(t, http.HandlerFunc(h.ExchangeIDToken), "/exchange", ExchangeIDTokenRequest{IDToken: "valid"})
	require.Equal(t, http.StatusOK, rec.Code)

	u, err := h.users.GetByExternalUID(context.Background(), "ext-uid-badname")
	require.NoError(t, err)
	assert.Empty(t, u.Name, "an unusable display name claim must not be stored verbatim")
}
