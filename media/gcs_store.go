package media

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"cloud.google.com/go/storage"
)

// Signer signs a GCS object URL for temporary, unauthenticated access. It is
// declared here, not imported from a signing library, because the caller
// supplies it: signing needs a service-account key or IAM credentials that a
// generic Store implementation should never have to know how to obtain.
type Signer interface {
	SignURL(ctx context.Context, bucket, object string, ttl time.Duration) (string, error)
}

// GCSStore implements Store on Google Cloud Storage.
type GCSStore struct {
	bucket       *storage.BucketHandle
	bucketName   string
	cacheControl string
	public       bool
	signer       Signer
	defaultTTL   time.Duration
}

// GCSOption configures a GCSStore. GCSStore has no other exported way to set
// these, keeping NewGCSStore's required arguments to exactly what every
// caller needs and the optional ones opt-in.
type GCSOption func(*GCSStore)

// Public marks the bucket as one that serves objects at a stable public URL
// (https://storage.googleapis.com/<bucket>/<key>) with no signing. Use this
// only for a bucket whose IAM policy already grants public read — it does not
// change the bucket's ACLs itself.
func Public() GCSOption {
	return func(s *GCSStore) { s.public = true }
}

// WithSigner supplies the Signer a private bucket needs for URL. Required
// unless Public() is set; URL returns an error without one.
func WithSigner(signer Signer) GCSOption {
	return func(s *GCSStore) { s.signer = signer }
}

// WithCacheControl sets the Cache-Control header written on every Put. The
// empty default leaves GCS's own default in place.
func WithCacheControl(value string) GCSOption {
	return func(s *GCSStore) { s.cacheControl = value }
}

// WithDefaultTTL sets the signed URL lifetime URL uses when the caller passes
// ttl <= 0. Defaults to one hour.
func WithDefaultTTL(ttl time.Duration) GCSOption {
	return func(s *GCSStore) { s.defaultTTL = ttl }
}

// NewGCSStore wraps bucketName as a Store using client.
func NewGCSStore(client *storage.Client, bucketName string, opts ...GCSOption) *GCSStore {
	s := &GCSStore{
		bucket:     client.Bucket(bucketName),
		bucketName: bucketName,
		defaultTTL: time.Hour,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// sha256MetadataKey is the object metadata key Put writes the hex-encoded
// SHA-256 of the uploaded body under, and reads back to verify the upload.
const sha256MetadataKey = "keel-sha256"

// maxPutAttempts bounds Put's idempotent re-uploads. A checksum mismatch is
// a stable outcome of one attempt, not a transient error that backoff would
// help, so attempts run back to back and the bound stays small.
const maxPutAttempts = 3

// Put implements Store.
//
// Every upload carries the body's MD5, which GCS validates server-side, and
// its hex SHA-256 as object metadata. After the write succeeds, Put reads
// the stored object's metadata back and compares size, MD5, and SHA-256
// against what was sent; on any mismatch it re-uploads — an overwrite of
// the same key, so the retry is idempotent — up to maxPutAttempts times. A
// partial upload that succeeds at the HTTP layer but lands truncated is
// therefore detected and replaced instead of being kept as the canonical
// variant.
func (s *GCSStore) Put(ctx context.Context, key string, body []byte, contentType string) error {
	sum := sha256.Sum256(body)
	wantSHA256 := hex.EncodeToString(sum[:])
	wantMD5 := md5.Sum(body)

	var err error
	for attempt := 1; attempt <= maxPutAttempts; attempt++ {
		err = s.putOnce(ctx, key, body, contentType, wantSHA256, wantMD5[:])
		if err == nil {
			err = s.verifyUpload(ctx, key, int64(len(body)), wantSHA256, wantMD5[:])
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("media: put %q failed after %d attempt(s): %w", key, maxPutAttempts, err)
}

// putOnce performs a single upload attempt with the integrity metadata the
// verification step checks against.
func (s *GCSStore) putOnce(ctx context.Context, key string, body []byte, contentType, sha256Hex string, md5sum []byte) error {
	w := s.bucket.Object(key).NewWriter(ctx)
	w.ContentType = contentType
	if s.cacheControl != "" {
		w.CacheControl = s.cacheControl
	}
	w.MD5 = md5sum
	w.Metadata = map[string]string{sha256MetadataKey: sha256Hex}
	if _, err := w.Write(body); err != nil {
		_ = w.Close() // Close would only return the same error again; the Write error is returned instead
		return fmt.Errorf("media: put %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("media: put %q: %w", key, err)
	}
	return nil
}

// verifyUpload reads the stored object's metadata back and compares it
// against what was sent. GCS computes size, MD5, and CRC32C over the bytes
// it actually stored, so a truncated upload that succeeded at the HTTP
// layer shows up here as a mismatch.
func (s *GCSStore) verifyUpload(ctx context.Context, key string, size int64, wantSHA256 string, wantMD5 []byte) error {
	attrs, err := s.bucket.Object(key).Attrs(ctx)
	if err != nil {
		return fmt.Errorf("media: verify %q: %w", key, err)
	}
	if attrs.Size != size {
		return fmt.Errorf("media: verify %q: size mismatch: stored %d, sent %d", key, attrs.Size, size)
	}
	if !bytes.Equal(attrs.MD5, wantMD5) {
		return fmt.Errorf("media: verify %q: MD5 mismatch", key)
	}
	if attrs.Metadata[sha256MetadataKey] != wantSHA256 {
		return fmt.Errorf("media: verify %q: SHA-256 mismatch", key)
	}
	return nil
}

// Get implements Store.
func (s *GCSStore) Get(ctx context.Context, key string) ([]byte, error) {
	r, err := s.bucket.Object(key).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, fmt.Errorf("media: get %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("media: get %q: %w", key, err)
	}
	defer func() { _ = r.Close() }()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return nil, fmt.Errorf("media: read %q: %w", key, err)
	}
	return buf.Bytes(), nil
}

// Delete implements Store.
func (s *GCSStore) Delete(ctx context.Context, key string) error {
	err := s.bucket.Object(key).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("media: delete %q: %w", key, err)
	}
	return nil
}

// URL implements Store.
func (s *GCSStore) URL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if s.public {
		return fmt.Sprintf("https://storage.googleapis.com/%s/%s", s.bucketName, key), nil
	}
	if s.signer == nil {
		return "", errors.New("media: private GCS store has no Signer configured — pass Public() or WithSigner()")
	}
	if ttl <= 0 {
		ttl = s.defaultTTL
	}
	url, err := s.signer.SignURL(ctx, s.bucketName, key, ttl)
	if err != nil {
		return "", fmt.Errorf("media: sign url for %q: %w", key, err)
	}
	return url, nil
}
