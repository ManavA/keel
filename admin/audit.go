package admin

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Outcomes recorded on an AuditEntry: "ok" when the audited handler answered
// with a status below 400, "error" otherwise.
const (
	AuditOutcomeOK    = "ok"
	AuditOutcomeError = "error"
)

// AuditEntry is one row of the admin audit trail: who did what to which
// target, when, and how it turned out.
type AuditEntry struct {
	ID        int64     `json:"id"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Outcome   string    `json:"outcome"`
	CreatedAt time.Time `json:"created_at"`
}

// AuditFilter narrows an audit listing: every non-empty field must match
// exactly. Empty means unfiltered.
type AuditFilter struct {
	Action  string
	Actor   string
	Outcome string
}

// AuditStore is the storage interface for the audit trail. It is
// append-only by construction: there is deliberately no update or delete,
// so the trail is a record of what happened, not a mutable table.
// MemoryAuditStore is a complete in-memory implementation for tests;
// admin/pg provides a Postgres-backed one.
type AuditStore interface {
	// Append records one entry, setting its ID and CreatedAt.
	Append(ctx context.Context, e *AuditEntry) error
	// List returns entries oldest first, narrowed by filter and paged by
	// limit and offset.
	List(ctx context.Context, filter AuditFilter, limit, offset int) ([]AuditEntry, error)
}

// MemoryAuditStore is an in-memory AuditStore, safe for concurrent use.
type MemoryAuditStore struct {
	mu      sync.Mutex
	entries []AuditEntry
	nextID  int64
}

// NewMemoryAuditStore returns an empty MemoryAuditStore.
func NewMemoryAuditStore() *MemoryAuditStore {
	return &MemoryAuditStore{}
}

// Append implements AuditStore.
func (s *MemoryAuditStore) Append(_ context.Context, e *AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	e.ID = s.nextID
	e.CreatedAt = time.Now()
	s.entries = append(s.entries, *e)
	return nil
}

// List implements AuditStore.
func (s *MemoryAuditStore) List(_ context.Context, filter AuditFilter, limit, offset int) ([]AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []AuditEntry
	for _, e := range s.entries {
		if filter.Action != "" && e.Action != filter.Action {
			continue
		}
		if filter.Actor != "" && e.Actor != filter.Actor {
			continue
		}
		if filter.Outcome != "" && e.Outcome != filter.Outcome {
			continue
		}
		matched = append(matched, e)
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(matched) {
		return nil, nil
	}
	out := matched[offset:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return append([]AuditEntry(nil), out...), nil
}

// Audit returns middleware that appends one AuditEntry per request to this
// Service's AuditStore. Mount it inside RequireAdmin so AdminIDFromContext
// carries the actor:
//
//	r.Group(func(r chi.Router) {
//	    r.Use(svc.RequireAdmin)
//	    r.Use(svc.Audit("user.disable", func(r *http.Request) string {
//	        return r.URL.Query().Get("id")
//	    }))
//	    r.Post("/users/disable", disableHandler)
//	})
//
// action names the privileged operation in the caller's own vocabulary; a
// nil target records the request path instead. The outcome is "ok" when the
// handler answers below 400 and "error" otherwise. A failing audit write
// never fails the admin action it records — it is logged instead, the way
// Login treats its own bookkeeping write.
func (s *Service) Audit(action string, target func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			outcome := AuditOutcomeOK
			if rec.status() >= 400 {
				outcome = AuditOutcomeError
			}
			t := r.URL.Path
			if target != nil {
				t = target(r)
			}
			entry := &AuditEntry{
				Actor:   AdminIDFromContext(r.Context()),
				Action:  action,
				Target:  t,
				Outcome: outcome,
			}
			if err := s.audit.Append(r.Context(), entry); err != nil {
				s.logger(r.Context()).Warn("admin: audit entry not recorded",
					"actor", entry.Actor, "action", entry.Action, "error", err)
			}
		})
	}
}

// statusWriter remembers the status a handler answered with, which net/http
// does not expose. Unwrap lets http.ResponseController reach the original
// writer.
type statusWriter struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.code = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.code = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// status reports the status actually sent. A handler that returns without
// writing anything has sent 200 by the time net/http is done with it.
func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// AuditListResponse is the body of GET /audit.
type AuditListResponse struct {
	Entries []AuditEntry `json:"entries"`
}

// AuditList serves GET /audit: the audit trail oldest first, narrowed by
// the action, actor and outcome query parameters and paged by the limit
// (default 50, capped at 200) and offset query parameters.
func (s *Service) AuditList(w http.ResponseWriter, r *http.Request) {
	filter := AuditFilter{
		Action:  r.URL.Query().Get("action"),
		Actor:   r.URL.Query().Get("actor"),
		Outcome: r.URL.Query().Get("outcome"),
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if limit > 200 {
		limit = 200
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		offset = n
	}

	entries, err := s.audit.List(r.Context(), filter, limit, offset)
	if err != nil {
		s.logger(r.Context()).Error("admin: audit list failed", "error", err)
		writeGenericError(w, http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []AuditEntry{}
	}
	writeJSON(w, http.StatusOK, AuditListResponse{Entries: entries})
}
