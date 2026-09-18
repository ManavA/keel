package media

import (
	"bytes"
	"context"
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

// Put implements Store.
func (s *GCSStore) Put(ctx context.Context, key string, body []byte, contentType string) error {
	w := s.bucket.Object(key).NewWriter(ctx)
	w.ContentType = contentType
	if s.cacheControl != "" {
		w.CacheControl = s.cacheControl
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close() // Close would only return the same error again; the Write error is returned instead
		return fmt.Errorf("media: put %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("media: put %q: %w", key, err)
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
