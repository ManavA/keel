package media

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

func crc32cBase64(data []byte) string {
	sum := crc32.Checksum(data, crc32cTable)
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], sum)
	return base64.StdEncoding.EncodeToString(buf[:])
}

func md5Base64(data []byte) string {
	sum := md5.Sum(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// fakeObject is what the fake GCS server has durably stored for one key.
type fakeObject struct {
	content     []byte
	contentType string
	metadata    map[string]string
}

// fakeGCS is a minimal GCS JSON/XML endpoint for tests: it speaks just
// enough of the protocol for GCSStore (single-shot multipart upload,
// object metadata read, XML download, delete). When truncateFirstUpload is
// set, the first upload stores only the first half of the bytes while the
// upload response still reports the full size and checksums — a partial
// upload that succeeds at the HTTP layer but lands truncated. Metadata
// reads and downloads always report the stored truth, the way the real
// service computes checksums over what it stored. When alwaysTruncate is
// set, every upload is truncated the same way.
type fakeGCS struct {
	mu                 sync.Mutex
	objects            map[string]*fakeObject
	uploads            int
	truncateFirst      bool
	alwaysTruncate     bool
	server             *httptest.Server
	truncatedFirstHalf []byte
}

func newFakeGCS(t *testing.T, truncateFirst, alwaysTruncate bool) *fakeGCS {
	t.Helper()
	f := &fakeGCS{
		objects:        make(map[string]*fakeObject),
		truncateFirst:  truncateFirst,
		alwaysTruncate: alwaysTruncate,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/upload/storage/v1/b/", f.handleUpload)
	mux.HandleFunc("/storage/v1/b/", f.handleJSON)
	mux.HandleFunc("/", f.handleXMLDownload)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGCS) uploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads
}

func (f *fakeGCS) stored(bucket, object string) *fakeObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj := f.objects[bucket+"/"+object]
	if obj == nil {
		return nil
	}
	out := *obj
	return &out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeGCSNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]any{"code": 404, "message": "No such object"},
	})
}

func objectResource(bucket, name, contentType string, content []byte, metadata map[string]string) map[string]any {
	return map[string]any{
		"kind":        "storage#object",
		"name":        name,
		"bucket":      bucket,
		"contentType": contentType,
		"size":        strconv.FormatInt(int64(len(content)), 10),
		"md5Hash":     md5Base64(content),
		"crc32c":      crc32cBase64(content),
		"metadata":    metadata,
	}
}

