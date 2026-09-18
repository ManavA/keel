//go:build live

package media

import (
	"context"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/stretchr/testify/require"
)

// TestGCSStore_Live exercises GCSStore against a real bucket. It requires:
//
//	GCS_LIVE_BUCKET  a bucket the ambient credentials can read and write
//
// Run with: go test -tags=live ./media/... -run Live
//
// This never runs in CI — there is no bucket or credential CI could safely
// hold — and is here so a change to the GCS-facing code has one command that
// proves it against the real service before it ships.
func TestGCSStore_Live(t *testing.T) {
	bucket := os.Getenv("GCS_LIVE_BUCKET")
	if bucket == "" {
		t.Skip("GCS_LIVE_BUCKET not set — skipping live GCS test")
	}

	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	s := NewGCSStore(client, bucket, Public())
	key := "keel-live-test/" + time.Now().UTC().Format("20060102T150405.000000000Z")

	require.NoError(t, s.Put(ctx, key, []byte("keel live test"), "text/plain"))
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	body, err := s.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "keel live test", string(body))

	url, err := s.URL(ctx, key, 0)
	require.NoError(t, err)
	require.Contains(t, url, key)

	require.NoError(t, s.Delete(ctx, key))
	_, err = s.Get(ctx, key)
	require.ErrorIs(t, err, ErrNotFound)
}
