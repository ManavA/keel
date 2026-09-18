package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrSessionNotFound is returned by a SessionStore when a token is unknown,
// expired, or has been revoked.
var ErrSessionNotFound = errors.New("auth: session not found or expired")

// SessionStore backs opaque, database-resident sessions: the token handed to
// a client is a random reference with no meaning of its own, and every
// validation is a lookup this store performs. That lookup is what makes a
// session revocable — deleting the row ends it immediately, everywhere,
// which a self-contained JWT cannot do without an extra deny-list of its own.
// See SessionMode's doc comment for the corresponding limit on SessionJWT.
//
// A persistent implementation should store a HASH of the token, the same way
// [VerificationStore] and [PasswordResetStore] do, rather than the token
// itself — a leaked database dump must not hand out live bearer credentials.
// [MemorySessionStore] stores the token directly because it never leaves
// process memory.
type SessionStore interface {
	// Create issues a new session for userID, valid for ttl, and returns the
	// opaque token a client will present.
	Create(ctx context.Context, userID string, ttl time.Duration) (token string, err error)
	// Validate returns the user ID a token belongs to, or ErrSessionNotFound
	// if it is unknown, expired, or revoked.
	Validate(ctx context.Context, token string) (userID string, err error)
	// Revoke ends a session immediately. Revoking an already-unknown token is
	// not an error — the caller's goal (that token no longer working) is
	// already true.
	Revoke(ctx context.Context, token string) error
	// RevokeAllForUser ends every session belonging to userID. Called after
	// a password change, a password reset, and account deletion — each of
	// those must not leave an earlier, possibly stolen, session usable.
	RevokeAllForUser(ctx context.Context, userID string) error
}

type sessionRecord struct {
	userID    string
	expiresAt time.Time
}

// sweepInterval bounds how often the in-memory stores in this file scan for
// expired entries: every sweepInterval writes, not on every one, since the
// scan is O(n) and a write is the only place these stores would otherwise
// never revisit an entry nobody ever explicitly deletes. Without this, a
// long-running process that issues many short-lived tokens (or receives many
// failed/expired verification attempts) grows its in-memory maps without
// bound — nothing ever removes a session, verification or reset token once
// it expires unless a caller happens to revoke or consume it first.
const sweepInterval = 128

// MemorySessionStore is an in-memory SessionStore, safe for concurrent use.
// It periodically evicts expired sessions (see sweepInterval) so a
// long-running process does not grow this map without bound.
type MemorySessionStore struct {
	mu      sync.Mutex
	byToken map[string]sessionRecord
	byUser  map[string]map[string]struct{} // userID -> set of tokens
	writes  int
}

// NewMemorySessionStore returns an empty MemorySessionStore.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{
		byToken: make(map[string]sessionRecord),
		byUser:  make(map[string]map[string]struct{}),
	}
}

// Create implements SessionStore. It refuses a non-positive ttl: a caller
// bug that computes a negative or zero duration should surface as an
// error, not silently issue a token that is already expired the moment it
// is minted.
func (s *MemorySessionStore) Create(_ context.Context, userID string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("auth: session ttl must be positive, got %s", ttl)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate session token: %w", err)
	}
	token := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.byToken[token] = sessionRecord{userID: userID, expiresAt: time.Now().Add(ttl)}
	if s.byUser[userID] == nil {
		s.byUser[userID] = make(map[string]struct{})
	}
	s.byUser[userID][token] = struct{}{}
	s.maybeSweepLocked()
	return token, nil
}

// Validate implements SessionStore.
func (s *MemorySessionStore) Validate(_ context.Context, token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byToken[token]
	if !ok || time.Now().After(rec.expiresAt) {
		return "", ErrSessionNotFound
	}
	return rec.userID, nil
}

// Revoke implements SessionStore.
func (s *MemorySessionStore) Revoke(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(token)
	return nil
}

// RevokeAllForUser implements SessionStore.
func (s *MemorySessionStore) RevokeAllForUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token := range s.byUser[userID] {
		delete(s.byToken, token)
	}
	delete(s.byUser, userID)
	return nil
}

