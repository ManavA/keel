package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalStore_Handler(t *testing.T) {
	ctx := context.Background()
	s, err := NewLocalStore(t.TempDir(), "")
	require.NoError(t, err)
	require.NoError(t, s.Put(ctx, "photos/1/large/0.jpg", []byte("pixels"), "image/jpeg"))

	srv := httptest.NewServer(s.Handler("/media/"))
	defer srv.Close()

	get := func(t *testing.T, url string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	t.Run("serves a stored key at the mounted prefix", func(t *testing.T) {
		resp := get(t, srv.URL+"/media/photos/1/large/0.jpg")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("a missing key answers 404, not the whole filesystem", func(t *testing.T) {
		resp := get(t, srv.URL+"/media/photos/1/large/no-such-file.jpg")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}
