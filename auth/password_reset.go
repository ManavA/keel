package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// PasswordResetTokenTTL is how long a password-reset link stays valid.
const PasswordResetTokenTTL = time.Hour

// ErrPasswordResetTokenInvalid covers an unknown, expired, or already-used
// reset token — reported identically for the same reason as
// ErrVerificationTokenInvalid: telling the three apart would make the
// endpoint an oracle for guessing valid tokens.
var ErrPasswordResetTokenInvalid = errors.New("auth: password reset token is invalid or has expired")

// PasswordResetStore persists password-reset tokens by their hash, mirroring
// VerificationStore. They are kept as separate interfaces (rather than one
// generic "token store") because their tokens must not be interchangeable:
// a leaked verification link must never double as a password reset.
type PasswordResetStore interface {
	Create(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	// ConsumeValid validates tokenHash and invalidates it in the same
	// operation. It returns ErrPasswordResetTokenInvalid for an unknown,
	// expired, or already-consumed token.
	ConsumeValid(ctx context.Context, tokenHash string) (userID string, err error)
	// InvalidateForUser retires every outstanding token for a user.
	InvalidateForUser(ctx context.Context, userID string) error
}

type passwordResetEntry struct {
	userID    string
	expiresAt time.Time
	consumed  bool
}

// MemoryPasswordResetStore is an in-memory PasswordResetStore, safe for
// concurrent use. It periodically evicts expired and consumed tokens (see
// sweepInterval in session_store.go) so a long-running process does not grow
// its maps without bound.
type MemoryPasswordResetStore struct {
	mu     sync.Mutex
	byHash map[string]*passwordResetEntry
	byUser map[string][]string
	writes int
}

// NewMemoryPasswordResetStore returns an empty MemoryPasswordResetStore.
func NewMemoryPasswordResetStore() *MemoryPasswordResetStore {
	return &MemoryPasswordResetStore{
		byHash: make(map[string]*passwordResetEntry),
		byUser: make(map[string][]string),
	}
}

// Create implements PasswordResetStore.
func (s *MemoryPasswordResetStore) Create(_ context.Context, userID, tokenHash string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHash[tokenHash] = &passwordResetEntry{userID: userID, expiresAt: expiresAt}
	s.byUser[userID] = append(s.byUser[userID], tokenHash)
	s.maybeSweepLocked()
	return nil
}

// maybeSweepLocked evicts every expired or already-consumed token once every
// sweepInterval writes. Callers hold s.mu.
func (s *MemoryPasswordResetStore) maybeSweepLocked() {
	s.writes++
	if s.writes%sweepInterval != 0 {
		return
	}
	now := time.Now()
	for hash, entry := range s.byHash {
		if entry.consumed || now.After(entry.expiresAt) {
			delete(s.byHash, hash)
		}
	}
	for userID, hashes := range s.byUser {
		kept := hashes[:0]
		for _, hash := range hashes {
			if _, stillPresent := s.byHash[hash]; stillPresent {
				kept = append(kept, hash)
			}
		}
		if len(kept) == 0 {
			delete(s.byUser, userID)
		} else {
			s.byUser[userID] = kept
		}
	}
}

// ConsumeValid implements PasswordResetStore.
func (s *MemoryPasswordResetStore) ConsumeValid(_ context.Context, tokenHash string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.byHash[tokenHash]
	if !ok || entry.consumed || time.Now().After(entry.expiresAt) {
		return "", ErrPasswordResetTokenInvalid
	}
	entry.consumed = true
	return entry.userID, nil
}

// InvalidateForUser implements PasswordResetStore.
func (s *MemoryPasswordResetStore) InvalidateForUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, hash := range s.byUser[userID] {
		if entry, ok := s.byHash[hash]; ok {
			entry.consumed = true
		}
	}
	return nil
}
