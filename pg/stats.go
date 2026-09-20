package pg

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Stats is a point-in-time snapshot of the pool, projecting the fields of
// pgxpool.Stat a service needs to size and watch its pool: how many
// connections are in use versus idle right now, and the cumulative counters
// that say whether acquirers are waiting.
//
// Poll it on a scrape interval and export Acquired, Idle and Total as gauges;
// the counters only ever rise, so a rate over the interval answers "how often"
// and TotalAcquireWait divided by Acquires answers "how long did the average
// acquire wait".
type Stats struct {
	// Acquired is the number of connections currently checked out and doing
	// work. Under concurrent load this rises toward Max; pinned at Max with
	// latency climbing, the pool is the bottleneck, not the database.
	Acquired int32

	// Idle is the number of open connections sitting free. Persistently high
	// idle with a nonzero MinConns is connections held for no reason.
	Idle int32

	// Total is Acquired plus Idle plus Constructing: every connection the
	// pool currently owns or is opening.
	Total int32

	// Max is the pool's configured ceiling, from Options.MaxConns or the
	// connection string. Total never exceeds it.
	Max int32

	// Constructing is the number of connections being established right now.
	// Acquires stall with Constructing high when the database accepts new
	// connections slowly; they stall with it at zero and Total at Max when
	// the pool is simply too small.
	Constructing int32

	// Acquires counts successful acquisitions since the pool opened. Waits
	// that gave up first never reach it; they land in CanceledAcquires. It
	// is the denominator for TotalAcquireWait.
	Acquires int64

	// EmptyAcquires counts successful acquisitions that arrived to find no
	// free connection and waited, for a new connection to be built or a
	// held one to be released. Rising steadily under load, the pool is
	// saturated; size MaxConns up, or the request concurrency down. An
	// acquire that gives up waiting instead of succeeding is counted in
	// CanceledAcquires, never here.
	EmptyAcquires int64

	// CanceledAcquires counts acquisitions that gave up waiting: the
	// caller's context expired or was cancelled first. Any steady count here
	// is acquirers timing out, the failure the pool previously reported only
	// as slow queries.
	CanceledAcquires int64

	// TotalAcquireWait is the cumulative time successful acquirers spent
	// waiting for a connection. Divided by Acquires it is the mean
	// successful-acquire wait, the number to alert on and the one
	// pg.Acquire's metrics hook records per call.
	TotalAcquireWait time.Duration
}

// Stat snapshots the pool. A nil pool reports the zero Stats rather than
// panicking, so a readiness check or exporter can call it before Open has
// returned.
func Stat(pool *pgxpool.Pool) Stats {
	if pool == nil {
		return Stats{}
	}
	s := pool.Stat()
	if s == nil {
		return Stats{}
	}
	return Stats{
		Acquired:         s.AcquiredConns(),
		Idle:             s.IdleConns(),
		Total:            s.TotalConns(),
		Max:              s.MaxConns(),
		Constructing:     s.ConstructingConns(),
		Acquires:         s.AcquireCount(),
		EmptyAcquires:    s.EmptyAcquireCount(),
		CanceledAcquires: s.CanceledAcquireCount(),
		TotalAcquireWait: s.AcquireDuration(),
	}
}
