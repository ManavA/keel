package auth

import "net/http"

// sessionsRevokedKey names the boolean field Logout, ResetPassword and
// DeleteAccount each include in their response body, reporting whether their
// own revocation call actually ends a session before it expires (see
// Logout's doc comment for why that is not the same as the request having
// succeeded).
const sessionsRevokedKey = "sessions_revoked"

// writeGenericError answers status with a fixed message chosen only by the
// status code, never by anything in the request. Callers pass a status; they
// never pass a string built from user input. This is what keeps a failed
// login, a missing account and a malformed token indistinguishable from one
// another at the one status code (401) where telling them apart would let a
// caller enumerate which emails are registered.
func writeGenericError(w http.ResponseWriter, status int) {
	http.Error(w, genericErrorBody(status), status)
}

func genericErrorBody(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid credentials"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not found"
	case http.StatusConflict:
		return "an account with this email already exists"
	case http.StatusServiceUnavailable:
		return "unavailable"
	default:
		return "invalid request"
	}
}
