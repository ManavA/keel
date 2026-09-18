package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	h, err := NewService(Options{
		Secret:        "test-secret",
		Users:         NewMemoryUserStore(),
		Verifications: NewMemoryVerificationStore(),
		Emailer:       &recordingEmailer{},
		SiteURL:       "https://example.com",
	})
	require.NoError(t, err)
	return h
}

func TestNewServiceZeroValueWorks(t *testing.T) {
	// The zero Options value must succeed and build a working SourceLocal
	// service over in-memory storage: no external service, no Secret, no
	// pre-built stores required.
	h, err := NewService(Options{})
	require.NoError(t, err)
	assert.True(t, h.hasSource(SourceLocal))
	assert.False(t, h.hasSource(SourceFirebase))

	backend, ok := h.session.(*opaqueSessionBackend)
	require.True(t, ok, "the default SessionMode must be opaque")
	assert.Equal(t, DefaultTokenTTL, backend.ttl)
	assert.Equal(t, 15, h.rateLimit.Requests)
	assert.NotNil(t, h.log)

	// And it must actually work end to end.
	token, err := h.session.Issue(context.Background(), "some-user-id")
	require.NoError(t, err)
	assert.NotEmpty(t, token)
}

func TestNewServiceRefusesJWTModeWithoutOptIn(t *testing.T) {
	_, err := NewService(Options{SessionMode: SessionJWT, Secret: "test-secret-placeholder-16bytes"})
	assert.Error(t, err, "SessionJWT must be refused unless AllowUnrevocableSessions is true")
}

func TestNewServiceRequiresSecretForJWTMode(t *testing.T) {
	_, err := NewService(Options{SessionMode: SessionJWT, AllowUnrevocableSessions: true})
	assert.Error(t, err)
}

func TestNewServiceRejectsAShortSecretForJWTMode(t *testing.T) {
	_, err := NewService(Options{SessionMode: SessionJWT, AllowUnrevocableSessions: true, Secret: "too-short"})
	assert.Error(t, err)
}

func TestNewServiceJWTModeIssuesAValidatableToken(t *testing.T) {
	h, err := NewService(Options{
		SessionMode:              SessionJWT,
		AllowUnrevocableSessions: true,
		Secret:                   "test-secret-placeholder-16bytes",
	})
	require.NoError(t, err)

	_, ok := h.session.(*jwtSessionBackend)
	require.True(t, ok)

	token, err := h.session.Issue(context.Background(), "user_1")
	require.NoError(t, err)
	userID, err := h.session.Validate(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "user_1", userID)
}

func TestNewServiceRejectsUnknownSessionMode(t *testing.T) {
	_, err := NewService(Options{SessionMode: "carrier-pigeon"})
	assert.Error(t, err)
}

func TestNewServiceRequiresVerifierForFirebaseSource(t *testing.T) {
	_, err := NewService(Options{Sources: []Source{SourceFirebase}})
	assert.Error(t, err)
}

func TestNewServiceRequiresVerifierForOIDCSource(t *testing.T) {
	_, err := NewService(Options{Sources: []Source{SourceOIDC}})
	assert.Error(t, err)
}

func TestNewServiceAcceptsAVerifierForFirebaseSource(t *testing.T) {
	h, err := NewService(Options{
		Sources:  []Source{SourceFirebase},
		Verifier: &fakeIDTokenVerifier{},
	})
	require.NoError(t, err)
	assert.NotNil(t, h.idVerifier)
}

func TestNewServiceComposesLocalAndFirebase(t *testing.T) {
	h, err := NewService(Options{
		Sources:  []Source{SourceLocal, SourceFirebase},
		Verifier: &fakeIDTokenVerifier{},
	})
	require.NoError(t, err)
	assert.True(t, h.hasSource(SourceLocal))
	assert.True(t, h.hasSource(SourceFirebase))
}

func TestNewServiceRefusesTypedNilVerifier(t *testing.T) {
	var verifier *FirebaseVerifier
	_, err := NewService(Options{
		Sources:  []Source{SourceFirebase},
		Verifier: verifier,
	})
	assert.Error(t, err, "a typed-nil verifier in Options must be refused, the same as SetIDTokenVerifier refuses one directly")
}

// TestSetIDTokenVerifierRefusesTypedNil is the regression test for the bug
// class this package exists to prevent: assigning a nil *FirebaseVerifier to
// an IDTokenVerifier-typed variable produces a non-nil interface (it carries
// a concrete type, *FirebaseVerifier), so a bare `verifier == nil` check
// passes it through, and the first call panics on the nil receiver. A caller
// that builds a *FirebaseVerifier conditionally and always assigns the
// result hits exactly this shape whenever the condition was false.
func TestSetIDTokenVerifierRefusesTypedNil(t *testing.T) {
	h := newTestService(t)

	var verifier *FirebaseVerifier // nil, but typed
	err := h.SetIDTokenVerifier(verifier)

	require.Error(t, err, "a typed-nil *FirebaseVerifier must be refused, not stored")
	assert.Nil(t, h.idVerifier, "the handler must not retain a typed-nil verifier")
}

func TestSetIDTokenVerifierRefusesUntypedNil(t *testing.T) {
	h := newTestService(t)
	err := h.SetIDTokenVerifier(nil)
	require.Error(t, err)
	assert.Nil(t, h.idVerifier)
}

func TestSetIDTokenVerifierAcceptsARealImplementation(t *testing.T) {
	h := newTestService(t)
	err := h.SetIDTokenVerifier(&fakeIDTokenVerifier{identity: Identity{UID: "uid-1"}})
	require.NoError(t, err)
	assert.NotNil(t, h.idVerifier)
}

func TestIsNilInterfaceValue(t *testing.T) {
	var nilPtr *FirebaseVerifier
	var nilMap map[string]string
	var nilSlice []string
	var nilFunc func()
	var nilChan chan int

	tests := []struct {
		name string
		v    any
		want bool
	}{
		{"untyped nil", nil, true},
		{"typed nil pointer", nilPtr, true},
		{"typed nil map", nilMap, true},
		{"typed nil slice", nilSlice, true},
		{"typed nil func", nilFunc, true},
		{"typed nil chan", nilChan, true},
		{"non-nil pointer", &FirebaseVerifier{}, false},
		{"non-pointer value", 42, false},
		{"non-empty string", "hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isNilInterfaceValue(tt.v))
		})
	}
}

// fakeIDTokenVerifier is a minimal, real (non-mock) IDTokenVerifier for tests
// that need one without a live Firebase project.
type fakeIDTokenVerifier struct {
	identity Identity
	err      error
}

func (f *fakeIDTokenVerifier) VerifyIDToken(_ context.Context, _ string) (Identity, error) {
	return f.identity, f.err
}

func (f *fakeIDTokenVerifier) VerifyIDTokenCheckRevoked(_ context.Context, _ string) (Identity, error) {
	return f.identity, f.err
}

type recordingEmailer struct {
	sentTo  []string
	kinds   []string
	failing bool
}

func (e *recordingEmailer) Send(_ context.Context, to, kind, _ string) error {
	if e.failing {
		return assert.AnError
	}
	e.sentTo = append(e.sentTo, to)
	e.kinds = append(e.kinds, kind)
	return nil
}
