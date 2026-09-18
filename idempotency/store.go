package idempotency

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"
)

// ErrInProgress reports that a key is claimed by a request that has not
// completed yet.
var ErrInProgress = errors.New("idempotency: request already in progress")

// ErrClaimLost reports that a claim's lease expired and another request took
// the key before Complete was called. Nothing was stored.
var ErrClaimLost = errors.New("idempotency: claim lost to a newer request")

// Record is the stored outcome of one idempotent request.
type Record struct {
	RequestHash string
	Status      int
	Header      http.Header
	Body        []byte
	ExpiresAt   time.Time
}

// Store persists idempotency keys across requests, and across processes for
// an implementation backed by something other than memory.
//
// A key starts unclaimed, Claim marks it in progress for the length of a
// lease, and Complete records the finished response. A caller that fails
// after a successful Claim should call Release. A claim that is never
// completed or released, because the process died, becomes reclaimable when
// its lease expires.
//
// Every claim carries a claim id. Complete and Release act only on the claim
// they were given, so a request that outlives its lease cannot overwrite or
// remove the claim of the request that took the key after it.
type Store interface {
	// Claim reserves key for a new request whose hash is requestHash.
	//
	// If key is unclaimed, or its record or lease has expired, Claim starts
	// a new claim lasting lease and returns its id with a nil record: the
	// caller must run the handler and call Complete.
	//
	// If key has a completed, unexpired record, Claim returns it with an
	// empty claim id, whether or not rec.RequestHash matches requestHash.
	// The caller decides between replaying it and reporting a conflict.
	//
	// If key is claimed, not completed, and inside its lease, Claim returns
	// ErrInProgress.
	Claim(ctx context.Context, key, requestHash string, lease time.Duration) (claimID string, rec *Record, err error)

	// Complete stores rec as the finished response for the claim claimID
	// holds on key. rec.ExpiresAt is when the record stops being replayed.
	// It returns ErrClaimLost if that claim no longer holds the key.
	Complete(ctx context.Context, key, claimID string, rec Record) error

	// Release abandons the claim claimID holds on key. It does nothing, and
	// is not an error, if that claim no longer holds the key or is already
	// completed.
	Release(ctx context.Context, key, claimID string) error
}

// NewClaimID returns a random claim id, for Store implementations.
func NewClaimID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// evictPerClaim bounds how many entries one Claim inspects for expiry, so
// eviction never holds the mutex for a walk of the whole map.
const evictPerClaim = 16

// MemoryStore is the in-process default [Store]. It does not survive a
// restart and does not coordinate across instances; use idempotency/pg
// where either of those matters.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]*memEntry
}

type memEntry struct {
	done    bool
	claimID string
	// record.ExpiresAt is the lease deadline until done, then the end of
	// the replay window.
	record Record
}

// NewMemoryStore builds an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: map[string]*memEntry{}}
}

// Len reports how many keys are resident, expired or not.
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Claim implements [Store.Claim].
func (s *MemoryStore) Claim(_ context.Context, key, requestHash string, lease time.Duration) (string, *Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if e, ok := s.entries[key]; ok && now.Before(e.record.ExpiresAt) {
		if !e.done {
			return "", nil, ErrInProgress
		}
		rec := e.record
		return "", &rec, nil
	}

	s.evictSomeLocked(now)

	claimID := NewClaimID()
	s.entries[key] = &memEntry{
		claimID: claimID,
		record:  Record{RequestHash: requestHash, ExpiresAt: now.Add(lease)},
	}
	return claimID, nil, nil
}

// Complete implements [Store.Complete].
func (s *MemoryStore) Complete(_ context.Context, key, claimID string, rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok || e.done || e.claimID != claimID {
		return ErrClaimLost
	}
	e.done = true
	e.record = rec
	return nil
}

// Release implements [Store.Release].
func (s *MemoryStore) Release(_ context.Context, key, claimID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.entries[key]; ok && !e.done && e.claimID == claimID {
		delete(s.entries, key)
	}
	return nil
}

// evictSomeLocked deletes the expired entries among up to evictPerClaim it
// looks at. Map iteration starts at a random position, so repeated calls
// sample the whole map over time. Called with s.mu held.
func (s *MemoryStore) evictSomeLocked(now time.Time) {
	seen := 0
	for k, e := range s.entries {
		if seen == evictPerClaim {
			return
		}
		seen++
		if now.After(e.record.ExpiresAt) {
			delete(s.entries, k)
		}
	}
}
