package auth

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"
)

// ErrUserNotFound is returned by a UserStore when no user matches the lookup.
var ErrUserNotFound = errors.New("auth: user not found")

// ErrDuplicateEmail is returned by UserStore.Create when the email is already
// registered. A Postgres-backed store maps its unique-violation error code to
// this sentinel so the handler can answer 409 without knowing which database
// is behind the interface.
var ErrDuplicateEmail = errors.New("auth: email already registered")

// User is the subset of an account this package needs to authenticate it.
// A caller's own user type usually carries more fields; UserStore
// implementations are free to embed or wrap User rather than return it
// directly, as long as they satisfy the interface.
type User struct {
	ID    string
	Email string
	// Name is a display name, usually populated from an identity provider's
	// claim on first sign-in (see sanitizeDisplayName) rather than typed by
	// hand. Empty for a password signup, which this package never asks a name
	// for.
	Name          string
	PasswordHash  string // empty for an account that only ever signed in via IDTokenVerifier
	ExternalUID   string // e.g. a Firebase UID; empty for a password-only account
	EmailVerified bool
	CreatedAt     time.Time
}

// UserStore is the storage interface the handlers in this package depend on.
// It is deliberately small: enough to authenticate and to run the email
// verification flow, not a general user-management API.
type UserStore interface {
	// GetByEmail returns ErrUserNotFound when no account matches.
	GetByEmail(ctx context.Context, email string) (*User, error)
	// GetByID returns ErrUserNotFound when no account matches.
	GetByID(ctx context.Context, id string) (*User, error)
	// GetByExternalUID returns ErrUserNotFound when no account matches.
	GetByExternalUID(ctx context.Context, uid string) (*User, error)
	// Create inserts a new user and sets its ID. It returns ErrDuplicateEmail
	// when the email is already registered.
	Create(ctx context.Context, u *User) error
	// SetEmailVerified marks the user's email verified.
	SetEmailVerified(ctx context.Context, id string) error
	// SetPasswordHash replaces a user's password hash, for the password-reset
	// flow (and for a local account setting a password for the first time
	// after arriving via an identity token).
	SetPasswordHash(ctx context.Context, id, passwordHash string) error
	// LinkExternalUID attaches an identity-provider UID to an existing user,
	// so a Firebase or OIDC sign-in that matches an existing verified email
	// joins that account instead of creating a second one for the same
	// person. Returns ErrUserNotFound if id does not exist.
	LinkExternalUID(ctx context.Context, id, externalUID string) error
	// Delete permanently removes a user (see Service.DeleteAccount).
	Delete(ctx context.Context, id string) error
}

// MemoryUserStore is an in-memory UserStore, safe for concurrent use. It is a
// complete implementation, not a mock: useful directly in tests and in a
// service too small to need a database yet.
type MemoryUserStore struct {
	mu       sync.Mutex
	byID     map[string]*User
	byEmail  map[string]string // email -> id
	byExtUID map[string]string // external UID -> id
	nextID   int
}

// NewMemoryUserStore returns an empty MemoryUserStore.
func NewMemoryUserStore() *MemoryUserStore {
	return &MemoryUserStore{
		byID:     make(map[string]*User),
		byEmail:  make(map[string]string),
		byExtUID: make(map[string]string),
	}
}

// GetByEmail implements UserStore.
func (s *MemoryUserStore) GetByEmail(_ context.Context, email string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byEmail[email]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneUser(s.byID[id]), nil
}

// GetByID implements UserStore.
func (s *MemoryUserStore) GetByID(_ context.Context, id string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneUser(u), nil
}

// GetByExternalUID implements UserStore.
func (s *MemoryUserStore) GetByExternalUID(_ context.Context, uid string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byExtUID[uid]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneUser(s.byID[id]), nil
}

// Create implements UserStore.
func (s *MemoryUserStore) Create(_ context.Context, u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.Email != "" {
		if _, exists := s.byEmail[u.Email]; exists {
			return ErrDuplicateEmail
		}
	}
	s.nextID++
	id := "user_" + strconv.Itoa(s.nextID)
	u.ID = id
	u.CreatedAt = time.Now()
	stored := *u
	s.byID[id] = &stored
	if u.Email != "" {
		s.byEmail[u.Email] = id
	}
	if u.ExternalUID != "" {
		s.byExtUID[u.ExternalUID] = id
	}
	return nil
}

// SetEmailVerified implements UserStore.
func (s *MemoryUserStore) SetEmailVerified(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return ErrUserNotFound
	}
	u.EmailVerified = true
	return nil
}

// SetPasswordHash implements UserStore.
func (s *MemoryUserStore) SetPasswordHash(_ context.Context, id, passwordHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return ErrUserNotFound
	}
	u.PasswordHash = passwordHash
	return nil
}

// LinkExternalUID implements UserStore.
func (s *MemoryUserStore) LinkExternalUID(_ context.Context, id, externalUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return ErrUserNotFound
	}
	u.ExternalUID = externalUID
	s.byExtUID[externalUID] = id
	return nil
}

// Delete implements UserStore.
func (s *MemoryUserStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return ErrUserNotFound
	}
	delete(s.byID, id)
	if u.Email != "" {
		delete(s.byEmail, u.Email)
	}
	if u.ExternalUID != "" {
		delete(s.byExtUID, u.ExternalUID)
	}
	return nil
}

func cloneUser(u *User) *User {
	if u == nil {
		return nil
	}
	clone := *u
	return &clone
}
