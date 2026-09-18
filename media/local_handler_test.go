package media

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func getReq(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestLocalStore_Handler(t *testing.T) {
	ctx := context.Background()
	s, err := NewLocalStore(t.TempDir(), "")
	require.NoError(t, err)
	require.NoError(t, s.Put(ctx, "photos/1/large/0.jpg", []byte("pixels"), "image/jpeg"))
	require.NoError(t, s.Put(ctx, "photos/1/large/note.txt", []byte("plain text content"), "text/plain"))

	srv := httptest.NewServer(s.Handler("/media/"))
	defer srv.Close()

	t.Run("serves a stored key at the mounted prefix", func(t *testing.T) {
		resp := getReq(t, srv.URL+"/media/photos/1/large/0.jpg")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("a missing key answers 404, not the whole filesystem", func(t *testing.T) {
		resp := getReq(t, srv.URL+"/media/photos/1/large/no-such-file.jpg")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("serves the exact content type Put was given, not one guessed from the extension", func(t *testing.T) {
		// note.txt has a .txt extension but was declared text/plain via
		// Put; also cover a case where the declared type would NOT match
		// what the extension or a byte-sniff would guess.
		require.NoError(t, s.Put(ctx, "photos/1/large/mislabeled.txt", []byte("{}"), "application/json"))
		resp := getReq(t, srv.URL+"/media/photos/1/large/mislabeled.txt")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	})

	t.Run("always sets X-Content-Type-Options: nosniff", func(t *testing.T) {
		resp := getReq(t, srv.URL+"/media/photos/1/large/0.jpg")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	})

	t.Run("a directory is never listed", func(t *testing.T) {
		resp := getReq(t, srv.URL+"/media/photos/1/large/")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		require.NotContains(t, string(body), "0.jpg", "a directory response must never enumerate its contents")
	})

	t.Run("a directory with no trailing slash is also never listed", func(t *testing.T) {
		resp := getReq(t, srv.URL+"/media/photos/1/large")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("the content-type sidecar itself is never served", func(t *testing.T) {
		resp := getReq(t, srv.URL+"/media/photos/1/large/0.jpg"+contentTypeSuffix)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("a request naming an in-progress temp file is refused", func(t *testing.T) {
		resp := getReq(t, srv.URL+"/media/photos/1/large/0.jpg"+tmpInfix+"deadbeef")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("a non-GET/HEAD method is rejected", func(t *testing.T) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/media/photos/1/large/0.jpg", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})
}

// TestLocalStore_Handler_SymlinkEscape is the HTTP-facing half of
// TestLocalStore_SymlinkEscape: even if a symlink somehow ends up inside the
// store's root, requesting it over Handler must not read the file it points
// to outside the root.
func TestLocalStore_Handler_SymlinkEscape(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("TOP SECRET"), 0o644))
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(rootDir, "link")))

	s, err := NewLocalStore(rootDir, "")
	require.NoError(t, err)

	srv := httptest.NewServer(s.Handler("/media/"))
	defer srv.Close()

	resp := getReq(t, srv.URL+"/media/link/secret.txt")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.NotContains(t, string(body), "TOP SECRET")
}
