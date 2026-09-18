package media

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
)

// newTestGCSClient builds a *storage.Client with no credentials and makes no
// network call — constructing a client and calling Bucket() are both local
// operations in this library, which is what lets URL()'s pure string logic
// be tested without a live GCS project (Put/Get/Delete need the real service
// and belong in gcs_live_test.go instead).
func newTestGCSClient(t *testing.T) *storage.Client {
	t.Helper()
	client, err := storage.NewClient(context.Background(), option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type fakeSigner struct {
	url string
	err error
}

func (f *fakeSigner) SignURL(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	return f.url, f.err
}

func TestGCSStore_URL(t *testing.T) {
	ctx := context.Background()
	client := newTestGCSClient(t)

	t.Run("public bucket returns a stable public URL", func(t *testing.T) {
		s := NewGCSStore(client, "my-bucket", Public())
		url, err := s.URL(ctx, "photos/1/large/0.jpg", 0)
		require.NoError(t, err)
		require.Equal(t, "https://storage.googleapis.com/my-bucket/photos/1/large/0.jpg", url)
	})

	t.Run("private bucket with no signer refuses to answer", func(t *testing.T) {
		s := NewGCSStore(client, "my-bucket")
		_, err := s.URL(ctx, "key.jpg", time.Minute)
		require.Error(t, err)
	})

	t.Run("private bucket with a signer delegates to it", func(t *testing.T) {
		signer := &fakeSigner{url: "https://signed.example/key.jpg?sig=abc"}
		s := NewGCSStore(client, "my-bucket", WithSigner(signer))
		url, err := s.URL(ctx, "key.jpg", time.Minute)
		require.NoError(t, err)
		require.Equal(t, signer.url, url)
	})
}