// deleteLocked removes token from both indexes. Callers hold s.mu.
func (s *MemorySessionStore) deleteLocked(token string) {
	rec, ok := s.byToken[token]
	if !ok {
		return
	}
	delete(s.byToken, token)
	if users := s.byUser[rec.userID]; users != nil {
		delete(users, token)
		if len(users) == 0 {
			delete(s.byUser, rec.userID)
		}
	}
}

// maybeSweepLocked evicts every expired session once every sweepInterval
// writes. Callers hold s.mu.
func (s *MemorySessionStore) maybeSweepLocked() {
	s.writes++
	if s.writes%sweepInterval != 0 {
		return
	}
	now := time.Now()
	for token, rec := range s.byToken {
		if now.After(rec.expiresAt) {
			s.deleteLocked(token)
		}
	}
}

// sessionBackend is what Service actually issues, validates and revokes
// against. It exists so Service can be built on a self-contained JWT or on a
// database-resident opaque token without its handlers knowing which.
type sessionBackend interface {
	Issue(ctx context.Context, userID string) (string, error)
	Validate(ctx context.Context, token string) (userID string, err error)
	// Revoke and RevokeAllForUser end a session, or every session belonging
	// to a user, immediately. A JWT-backed implementation cannot honor
	// either without an external deny-list this package does not keep (see
	// SessionMode); it reports success anyway; see jwtSessionBackend's doc
	// comment for what that means for a caller.
	Revoke(ctx context.Context, token string) error
	RevokeAllForUser(ctx context.Context, userID string) error
	// Revocable reports whether Revoke and RevokeAllForUser can actually end
	// a session before it expires. False under SessionJWT, true under
	// SessionOpaque — a caller-facing handler (Logout, ResetPassword,
	// DeleteAccount) surfaces this rather than answering a bare success that
	// reads the same whether a token actually stopped working or not.
	Revocable() bool
}

// jwtSessionBackend adapts sessionIssuer (see session.go) to sessionBackend.
type jwtSessionBackend struct {
	issuer *sessionIssuer
}

func (b *jwtSessionBackend) Issue(_ context.Context, userID string) (string, error) {
	return b.issuer.IssueToken(userID)
}

func (b *jwtSessionBackend) Validate(_ context.Context, token string) (string, error) {
	return b.issuer.ValidateToken(token)
}

// Revoke is a no-op that reports success. A self-contained JWT remains
// valid, on any server that holds the signing secret, until it expires —
// this package has no deny-list to add a revoked token to. Logout and
// account changes still succeed under SessionJWT because the CLIENT-side
// half of logout (discarding the token) is genuine regardless; the token
// itself keeps working until DefaultTokenTTL (or Options.TokenTTL) elapses.
// A deployment that needs a token to stop working the moment a caller asks
// should use SessionOpaque, where Revoke actually ends the session.
func (b *jwtSessionBackend) Revoke(_ context.Context, _ string) error { return nil }

// RevokeAllForUser has the same limit as Revoke: see its doc comment.
func (b *jwtSessionBackend) RevokeAllForUser(_ context.Context, _ string) error { return nil }

// Revocable implements sessionBackend: always false.
func (b *jwtSessionBackend) Revocable() bool { return false }

// opaqueSessionBackend adapts a SessionStore to sessionBackend.
type opaqueSessionBackend struct {
	store SessionStore
	ttl   time.Duration
}

func (b *opaqueSessionBackend) Issue(ctx context.Context, userID string) (string, error) {
	return b.store.Create(ctx, userID, b.ttl)
}

func (b *opaqueSessionBackend) Validate(ctx context.Context, token string) (string, error) {
	userID, err := b.store.Validate(ctx, token)
	if err != nil {
		return "", ErrInvalidToken
	}
	return userID, nil
}

func (b *opaqueSessionBackend) Revoke(ctx context.Context, token string) error {
	return b.store.Revoke(ctx, token)
}

func (b *opaqueSessionBackend) RevokeAllForUser(ctx context.Context, userID string) error {
	return b.store.RevokeAllForUser(ctx, userID)
}

// Revocable implements sessionBackend: always true.
func (b *opaqueSessionBackend) Revocable() bool { return true }
