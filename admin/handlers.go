package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// LoginRequest is the body of POST /login.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// SessionResponse is returned by Login and Refresh.
type SessionResponse struct {
	Token string `json:"token"`
	Admin struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	} `json:"admin"`
}

func sessionResponse(token string, a *Admin) SessionResponse {
	resp := SessionResponse{Token: token}
	resp.Admin.ID = a.ID
	resp.Admin.Email = a.Email
	resp.Admin.Name = a.Name
	resp.Admin.Role = a.Role
	return resp
}

// timingEqualizerHash is compared against on a login miss so that path takes
// about as long as a genuine wrong-password comparison, so response time
// does not reveal which admin emails exist.
var timingEqualizerHash = func() []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte("admin-timing-equalizer"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return hash
}()

// Login handles POST /login.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	email := strings.TrimSpace(strings.ToLower(req.Email))
	admin, err := s.users.GetByEmail(r.Context(), email)
	switch {
	case errors.Is(err, ErrAdminNotFound):
		_ = bcrypt.CompareHashAndPassword(timingEqualizerHash, []byte(req.Password))
		writeGenericError(w, http.StatusUnauthorized)
		return
	case err != nil:
		// A real store failure, not "no such admin" — reporting it as
		// invalid credentials would hide an outage behind what looks like a
		// typed password.
		s.logger(r.Context()).Error("admin: login failed to look up admin", "error", err)
		writeGenericError(w, http.StatusServiceUnavailable)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(req.Password)) != nil {
		writeGenericError(w, http.StatusUnauthorized)
		return
	}

	// Best-effort: a bookkeeping write must not be able to lock an operator
	// out of the console.
	if err := s.users.UpdateLastLogin(r.Context(), admin.ID); err != nil {
		s.logger(r.Context()).Warn("admin: last_login not recorded", "admin_id", admin.ID, "error", err)
	}

	token, err := s.session.IssueToken(admin.ID)
	if err != nil {
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse(token, admin))
}

// Refresh handles POST /refresh (requires RequireAdmin).
func (s *Service) Refresh(w http.ResponseWriter, r *http.Request) {
	adminID := AdminIDFromContext(r.Context())
	if adminID == "" {
		writeGenericError(w, http.StatusUnauthorized)
		return
	}
	admin, err := s.users.GetByID(r.Context(), adminID)
	if err != nil {
		writeGenericError(w, http.StatusUnauthorized)
		return
	}
	token, err := s.session.IssueToken(admin.ID)
	if err != nil {
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse(token, admin))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
