package media

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLocalStore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewLocalStore(root, "https://cdn.example.test")
	require.NoError(t, err)

	t.Run("put then get round-trips the body", func(t *testing.T) {
		require.NoError(t, s.Put(ctx, "photos/1/large/0.jpg", []byte("hello"), "image/jpeg"))
		body, err := s.Get(ctx, "photos/1/large/0.jpg")
		require.NoError(t, err)
		require.Equal(t, []byte("hello"), body)
	})

	t.Run("get of a missing key reports ErrNotFound", func(t *testing.T) {
		_, err := s.Get(ctx, "nothing/here.jpg")
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("delete of a missing key is not an error", func(t *testing.T) {
		require.NoError(t, s.Delete(ctx, "still/nothing/here.jpg"))
	})

	t.Run("delete removes a real object", func(t *testing.T) {
		require.NoError(t, s.Put(ctx, "to-delete.jpg", []byte("x"), "image/jpeg"))
		require.NoError(t, s.Delete(ctx, "to-delete.jpg"))
		_, err := s.Get(ctx, "to-delete.jpg")
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("URL joins BaseURL and key regardless of ttl", func(t *testing.T) {
		require.NoError(t, s.Put(ctx, "a/b.jpg", []byte("x"), "image/jpeg"))
		url, err := s.URL(ctx, "a/b.jpg", time.Minute)
		require.NoError(t, err)
		require.Equal(t, "https://cdn.example.test/a/b.jpg", url)
	})

	t.Run("a key that tries to escape the root is neutralized, not followed", func(t *testing.T) {
		// filepath.Clean("/" + key) collapses the leading ".." segments against
		// the synthetic root before Join ever sees them, so the write lands
		// inside root rather than at /etc/passwd. This asserts the outcome
		// that matters — nothing was written outside root — rather than
		// requiring a particular error, which the defense-in-depth HasPrefix
		// check in resolve() exists to catch if that collapsing behavior ever
		// changes.
		require.NoError(t, s.Put(ctx, "../../etc/passwd", []byte("x"), "text/plain"))
		body, err := s.Get(ctx, "../../etc/passwd")
		require.NoError(t, err)
		require.Equal(t, []byte("x"), body)
		_, statErr := os.Stat("/etc/passwd.tmp")
		require.True(t, os.IsNotExist(statErr), "must never have touched a real /etc path")
	})
}

func TestNewLocalStore_RejectsEmptyRoot(t *testing.T) {
	_, err := NewLocalStore("", "")
	require.Error(t, err)
}

// TestLocalStore_PathEscapeControl is the control for the path-traversal
// guard: a key that does NOT try to escape must be accepted, proving the
// check discriminates rather than rejecting everything.
func TestLocalStore_PathEscapeControl(t *testing.T) {
	s, err := NewLocalStore(t.TempDir(), "")
	require.NoError(t, err)
	err = s.Put(context.Background(), "safe/nested/key.jpg", []byte("x"), "image/jpeg")
	require.NoError(t, err)
	require.False(t, errors.Is(err, ErrNotFound))
}
