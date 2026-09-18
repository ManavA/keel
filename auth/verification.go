package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// VerificationTokenTTL is how long an email-verification link stays valid.
const VerificationTokenTTL = 72 * time.Hour

// ErrVerificationTokenInvalid covers an unknown, expired, or already-used
// verification token. The three are reported identically: which one it was
// is not the caller's business, and distinguishing them would turn the
// endpoint into an oracle for guessing valid tokens.
var ErrVerificationTokenInvalid = errors.New("auth: verification token is invalid or has expired")

// VerificationStore persists email-verification tokens by their hash — never
// the raw token, which is the one-time credential mailed to the user.
type VerificationStore interface {
	Create(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	// ConsumeValid validates tokenHash and invalidates it in the same
	// operation (a token that has been consumed must not validate again). It
	// returns ErrVerificationTokenInvalid for an unknown, expired, or
	// already-consumed token.
	ConsumeValid(ctx context.Context, tokenHash string) (userID string, err error)
	// InvalidateForUser retires every outstanding token for a user, so a
	// resend does not leave an earlier link still working.
	InvalidateForUser(ctx context.Context, userID string) error
}

// HashVerificationToken returns the SHA-256 hex digest stored for a raw
// token. Only the hash is ever persisted.
func HashVerificationToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// GenerateVerificationToken returns a fresh, URL-safe random raw token.
func GenerateVerificationToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type verificationEntry struct {
	userID    string
	expiresAt time.Time
	consumed  bool
}

// MemoryVerificationStore is an in-memory VerificationStore, safe for
// concurrent use. It periodically evicts expired and consumed tokens (see
// sweepInterval in session_store.go) so a long-running process does not grow
// its maps without bound.
type MemoryVerificationStore struct {
	mu      sync.Mutex
	byHash  map[string]*verificationEntry
	byUser  map[string][]string // userID -> hashes issued
	nowFunc func() time.Time
	writes  int
}

// NewMemoryVerificationStore returns an empty MemoryVerificationStore.
func NewMemoryVerificationStore() *MemoryVerificationStore {
	return &MemoryVerificationStore{
		byHash:  make(map[string]*verificationEntry),
		byUser:  make(map[string][]string),
		nowFunc: time.Now,
	}
}

// Create implements VerificationStore.
func (s *MemoryVerificationStore) Create(_ context.Context, userID, tokenHash string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHash[tokenHash] = &verificationEntry{userID: userID, expiresAt: expiresAt}
	s.byUser[userID] = append(s.byUser[userID], tokenHash)
	s.maybeSweepLocked()
	return nil
}

// maybeSweepLocked evicts every expired or already-consumed token once every
// sweepInterval writes. Callers hold s.mu.
func (s *MemoryVerificationStore) maybeSweepLocked() {
	s.writes++
	if s.writes%sweepInterval != 0 {
		return
	}
	now := s.nowFunc()
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

// ConsumeValid implements VerificationStore.
func (s *MemoryVerificationStore) ConsumeValid(_ context.Context, tokenHash string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.byHash[tokenHash]
	if !ok || entry.consumed || s.nowFunc().After(entry.expiresAt) {
		return "", ErrVerificationTokenInvalid
	}
	entry.consumed = true
	return entry.userID, nil
}

// InvalidateForUser implements VerificationStore.
func (s *MemoryVerificationStore) InvalidateForUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, hash := range s.byUser[userID] {
		if entry, ok := s.byHash[hash]; ok {
			entry.consumed = true
		}
	}
	return nil
}
