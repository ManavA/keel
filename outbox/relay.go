package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/retry"
)

// Envelope is what Relay actually publishes. ID is the outbox row's own
// id, given to a consumer as the dedupe key the package doc's at-least-once
// contract requires.
//
// encoding/json base64-encodes a []byte, so Payload travels as base64 text
// inside the envelope, not as the raw bytes [events.Marshal] passes through.
type Envelope struct {
	ID      string `json:"id"`
	Payload []byte `json:"payload"`
}

// conn is the subset of *pgxpool.Pool Relay needs, so a test can fake it.
type conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Options configures a [Relay]. Publisher is required; everything else has
// a default.
type Options struct {
	Publisher events.Publisher

	// PollInterval is how often Run checks for unpublished rows, default 1
	// second.
	PollInterval time.Duration

	// BatchSize is the most rows one poll fetches, default 20.
	BatchSize int

	// PublishRetry configures the backoff around each row's publish call.
	// The zero value uses retry's own defaults.
	PublishRetry retry.Options

	// FailureBackoff is how long a row waits before its next attempt after
	// its first failed tick, default 1 second. The wait doubles with each
	// further failed tick, up to MaxFailureBackoff, default 5 minutes.
	FailureBackoff    time.Duration
	MaxFailureBackoff time.Duration

	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Relay polls the outbox table and publishes unpublished rows. See the
// package doc for its delivery contract.
type Relay struct {
	db   conn
	opts Options
}

// NewRelay builds a Relay over db, which must already have the schema from
// outbox/pg/migrations applied.
func NewRelay(db conn, opts Options) (*Relay, error) {
	if opts.Publisher == nil {
		return nil, fmt.Errorf("outbox: Options.Publisher is required")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 20
	}
	if opts.FailureBackoff <= 0 {
		opts.FailureBackoff = time.Second
	}
	if opts.MaxFailureBackoff <= 0 {
		opts.MaxFailureBackoff = 5 * time.Minute
	}
	if opts.MaxFailureBackoff < opts.FailureBackoff {
		opts.MaxFailureBackoff = opts.FailureBackoff
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Relay{db: db, opts: opts}, nil
}

// Run polls and publishes until ctx is canceled, then returns ctx.Err(). A
// row that fails every retry in one tick is retried again on the next
// tick, rather than stopping the loop.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.opts.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := r.Tick(ctx); err != nil {
				r.opts.Logger.ErrorContext(ctx, "outbox relay: tick failed", "error", err)
			}
		}
	}
}

type row struct {
	id       string
	topic    string
	payload  []byte
	attempts int
}

// Tick runs one poll-publish cycle and reports how many rows it published.
// A row whose publish fails is recorded, given a backoff, and skipped rather
// than stopping the batch. Call Tick directly, instead of Run, to control
// exactly when a cycle happens, as the tests do.
func (r *Relay) Tick(ctx context.Context) (published int, err error) {
	rows, err := r.fetch(ctx)
	if err != nil {
		return 0, err
	}

	for _, rw := range rows {
		publishErr := retry.Do(ctx, func() error {
			return r.opts.Publisher.Publish(ctx, rw.topic, Envelope{ID: rw.id, Payload: rw.payload})
		}, r.opts.PublishRetry)

		if publishErr != nil {
			if markErr := r.recordFailure(ctx, rw, publishErr); markErr != nil {
				r.opts.Logger.ErrorContext(ctx, "outbox relay: record publish failure",
					"id", rw.id, "topic", rw.topic, "publish_error", publishErr, "error", markErr)
			}
			continue
		}

		if markErr := r.markPublished(ctx, rw.id); markErr != nil {
			// Published but not yet marked: the next Tick, on this
			// instance or a restarted one, sees the row as unpublished
			// and republishes it. See the package doc.
			r.opts.Logger.ErrorContext(ctx, "outbox relay: publish succeeded but marking it published failed; "+
				"the row will be republished", "id", rw.id, "topic", rw.topic, "error", markErr)
			continue
		}

		published++
	}

	return published, nil
}

func (r *Relay) fetch(ctx context.Context) ([]row, error) {
	// A failed row is hidden until next_attempt_at, so it cannot hold a slot
	// in every batch; once due it is fetched in age order like any other row.
	rows, err := r.db.Query(ctx,
		"select id, topic, payload, attempts from "+Table+
			" where published_at is null and (next_attempt_at is null or next_attempt_at <= now())"+
			" order by created_at limit $1",
		r.opts.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("outbox: fetch unpublished rows: %w", err)
	}
	defer rows.Close()

	var out []row
	for rows.Next() {
		var rw row
		if err := rows.Scan(&rw.id, &rw.topic, &rw.payload, &rw.attempts); err != nil {
			return nil, fmt.Errorf("outbox: scan unpublished row: %w", err)
		}
		out = append(out, rw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: read unpublished rows: %w", err)
	}
	return out, nil
}

func (r *Relay) markPublished(ctx context.Context, id string) error {
	_, err := r.db.Exec(ctx, "update "+Table+" set published_at = now() where id = $1", id)
	return err
}

func (r *Relay) recordFailure(ctx context.Context, rw row, publishErr error) error {
	delay := failureBackoff(rw.attempts+1, r.opts.FailureBackoff, r.opts.MaxFailureBackoff)
	_, err := r.db.Exec(ctx,
		"update "+Table+" set attempts = attempts + 1, last_error = $2,"+
			" next_attempt_at = now() + $3 * interval '1 microsecond' where id = $1",
		rw.id, publishErr.Error(), delay.Microseconds())
	return err
}

// failureBackoff is base doubled once per failure after the first, capped at
// limit. It is not jittered: a relay's rows do not retry in lockstep with
// anything, and a fixed delay keeps a poisoned row reliably out of the batch.
func failureBackoff(failures int, base, limit time.Duration) time.Duration {
	delay := base
	for i := 1; i < failures; i++ {
		if delay >= limit/2 {
			return limit
		}
		delay *= 2
	}
	return min(delay, limit)
}