// handleUpload serves POST /upload/storage/v1/b/{bucket}/o (uploadType=multipart).
func (f *fakeGCS) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bucket := strings.TrimPrefix(r.URL.EscapedPath(), "/upload/storage/v1/b/")
	if i := strings.Index(bucket, "/"); i >= 0 {
		bucket = bucket[:i]
	}
	bucket, _ = url.PathUnescape(bucket)

	mediatype, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediatype, "multipart/") {
		http.Error(w, "expected multipart upload", http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var meta struct {
		Name        string            `json:"name"`
		ContentType string            `json:"contentType"`
		Metadata    map[string]string `json:"metadata"`
	}
	var media []byte
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "bad multipart body", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(part)
		if err != nil {
			http.Error(w, "bad multipart body", http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(part.Header.Get("Content-Type"), "application/json") {
			if err := json.Unmarshal(data, &meta); err != nil {
				http.Error(w, "bad metadata part", http.StatusBadRequest)
				return
			}
		} else {
			media = data
		}
	}
	if meta.Name == "" {
		http.Error(w, "missing object name", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.uploads++
	n := f.uploads
	f.mu.Unlock()

	stored := media
	truncate := (f.truncateFirst && n == 1) || f.alwaysTruncate
	if truncate && len(media) > 1 {
		stored = media[:len(media)/2]
	}

	f.mu.Lock()
	f.objects[bucket+"/"+meta.Name] = &fakeObject{
		content:     append([]byte(nil), stored...),
		contentType: meta.ContentType,
		metadata:    meta.Metadata,
	}
	if f.truncateFirst && n == 1 {
		f.truncatedFirstHalf = append([]byte(nil), stored...)
	}
	f.mu.Unlock()

	// The upload response reports the full received bytes even when the
	// stored object was truncated: the HTTP layer succeeded.
	writeJSON(w, http.StatusOK, objectResource(bucket, meta.Name, meta.ContentType, media, meta.Metadata))
}

// splitObjectPath splits an escaped "/storage/v1/b/{bucket}/o/{object}" path.
func splitObjectPath(escapedPath string) (bucket, object string, ok bool) {
	rest := strings.TrimPrefix(escapedPath, "/storage/v1/b/")
	parts := strings.SplitN(rest, "/o/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	bucket, err1 := url.PathUnescape(parts[0])
	object, err2 := url.PathUnescape(parts[1])
	if err1 != nil || err2 != nil {
		return "", "", false
	}
	return bucket, object, true
}

// handleJSON serves object metadata GET and DELETE.
func (f *fakeGCS) handleJSON(w http.ResponseWriter, r *http.Request) {
	bucket, object, ok := splitObjectPath(r.URL.EscapedPath())
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		obj := f.stored(bucket, object)
		if obj == nil {
			writeGCSNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, objectResource(bucket, object, obj.contentType, obj.content, obj.metadata))
	case http.MethodDelete:
		f.mu.Lock()
		_, found := f.objects[bucket+"/"+object]
		if found {
			delete(f.objects, bucket+"/"+object)
		}
		f.mu.Unlock()
		if !found {
			writeGCSNotFound(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleXMLDownload serves GET /{bucket}/{object} with the stored bytes.
func (f *fakeGCS) handleXMLDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	bucket, err1 := url.PathUnescape(parts[0])
	object, err2 := url.PathUnescape(parts[1])
	if err1 != nil || err2 != nil {
		http.NotFound(w, r)
		return
	}
	obj := f.stored(bucket, object)
	if obj == nil {
		writeGCSNotFound(w)
		return
	}
	w.Header().Set("Content-Type", obj.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.content)
}

func newEmulatorClient(t *testing.T, f *fakeGCS) *storage.Client {
	t.Helper()
	t.Setenv("STORAGE_EMULATOR_HOST", strings.TrimPrefix(f.server.URL, "http://"))
	client, err := storage.NewClient(context.Background(), option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestGCSStore_Put_RetriesTruncatedUpload(t *testing.T) {
	ctx := context.Background()
	fake := newFakeGCS(t, true, false)
	client := newEmulatorClient(t, fake)

	s := NewGCSStore(client, "test-bucket")
	body := bytes.Repeat([]byte("0123456789abcdef"), 64) // 1024 bytes
	require.NoError(t, s.Put(ctx, "photos/1/large/0.jpg", body, "image/jpeg"))

	got, err := s.Get(ctx, "photos/1/large/0.jpg")
	require.NoError(t, err)
	require.Equal(t, body, got, "retry must replace the truncated first upload")
	require.Equal(t, 2, fake.uploadCount(), "truncated first upload should have been retried once")

	stored := fake.stored("test-bucket", "photos/1/large/0.jpg")
	require.NotNil(t, stored)
	sum := sha256.Sum256(body)
	require.Equal(t, hex.EncodeToString(sum[:]), stored.metadata["keel-sha256"],
		"stored object must carry the SHA-256 of the full body")
}

func TestGCSStore_Put_GivesUpAfterRepeatedTruncation(t *testing.T) {
	ctx := context.Background()
	fake := newFakeGCS(t, false, true)
	client := newEmulatorClient(t, fake)

	s := NewGCSStore(client, "test-bucket")
	body := bytes.Repeat([]byte("0123456789abcdef"), 64)
	require.Error(t, s.Put(ctx, "photos/1/large/0.jpg", body, "image/jpeg"))
	require.Equal(t, maxPutAttempts, fake.uploadCount(), "should retry up to the attempt limit")
}
