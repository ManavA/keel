package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/metrics"
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

	// MaxAttempts is how many failed publish attempts one row gets before
	// Relay parks it and stops trying. Zero or negative means no cap: a
	// poisoned row is retried at the capped backoff interval forever.
	MaxAttempts int

	// OrderedPartitions makes Relay publish rows sharing a non-empty
	// partition key (see Event.PartitionKey) in creation order: a row is
	// held out of the batch while an older unpublished row with the same
	// key exists, and a row that fails holds the rest of its partition
	// for the remainder of the tick. Rows with an empty key publish
	// independently, as do rows in different partitions. A parked head
	// row does not block its partition: it will never publish, so the
	// rows behind it flow. False keeps the long-standing behavior, where
	// a backing-off head row does not hold the rows behind it.
	OrderedPartitions bool

	// Metrics receives one publish count, lag and attempt count per
	// relayed row, and one failure count per failed attempt. Nil records
	// nothing.
	Metrics *metrics.Metrics

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
	id           string
	topic        string
	payload      []byte
	partitionKey string
	attempts     int
	createdAt    time.Time
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

	// blocked holds the partitions whose head row already failed in this
	// tick. The fetch holds a partition's later rows out of the batch when
	// the head is still unpublished, but a head that fails mid-tick leaves
	// its partition's later rows already fetched, so they are skipped
	// here instead of being attempted out of order.
	blocked := map[string]bool{}

	for _, rw := range rows {
		if r.opts.OrderedPartitions && rw.partitionKey != "" && blocked[rw.partitionKey] {
			continue
		}

		publishErr := retry.Do(ctx, func() error {
			return r.opts.Publisher.Publish(ctx, rw.topic, Envelope{ID: rw.id, Payload: rw.payload})
		}, r.opts.PublishRetry)

		if publishErr != nil {
			r.opts.Metrics.ObserveOutboxFailed(ctx, rw.topic, rw.attempts+1)
			if markErr := r.recordFailure(ctx, rw, publishErr); markErr != nil {
				r.opts.Logger.ErrorContext(ctx, "outbox relay: record publish failure",
					"id", rw.id, "topic", rw.topic, "publish_error", publishErr, "error", markErr)
			}
			if r.opts.OrderedPartitions && rw.partitionKey != "" {
				blocked[rw.partitionKey] = true
			}
			continue
		}

		if markErr := r.markPublished(ctx, rw.id); markErr != nil {
			// Published but not yet marked: the next Tick, on this
			// instance or a restarted one, sees the row as unpublished
			// and republishes it. See the package doc.
			r.opts.Logger.ErrorContext(ctx, "outbox relay: publish succeeded but marking it published failed; "+
				"the row will be republished", "id", rw.id, "topic", rw.topic, "error", markErr)
			if r.opts.OrderedPartitions && rw.partitionKey != "" {
				blocked[rw.partitionKey] = true
			}
			continue
		}

		r.opts.Metrics.ObserveOutboxPublished(ctx, rw.topic, time.Since(rw.createdAt), rw.attempts+1)
		published++
	}

	return published, nil
}

func (r *Relay) fetch(ctx context.Context) ([]row, error) {
	// A failed row is hidden until next_attempt_at, so it cannot hold a slot
	// in every batch; once due it is fetched in age order like any other row.
	// A parked row is never fetched again.
	//
	// In ordered mode a row with a non-empty partition key is additionally
	// held out while an older row with the same key is still unpublished,
	// so a head row hidden in its backoff still holds its partition. Rows
	// with an empty key, and the head row of each partition, pass through.
	query := "select id, topic, payload, partition_key, attempts, created_at from " + Table +
		" where published_at is null and parked_at is null" +
		" and (next_attempt_at is null or next_attempt_at <= now())"
	if r.opts.OrderedPartitions {
		query += " and (partition_key = '' or not exists (select 1 from " + Table + " older" +
			" where older.partition_key = " + Table + ".partition_key" +
			" and older.published_at is null and older.parked_at is null" +
			" and older.created_at < " + Table + ".created_at))"
	}
	query += " order by created_at limit $1"
	rows, err := r.db.Query(ctx, query, r.opts.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("outbox: fetch unpublished rows: %w", err)
	}
	defer rows.Close()

	var out []row
	for rows.Next() {
		var rw row
		if err := rows.Scan(&rw.id, &rw.topic, &rw.payload, &rw.partitionKey, &rw.attempts, &rw.createdAt); err != nil {
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
	if r.opts.MaxAttempts > 0 && rw.attempts+1 >= r.opts.MaxAttempts {
		_, err := r.db.Exec(ctx,
			"update "+Table+" set attempts = attempts + 1, last_error = $2,"+
				" parked_at = now(), next_attempt_at = null where id = $1",
			rw.id, publishErr.Error())
		if err != nil {
			return err
		}
		r.opts.Logger.WarnContext(ctx, "outbox relay: parking row after max attempts",
			"id", rw.id, "topic", rw.topic, "attempts", rw.attempts+1)
		return nil
	}

	delay := failureBackoff(rw.attempts+1, r.opts.FailureBackoff, r.opts.MaxFailureBackoff)
	_, err := r.db.Exec(ctx,
		"update "+Table+" set attempts = attempts + 1, last_error = $2,"+
			" next_attempt_at = now() + $3 * interval '1 microsecond' where id = $1",
		rw.id, publishErr.Error(), delay.Microseconds())
	return err
}

// RowState is the delivery state of one outbox row: how many times Relay
// has attempted it, and whether it is parked or published.
type RowState struct {
	Attempts  int
	Parked    bool
	Published bool
}

// RowState reports the delivery state of the row with the given id. It is
// the operator surface for a poisoned row: the attempt count shows how
// sick the row is, and Parked tells whether Relay has stopped trying.
func (r *Relay) RowState(ctx context.Context, id string) (RowState, error) {
	rows, err := r.db.Query(ctx,
		"select attempts, parked_at is not null, published_at is not null from "+Table+
			" where id = $1",
		id)
	if err != nil {
		return RowState{}, fmt.Errorf("outbox: read row state: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return RowState{}, fmt.Errorf("outbox: no row with id %s", id)
	}
	var state RowState
	if err := rows.Scan(&state.Attempts, &state.Parked, &state.Published); err != nil {
		return RowState{}, fmt.Errorf("outbox: scan row state: %w", err)
	}
	if err := rows.Err(); err != nil {
		return RowState{}, fmt.Errorf("outbox: read row state: %w", err)
	}
	return state, nil
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
