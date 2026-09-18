package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// contentTypeSuffix marks the sidecar file LocalStore writes next to each
// object to remember the content type Put was given. Neither the object's
// bytes nor its key reliably carry this — Handler needs the exact type the
// caller declared, not a guess from the key's extension or a sniff of a
// possibly-unrelated byte pattern.
const contentTypeSuffix = ".__keel_content_type"

// tmpInfix appears in every temporary name Put writes before renaming into
// place. Handler refuses to serve any key containing it, so an in-progress
// or abandoned write is never exposed even if one is left behind.
const tmpInfix = ".__keel_tmp_"

// rootFS is the subset of *os.Root's methods Put needs. Declared so a
// same-package test can inject a failure at a specific write — the write
// that lands after another one in the same Put has already succeeded is
// otherwise impractical to trigger through the real filesystem. *os.Root
// satisfies this interface without any wrapping.
type rootFS interface {
	MkdirAll(name string, perm os.FileMode) error
	WriteFile(name string, data []byte, perm os.FileMode) error
	Rename(oldname, newname string) error
	Remove(name string) error
}

// LocalStore implements Store on the local filesystem: for tests, local
// development, and any deployment with no object storage at all. It never
// signs anything — URL just joins BaseURL and key — so whatever serves
// BaseURL is responsible for actually exposing the store over HTTP (see
// Handler).
//
// All filesystem access goes through an os.Root opened on the store's root
// directory. os.Root refuses to resolve any path component, including
// through a symbolic link, to a location outside that directory — a plain
// string check on the key cannot catch a symlink placed inside the root
// that points outside it, since the escape happens at the filesystem level,
// not in the path string.
type LocalStore struct {
	root    *os.Root // Get, contentType and Handler need the concrete *os.File from Open
	fs      rootFS   // Put's writes; always root, except in this package's own tests
	baseURL string
}

// NewLocalStore creates the root directory if needed and returns a
// LocalStore rooted there. baseURL is prefixed to a key to form the URL
// LocalStore reports; pass "" if nothing serves these files over HTTP yet.
func NewLocalStore(root, baseURL string) (*LocalStore, error) {
	if root == "" {
		return nil, errors.New("media: local store root must not be empty")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("media: create local store root: %w", err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("media: open local store root: %w", err)
	}
	return &LocalStore{root: r, fs: r, baseURL: strings.TrimRight(baseURL, "/")}, nil
}

// Close releases the root directory handle. A LocalStore used for the life
// of a process does not need to call this.
func (s *LocalStore) Close() error {
	return s.root.Close()
}

// cleanKey normalizes a caller-supplied key to a "/"-free-of-".." relative
// path. This is defense in depth, not the actual security boundary: os.Root
// itself refuses any component that would escape the root, including a
// symlink, so cleanKey exists to reject an empty key and to normalize
// redundant separators, not to be the thing standing between a key and a
// traversal.
func cleanKey(key string) (string, error) {
	if key == "" {
		return "", errors.New("media: key must not be empty")
	}
	cleaned := path.Clean("/" + key)
	return strings.TrimPrefix(cleaned, "/"), nil
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail on any platform Go supports
	return hex.EncodeToString(b[:])
}

// Put implements Store. The object and its content type are each written to
// a uniquely-named temporary file first, and neither is renamed into place
// until both writes have succeeded — so two concurrent Puts of the same key
// never race on one temp path, a reader never observes a partially written
// object, and a Put that fails while overwriting an existing key leaves the
// previous object exactly as it was rather than deleting it: nothing about
// the live key is touched until there is a complete replacement ready to
// swap in. The two renames themselves cannot be made atomic together
// without OS support neither this filesystem nor most others offer; if the
// first rename succeeds and the second does not, the object has been
// updated but the content type may briefly lag behind it, which Handler
// tolerates by falling back to no explicit Content-Type.
func (s *LocalStore) Put(_ context.Context, key string, body []byte, contentType string) error {
	cleaned, err := cleanKey(key)
	if err != nil {
		return err
	}

	if dir := path.Dir(cleaned); dir != "." {
		if err := s.fs.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("media: create directory for %q: %w", key, err)
		}
	}

	bodyTmp := cleaned + tmpInfix + randomSuffix()
	if err := s.fs.WriteFile(bodyTmp, body, 0o644); err != nil {
		return fmt.Errorf("media: write %q: %w", key, err)
	}

	ctPath := cleaned + contentTypeSuffix
	ctTmp := ctPath + tmpInfix + randomSuffix()
	if err := s.fs.WriteFile(ctTmp, []byte(contentType), 0o644); err != nil {
		_ = s.fs.Remove(bodyTmp)
		return fmt.Errorf("media: write content type for %q: %w", key, err)
	}

	if err := s.fs.Rename(bodyTmp, cleaned); err != nil {
		_ = s.fs.Remove(bodyTmp)
		_ = s.fs.Remove(ctTmp)
		return fmt.Errorf("media: finalize %q: %w", key, err)
	}
	if err := s.fs.Rename(ctTmp, ctPath); err != nil {
		return fmt.Errorf("media: finalize content type for %q: %w", key, err)
	}

	return nil
}

// Get implements Store.
func (s *LocalStore) Get(_ context.Context, key string) ([]byte, error) {
	cleaned, err := cleanKey(key)
	if err != nil {
		return nil, err
	}
	body, err := s.root.ReadFile(cleaned)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("media: get %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("media: read %q: %w", key, err)
	}
	return body, nil
}

// contentType reads back the content type Put stored for key, or "" if
// there is none (an object stored some other way, or a missing sidecar).
func (s *LocalStore) contentType(key string) string {
	b, err := s.root.ReadFile(key + contentTypeSuffix)
	if err != nil {
		return ""
	}
	return string(b)
}

// Delete implements Store. Deleting a key that does not exist is not an
// error.
func (s *LocalStore) Delete(_ context.Context, key string) error {
	cleaned, err := cleanKey(key)
	if err != nil {
		return err
	}
	if err := s.root.Remove(cleaned); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("media: delete %q: %w", key, err)
	}
	_ = s.root.Remove(cleaned + contentTypeSuffix) // best-effort; absence is not an error
	return nil
}

// URL implements Store. ttl is ignored: a local filesystem has no signing
// concept, so URL always returns the same BaseURL-prefixed path. It does
// not check that key exists.
func (s *LocalStore) URL(_ context.Context, key string, _ time.Duration) (string, error) {
	if _, err := cleanKey(key); err != nil {
		return "", err
	}
	return s.baseURL + "/" + strings.TrimLeft(key, "/"), nil
}
