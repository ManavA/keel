package jobs

import (
	"context"
	"sync"
	"time"
)

// RunRecord is one finished job run: when it happened, what it counted, and
// the status and exit code those counts compute to. Status and ExitCode are
// stored rather than recomputed so a query answers what the run reported,
// even if the computation ever changes.
type RunRecord struct {
	// Name is the entry or job name the run belongs to.
	Name string
	// StartedAt and FinishedAt bound the run.
	StartedAt  time.Time
	FinishedAt time.Time
	// Attempted, Succeeded, Failed and Fatal are the run's Outcome counts.
	Attempted int
	Succeeded int
	Failed    int
	Fatal     bool
	// Status is Outcome.Status() for the counts above; ExitCode is
	// Outcome.ExitCode().
	Status   string
	ExitCode int
}

// NewRunRecord builds a RunRecord from a run's name, bounds and Outcome. The
// status and exit code come from the Outcome, so the recorded word cannot
// disagree with the recorded counts.
func NewRunRecord(name string, started, finished time.Time, o Outcome) RunRecord {
	return RunRecord{
		Name:       name,
		StartedAt:  started,
		FinishedAt: finished,
		Attempted:  o.Attempted,
		Succeeded:  o.Succeeded,
		Failed:     o.Failed,
		Fatal:      o.Fatal,
		Status:     o.Status(),
		ExitCode:   o.ExitCode(),
	}
}

// HistoryStore persists run history and answers queries over it. Record is
// called once per finished run; List reads the newest runs back.
//
// jobs/pg is the Postgres implementation, for history that survives a
// restart. MemoryHistoryStore is the in-process default, for tests and for a
// deployment that only needs the last runs while it is up.
type HistoryStore interface {
	// Record persists one finished run.
	Record(ctx context.Context, rec RunRecord) error

	// List returns the newest runs first, up to limit rows. An empty name
	// returns runs for every entry; a non-empty one returns only that
	// entry's. A non-positive limit applies a default.
	List(ctx context.Context, name string, limit int) ([]RunRecord, error)
}

// MemoryHistoryStore is the in-process [HistoryStore]. It does not survive a
// restart; use jobs/pg where that matters.
type MemoryHistoryStore struct {
	mu   sync.Mutex
	runs []RunRecord
}

// NewMemoryHistoryStore builds an empty MemoryHistoryStore.
func NewMemoryHistoryStore() *MemoryHistoryStore {
	return &MemoryHistoryStore{}
}

// Record implements [HistoryStore.Record].
func (s *MemoryHistoryStore) Record(_ context.Context, rec RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, rec)
	return nil
}

// List implements [HistoryStore.List]: newest first, filtered by name when
// one is given, bounded by limit.
func (s *MemoryHistoryStore) List(_ context.Context, name string, limit int) ([]RunRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RunRecord
	for i := len(s.runs) - 1; i >= 0 && len(out) < limit; i-- {
		if name != "" && s.runs[i].Name != name {
			continue
		}
		out = append(out, s.runs[i])
	}
	return out, nil
}
