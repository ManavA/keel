package admin

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"
)

// ErrAdminNotFound is returned by an AdminStore when no admin matches the
// lookup.
var ErrAdminNotFound = errors.New("admin: admin not found")

// ErrDuplicateEmail is returned by AdminStore.Create when the email is
// already registered.
var ErrDuplicateEmail = errors.New("admin: email already registered")

// Admin is an operator account.
type Admin struct {
	ID           string
	Email        string
	Name         string
	Role         string
	PasswordHash string
	LastLoginAt  time.Time
	CreatedAt    time.Time
}

// AdminStore is the storage interface this package depends on.
type AdminStore interface {
	// GetByEmail returns ErrAdminNotFound when no admin matches.
	GetByEmail(ctx context.Context, email string) (*Admin, error)
	// GetByID returns ErrAdminNotFound when no admin matches.
	GetByID(ctx context.Context, id string) (*Admin, error)
	// Create inserts a new admin and sets its ID. It returns
	// ErrDuplicateEmail when the email is already registered.
	Create(ctx context.Context, a *Admin) error
	// UpdateLastLogin records the current time as the admin's most recent
	// login. A failure here must not block the login it is recording (see
	// Service.Login); it is logged instead.
	UpdateLastLogin(ctx context.Context, id string) error
}

// MemoryAdminStore is an in-memory AdminStore, safe for concurrent use.
type MemoryAdminStore struct {
	mu      sync.Mutex
	byID    map[string]*Admin
	byEmail map[string]string
	nextID  int
}

// NewMemoryAdminStore returns an empty MemoryAdminStore.
func NewMemoryAdminStore() *MemoryAdminStore {
	return &MemoryAdminStore{
		byID:    make(map[string]*Admin),
		byEmail: make(map[string]string),
	}
}

// GetByEmail implements AdminStore.
func (s *MemoryAdminStore) GetByEmail(_ context.Context, email string) (*Admin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byEmail[email]
	if !ok {
		return nil, ErrAdminNotFound
	}
	return cloneAdmin(s.byID[id]), nil
}

// GetByID implements AdminStore.
func (s *MemoryAdminStore) GetByID(_ context.Context, id string) (*Admin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[id]
	if !ok {
		return nil, ErrAdminNotFound
	}
	return cloneAdmin(a), nil
}

// Create implements AdminStore.
func (s *MemoryAdminStore) Create(_ context.Context, a *Admin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Email != "" {
		if _, exists := s.byEmail[a.Email]; exists {
			return ErrDuplicateEmail
		}
	}
	s.nextID++
	id := "admin_" + strconv.Itoa(s.nextID)
	a.ID = id
	a.CreatedAt = time.Now()
	stored := *a
	s.byID[id] = &stored
	if a.Email != "" {
		s.byEmail[a.Email] = id
	}
	return nil
}

// UpdateLastLogin implements AdminStore.
func (s *MemoryAdminStore) UpdateLastLogin(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[id]
	if !ok {
		return ErrAdminNotFound
	}
	a.LastLoginAt = time.Now()
	return nil
}

func cloneAdmin(a *Admin) *Admin {
	if a == nil {
		return nil
	}
	clone := *a
	return &clone
}
