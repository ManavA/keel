package media

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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

	t.Run("a key that tries to escape the root with ../ is neutralized, not followed", func(t *testing.T) {
		// path.Clean("/" + key) collapses the leading ".." segments against
		// the synthetic root before os.Root ever sees them, so the write
		// lands inside root rather than at /etc/passwd. This asserts the
		// outcome that matters — nothing was written outside root — rather
		// than requiring a particular error.
		require.NoError(t, s.Put(ctx, "../../etc/passwd", []byte("x"), "text/plain"))
		body, err := s.Get(ctx, "../../etc/passwd")
		require.NoError(t, err)
		require.Equal(t, []byte("x"), body)
		_, statErr := os.Stat("/etc/passwd.tmp")
		require.True(t, os.IsNotExist(statErr), "must never have touched a real /etc path")
	})

	t.Run("concurrent Puts of the same key do not collide on a shared temp name", func(t *testing.T) {
		const n = 20
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = s.Put(ctx, "hammered.jpg", []byte("value"), "image/jpeg")
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		body, err := s.Get(ctx, "hammered.jpg")
		require.NoError(t, err)
		require.Equal(t, []byte("value"), body)
	})
}

func TestNewLocalStore_RejectsEmptyRoot(t *testing.T) {
	_, err := NewLocalStore("", "")
	require.Error(t, err)
}

// TestLocalStore_PathEscapeControl is the control for the traversal guard: a
// key that does NOT try to escape must be accepted, proving the escape
// tests discriminate rather than everything simply failing.
func TestLocalStore_PathEscapeControl(t *testing.T) {
	s, err := NewLocalStore(t.TempDir(), "")
	require.NoError(t, err)
	require.NoError(t, s.Put(context.Background(), "safe/nested/key.jpg", []byte("x"), "image/jpeg"))
	body, err := s.Get(context.Background(), "safe/nested/key.jpg")
	require.NoError(t, err)
	require.Equal(t, []byte("x"), body)
}

// TestLocalStore_SymlinkEscape reproduces the exploit a plain path check
// cannot catch: a symlink placed inside the store's root pointing at a
// directory outside it. cleanKey's ".." collapsing never sees this, since
// the key itself ("link/secret.txt") contains no traversal segments at
// all — the escape happens when the filesystem resolves "link". os.Root is
// the actual protection here.
func TestLocalStore_SymlinkEscape(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()
	outsideDir := t.TempDir()

	secretPath := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(secretPath, []byte("TOP SECRET"), 0o644))
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(rootDir, "link")))

	s, err := NewLocalStore(rootDir, "")
	require.NoError(t, err)

	t.Run("Get refuses to read through the symlink", func(t *testing.T) {
		_, err := s.Get(ctx, "link/secret.txt")
		require.Error(t, err)
	})

	t.Run("Put refuses to write through the symlink", func(t *testing.T) {
		err := s.Put(ctx, "link/planted.txt", []byte("pwned"), "text/plain")
		require.Error(t, err)
		_, statErr := os.Stat(filepath.Join(outsideDir, "planted.txt"))
		require.True(t, os.IsNotExist(statErr), "must never have written outside the root")
	})
}
