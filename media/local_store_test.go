package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// failAfterNWrites wraps a real rootFS and fails every WriteFile call after
// the first n have succeeded, letting a test target the write that lands
// after a previous one in the same Put has already landed — the window
// where an overwrite could destroy a previously-good object.
type failAfterNWrites struct {
	rootFS
	writesRemaining int
}

func (f *failAfterNWrites) WriteFile(name string, data []byte, perm os.FileMode) error {
	if f.writesRemaining <= 0 {
		return errors.New("simulated write failure")
	}
	f.writesRemaining--
	return f.rootFS.WriteFile(name, data, perm)
}

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

// TestLocalStore_PutPartialFailure targets the write that lands after
// another one in the same Put has already succeeded — the window in which
// an overwrite could previously destroy a good object by deleting it when
// only the second of its two writes failed.
func TestLocalStore_PutPartialFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("a fresh Put that fails on its second write leaves nothing behind", func(t *testing.T) {
		root, err := os.OpenRoot(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = root.Close() })

		s := &LocalStore{root: root, fs: &failAfterNWrites{rootFS: root, writesRemaining: 1}}

		err = s.Put(ctx, "new-key.txt", []byte("new body"), "text/plain")
		require.Error(t, err)

		_, err = s.Get(ctx, "new-key.txt")
		require.ErrorIs(t, err, ErrNotFound, "a key that never existed must still not exist after a failed Put")
	})

	t.Run("an overwrite that fails on its second write leaves the previous object untouched", func(t *testing.T) {
		root, err := os.OpenRoot(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = root.Close() })

		s := &LocalStore{root: root, fs: root}
		require.NoError(t, s.Put(ctx, "existing-key.txt", []byte("original body"), "text/original"))

		// Now make only the FIRST write of the next Put succeed (the new
		// body), and the second (its content type) fail — the exact
		// sequence that used to delete the already-landed new body,
		// destroying the working object that was there before this Put
		// was even attempted.
		s.fs = &failAfterNWrites{rootFS: root, writesRemaining: 1}
		err = s.Put(ctx, "existing-key.txt", []byte("new body"), "text/new")
		require.Error(t, err)

		s.fs = root // restore real writes for the verification read
		body, err := s.Get(ctx, "existing-key.txt")
		require.NoError(t, err, "the previously-stored object must still be readable")
		require.Equal(t, "original body", string(body), "a failed overwrite must not destroy the original body")
		require.Equal(t, "text/original", s.contentType("existing-key.txt"), "a failed overwrite must not destroy the original content type")
	})
}
