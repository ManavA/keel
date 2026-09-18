package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalStore implements Store on the local filesystem: for tests, local
// development, and any deployment with no object storage at all. It never
// signs anything — URL just joins BaseURL and key — so whatever serves
// BaseURL is responsible for actually exposing Root over HTTP.
type LocalStore struct {
	root    string
	baseURL string
}

// NewLocalStore creates the root directory if needed and returns a LocalStore
// rooted there. baseURL is prefixed to a key to form the URL LocalStore
// reports; pass "" if nothing serves these files over HTTP yet.
func NewLocalStore(root, baseURL string) (*LocalStore, error) {
	if root == "" {
		return nil, errors.New("media: local store root must not be empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("media: create local store root: %w", err)
	}
	return &LocalStore{root: root, baseURL: strings.TrimRight(baseURL, "/")}, nil
}

// resolve maps key to a path under root, rejecting anything that would
// escape it. A key is caller-supplied and, in a real pipeline, often derived
// from an upstream feed's own identifiers — it must never be trusted to stay
// inside root on its own.
func (s *LocalStore) resolve(key string) (string, error) {
	if key == "" {
		return "", errors.New("media: key must not be empty")
	}
	cleaned := filepath.Clean("/" + key) // leading slash forces Clean to collapse any ".." to root
	full := filepath.Join(s.root, cleaned)
	if full != s.root && !strings.HasPrefix(full, s.root+string(filepath.Separator)) {
		return "", fmt.Errorf("media: key %q escapes store root", key)
	}
	return full, nil
}

// Put implements Store. It writes to a temporary file and renames into place
// so a reader never observes a partially written object, and so a Put that
// fails partway through never corrupts whatever was there before.
func (s *LocalStore) Put(_ context.Context, key string, body []byte, _ string) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("media: create directory for %q: %w", key, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("media: write %q: %w", key, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("media: finalize %q: %w", key, err)
	}
	return nil
}

// Get implements Store.
func (s *LocalStore) Get(_ context.Context, key string) ([]byte, error) {
	path, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("media: get %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("media: read %q: %w", key, err)
	}
	return body, nil
}

// Delete implements Store.
func (s *LocalStore) Delete(_ context.Context, key string) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("media: delete %q: %w", key, err)
	}
	return nil
}

// URL implements Store. ttl is ignored: a local filesystem has no signing
// concept, so URL always returns the same BaseURL-prefixed path.
func (s *LocalStore) URL(_ context.Context, key string, _ time.Duration) (string, error) {
	if _, err := s.resolve(key); err != nil {
		return "", err
	}
	return s.baseURL + "/" + strings.TrimLeft(key, "/"), nil
}
