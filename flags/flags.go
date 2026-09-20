package flags

import (
	"context"
	"errors"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
)

// ErrNotFound reports that no flag with the requested key exists.
var ErrNotFound = errors.New("flags: not found")

// Flag is one feature flag. Percentage is the share of subjects admitted,
// from 0 (none) to 100 (all). Allow lists subject keys that are always
// admitted while the flag is enabled.
type Flag struct {
	Key        string   `json:"key"`
	Enabled    bool     `json:"enabled"`
	Percentage int      `json:"percentage"`
	Allow      []string `json:"allowlist"`
}

// Validate reports whether the flag definition is usable.
func (f Flag) Validate() error {
	if f.Key == "" {
		return errors.New("flags: key is empty")
	}
	if f.Percentage < 0 || f.Percentage > 100 {
		return errors.New("flags: percentage must be between 0 and 100")
	}
	return nil
}

// Evaluate reports whether subject gets the flag. It is pure and
// deterministic: the percentage check hashes the flag key with the subject,
// so the same pair always decides the same way.
func Evaluate(f Flag, subject string) bool {
	if !f.Enabled {
		return false
	}
	for _, a := range f.Allow {
		if a == subject {
			return true
		}
	}
	switch {
	case f.Percentage <= 0:
		return false
	case f.Percentage >= 100:
		return true
	default:
		return bucket(f.Key, subject) < uint64(f.Percentage)
	}
}

// bucket assigns subject to one of 100 buckets for flagKey. FNV-1a is stable
// across processes, unlike the runtime string hash, so a rollout survives a
// restart; the separator keeps ("ab", "c") and ("a", "bc") apart.
func bucket(flagKey, subject string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(flagKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(subject))
	return h.Sum64() % 100
}

// Store keeps flag definitions. Upsert validates before storing.
type Store interface {
	Get(ctx context.Context, key string) (Flag, error)
	List(ctx context.Context) ([]Flag, error)
	Upsert(ctx context.Context, f Flag) error
}

// MemoryStore is the in-process default [Store]. It does not survive a
// restart and does not coordinate across instances; use flags/pg where
// either of those matters.
type MemoryStore struct {
	mu    sync.RWMutex
	flags map[string]Flag
}

// NewMemoryStore builds an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{flags: map[string]Flag{}}
}

// Get implements [Store.Get].
func (s *MemoryStore) Get(_ context.Context, key string) (Flag, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	f, ok := s.flags[key]
	if !ok {
		return Flag{}, ErrNotFound
	}
	return clone(f), nil
}

// List implements [Store.List], ordered by key.
func (s *MemoryStore) List(_ context.Context) ([]Flag, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Flag, 0, len(s.flags))
	for _, f := range s.flags {
		out = append(out, clone(f))
	}
	slices.SortFunc(out, func(a, b Flag) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// Upsert implements [Store.Upsert].
func (s *MemoryStore) Upsert(_ context.Context, f Flag) error {
	if err := f.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.flags[f.Key] = clone(f)
	return nil
}

// clone copies the allowlist so a caller cannot mutate the stored flag, or
// see a later Upsert through a flag it already holds.
func clone(f Flag) Flag {
	f.Allow = append([]string(nil), f.Allow...)
	return f
}
