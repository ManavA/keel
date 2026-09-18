package geocode

import (
	"context"
	"sync"
)

// MemoryStore is an in-process Store: a single instance's cache, gone on
// restart and not shared across replicas. Good for a single-instance
// deployment, tests, and local development; a service that needs the cache
// to survive a restart or be shared across replicas supplies its own Store.
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[string]Coordinates
}

// NewMemoryStore returns an empty MemoryStore, ready to use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: make(map[string]Coordinates)}
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, key string) (Coordinates, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	coords, ok := s.entries[key]
	return coords, ok, nil
}

// Set implements Store.
func (s *MemoryStore) Set(_ context.Context, key string, coords Coordinates) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = coords
	return nil
}
