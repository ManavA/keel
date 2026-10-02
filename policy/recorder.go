package policy

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"
)

// Record is one decision as it is logged.
type Record struct {
	At       time.Time `json:"at"`
	Action   Action    `json:"action"`
	Decision Decision  `json:"decision"`
	// Version is the Policy.Version that decided.
	Version string `json:"version,omitempty"`
}

// Recorder keeps the decision log.
type Recorder interface {
	Record(ctx context.Context, rec Record) error
}

// MemoryRecorder is the in-process Recorder. It is safe for concurrent use.
type MemoryRecorder struct {
	mu   sync.Mutex
	recs []Record
}

// NewMemoryRecorder builds an empty MemoryRecorder.
func NewMemoryRecorder() *MemoryRecorder {
	return &MemoryRecorder{}
}

// Record implements Recorder. It keeps a copy of rec, and cannot fail.
func (m *MemoryRecorder) Record(_ context.Context, rec Record) error {
	rec = rec.clone()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, rec)
	return nil
}

// Records returns a copy of the log, oldest first.
func (m *MemoryRecorder) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, len(m.recs))
	for i, rec := range m.recs {
		out[i] = rec.clone()
	}
	return out
}

// clone copies what a caller could change in place: the attribute map and the
// list of matched rules. The attribute values are not copied.
func (r Record) clone() Record {
	r.Action.Attrs = maps.Clone(r.Action.Attrs)
	r.Decision.Matched = slices.Clone(r.Decision.Matched)
	return r
}

var _ Recorder = (*MemoryRecorder)(nil)
