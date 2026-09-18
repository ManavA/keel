package media

import "net/http"

// Handler serves LocalStore's root over HTTP, so a service using LocalStore
// with no external object storage can mount one handler and get working
// URLs. The path a request arrives on, after prefix is stripped, is the
// same key Put and Get use, so URL(key) and this handler agree on what a
// key refers to.
//
// This is the only part of media that imports net/http. GCSStore does not
// need it, since a real bucket serves its own objects; a caller that does
// not use LocalStore does not pull in http.FileServer through this
// package.
func (s *LocalStore) Handler(prefix string) http.Handler {
	return http.StripPrefix(prefix, http.FileServer(http.Dir(s.root)))
}
