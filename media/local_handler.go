package media

import (
	"net/http"
	"strings"
)

// Handler serves LocalStore's contents over HTTP: GET/HEAD only, no
// directory listing, the exact content type Put was given (falling back to
// net/http's own sniff of the bytes if that metadata is missing), and
// X-Content-Type-Options: nosniff on every response — so a stored file is
// never reinterpreted by a browser as a more dangerous type (an uploaded
// .svg served as image/svg+xml, for instance, is active content at the
// media origin) than the one it was declared to be.
//
// Reads go through the same os.Root LocalStore itself uses for Get and Put,
// so a symlink placed inside the store cannot be used to read a file
// outside it through this handler either.
func (s *LocalStore) Handler(prefix string) http.Handler {
	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		key := strings.TrimPrefix(r.URL.Path, "/")
		if key == "" || strings.Contains(key, contentTypeSuffix) || strings.Contains(key, tmpInfix) {
			// Reject an empty key, a request for the content-type sidecar
			// itself, and anything naming an in-progress or abandoned
			// temporary write.
			http.NotFound(w, r)
			return
		}

		f, err := s.root.Open(key)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = f.Close() }()

		info, err := f.Stat()
		if err != nil || info.IsDir() {
			// A directory is never served or listed.
			http.NotFound(w, r)
			return
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		if contentType := s.contentType(key); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		http.ServeContent(w, r, "", info.ModTime(), f)
	}))
}
