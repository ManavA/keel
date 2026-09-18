package media

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Get and by a Store's URL when key does not exist.
var ErrNotFound = errors.New("media: object not found")

// Store puts and fetches opaque byte objects under a key, and mints a URL a
// client can fetch the object from directly — a stable public URL for a
// public bucket, or a signed, expiring one for a private bucket or a local
// filesystem with no public serving path of its own.
//
// Store has no listing, no metadata beyond content type, and no versioning.
// A media pipeline needs exactly these four operations; anything more
// belongs in a dedicated object-storage client rather than in this
// interface, where every implementation would have to support it.
type Store interface {
	// Put writes body under key with the given content type, replacing
	// whatever was there. Calling it twice with the same key and body is not
	// an error — that repeatability is what lets a pipeline re-run stay
	// idempotent.
	Put(ctx context.Context, key string, body []byte, contentType string) error

	// Get reads the object back. Returns ErrNotFound if key does not exist.
	Get(ctx context.Context, key string) ([]byte, error)

	// Delete removes the object. Deleting a key that does not exist is not an
	// error — a caller cleaning up after a failed run should not have to
	// check existence first.
	Delete(ctx context.Context, key string) error

	// URL returns an address a client can fetch key from directly. ttl is a
	// hint: an implementation backed by a public bucket ignores it and
	// returns a stable public URL; one backed by a private bucket returns a
	// signed URL that expires around ttl. ttl <= 0 asks for the
	// implementation's own default.
	URL(ctx context.Context, key string, ttl time.Duration) (string, error)
}
