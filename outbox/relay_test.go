package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/outbox"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/retry"
)

type fakeCall struct {
	topic string
	env   outbox.Envelope
}

// fakePublisher records every call and can be told to fail forever for a
// specific envelope id, to simulate a row nothing will ever be able to
// deliver.
type fakePublisher struct {
	mu   sync.Mutex
	seen []fakeCall
	fail map[string]bool
	once map[string]bool
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{fail: map[string]bool{}, once: map[string]bool{}}
}

func (f *fakePublisher) failAlways(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[id] = true
}

func (f *fakePublisher) failOnce(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.once[id] = true
}

func (f *fakePublisher) Publish(_ context.Context, topic string, event any) error {
	env, ok := event.(outbox.Envelope)
	if !ok {
		return fmt.Errorf("fakePublisher: unexpected event type %T", event)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, fakeCall{topic: topic, env: env})
	if f.once[env.ID] {
		delete(f.once, env.ID)
		return fmt.Errorf("fakePublisher: %s failed transiently", env.ID)
	}
	if f.fail[env.ID] {
		return fmt.Errorf("fakePublisher: %s is poisoned", env.ID)
	}
	return nil
}

func (f *fakePublisher) calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.seen))
	copy(out, f.seen)
	return out
}

func TestNewRelay_RequiresPublisher(t *testing.T) {
	_, err := outbox.NewRelay(nil, outbox.Options{})
	assert.Error(t, err)
}

func TestRelay_PublishesUnpublishedRows(t *testing.T) {
	pool := openEmptyPool(t)
	id := enqueueOne(t, pool, "widgets.created", []byte(`{"a":1}`))

	publisher := newFakePublisher()
	relay, err := outbox.NewRelay(pool, outbox.Options{Publisher: publisher})
	require.NoError(t, err)

	published, err := relay.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, published)

	calls := publisher.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "widgets.created", calls[0].topic)
	assert.Equal(t, id, calls[0].env.ID)
	assert.Equal(t, []byte(`{"a":1}`), calls[0].env.Payload)
	assertPublished(t, pool, id)

	published, err = relay.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, published, "an already-published row must not be republished")
	assert.Len(t, publisher.calls(), 1)
}

// TestRelay_PoisonedRowDoesNotBlockOthers enqueues a row that will never
// publish successfully alongside two that will, and asserts the two good
// rows still go out in the same tick.
func TestRelay_PoisonedRowDoesNotBlockOthers(t *testing.T) {
	pool := openEmptyPool(t)
	poisoned := enqueueOne(t, pool, "poison", []byte("bad"))
	good1 := enqueueOne(t, pool, "good.1", []byte("ok1"))
	good2 := enqueueOne(t, pool, "good.2", []byte("ok2"))

	publisher := newFakePublisher()
	publisher.failAlways(poisoned)

	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:    publisher,
		PublishRetry: retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond},
	})
	require.NoError(t, err)

	published, err := relay.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, published, "the two healthy rows must publish despite the poisoned one")

	assertUnpublished(t, pool, poisoned)
	assertPublished(t, pool, good1)
	assertPublished(t, pool, good2)
}

// TestRelay_PoisonedRowsExceedingBatchSizeDoNotStarveAHealthyRow puts more
// permanently failing rows than a batch holds ahead of one healthy row. Each
// poisoned row backs off after its failure, so the batches behind it reach
// the healthy row.
func TestRelay_PoisonedRowsExceedingBatchSizeDoNotStarveAHealthyRow(t *testing.T) {
	pool := openEmptyPool(t)

	const batchSize = 5
	const poisonedCount = 12 // several multiples of batchSize

	publisher := newFakePublisher()
	poisonedIDs := make([]string, poisonedCount)
	for i := range poisonedIDs {
		id := enqueueOne(t, pool, "poison", []byte("bad"))
		publisher.failAlways(id)
		poisonedIDs[i] = id
	}
	healthy := enqueueOne(t, pool, "widgets.created", []byte("ok"))

	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:    publisher,
		BatchSize:    batchSize,
		PublishRetry: retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond},
		// Longer than the test, so no poisoned row comes due again.
		FailureBackoff: time.Hour,
	})
	require.NoError(t, err)

	// 12 poisoned rows at 5 per batch: the third batch holds the healthy row.
	const maxTicks = 3
	published := false
	for i := 0; i < maxTicks && !published; i++ {
		_, err := relay.Tick(context.Background())
		require.NoError(t, err)

		var isPublished bool
		require.NoError(t, pool.QueryRow(context.Background(),
			"select published_at is not null from outbox_events where id = $1", healthy,
		).Scan(&isPublished))
		published = isPublished
	}

	assert.True(t, published, "the healthy row must be published within %d ticks", maxTicks)

	for _, id := range poisonedIDs {
		assertUnpublished(t, pool, id)
	}
}

// TestRelay_CrashBetweenPublishAndMarkRepublishes simulates a Relay that
// crashes after publishing a row but before recording it as published, by
// making one connection's mark-published update fail once. A second Relay
// standing in for the restarted process must see the row as still
// unpublished and deliver it again, carrying the same id both times.
func TestRelay_CrashBetweenPublishAndMarkRepublishes(t *testing.T) {
	pool := openEmptyPool(t)
	enqueueOne(t, pool, "orders.created", []byte("hello"))

	publisher := newFakePublisher()
	crashing := &crashingConn{pgxConn: pool, failNextMark: true}

	relay, err := outbox.NewRelay(crashing, outbox.Options{Publisher: publisher})
	require.NoError(t, err)

	published, err := relay.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, published, "the mark-published update failed, so this tick must not count it as published")

	calls := publisher.calls()
	require.Len(t, calls, 1, "the publish itself happened before the simulated crash")

	restarted, err := outbox.NewRelay(pool, outbox.Options{Publisher: publisher})
	require.NoError(t, err)

	published, err = restarted.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, published)

	calls = publisher.calls()
	require.Len(t, calls, 2, "at-least-once: the row publishes again after the crash")
	assert.Equal(t, calls[0].env.ID, calls[1].env.ID,
		"both deliveries carry the same id, which a consumer uses to dedupe")
}

// crashingConn wraps a real pool and fails the first update that sets
// published_at, to model a crash between a successful publish and the
// write that records it.
type crashingConn struct {
	pgxConn *pgxpool.Pool

	mu           sync.Mutex
	failNextMark bool
}

func (c *crashingConn) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.mu.Lock()
	fail := c.failNextMark && strings.Contains(sql, "published_at = now()")
	if fail {
		c.failNextMark = false
	}
	c.mu.Unlock()

	if fail {
		return pgconn.CommandTag{}, errors.New("simulated crash before mark-published committed")
	}
	return c.pgxConn.Exec(ctx, sql, args...)
}

func (c *crashingConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return c.pgxConn.Query(ctx, sql, args...)
}

// TestRelay_Run_PublishesOnEachTick covers Relay.Run's normal operation:
// left running, it publishes a row without anyone calling Tick directly,
// and stops when its context is done.
func TestRelay_Run_PublishesOnEachTick(t *testing.T) {
	pool := openEmptyPool(t)
	id := enqueueOne(t, pool, "widgets.created", []byte("hello"))

	publisher := newFakePublisher()
	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:    publisher,
		PollInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	assert.ErrorIs(t, runWithin(ctx, t, relay, 5*time.Second), context.DeadlineExceeded,
		"Run must return ctx.Err() once its context is done")
	assertPublished(t, pool, id)
}

// cancelingConn wraps a real pool and, the first time it sees the
// mark-published update succeed, calls cancel — simulating a shutdown
// signal landing at the instant Tick finishes committing one row's
// outcome and is about to move on to the next row in the same batch.
type cancelingConn struct {
	pgxConn *pgxpool.Pool
	cancel  context.CancelFunc

	mu        sync.Mutex
	triggered bool
}

func (c *cancelingConn) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := c.pgxConn.Exec(ctx, sql, args...)

	if err == nil && strings.Contains(sql, "published_at = now()") {
		c.mu.Lock()
		if !c.triggered {
			c.triggered = true
			c.cancel()
		}
		c.mu.Unlock()
	}

	return tag, err
}

func (c *cancelingConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return c.pgxConn.Query(ctx, sql, args...)
}

// TestRelay_Run_ShutdownMidTickLeavesNoTornState formalizes the "shutdown
// mid-tick" scenario: a batch has several rows, and the context is
// canceled partway through the batch's for loop, not between ticks. Every
// row Tick reaches after that must still end up in a consistent state —
// either actually published and marked so, or left cleanly unpublished —
// never marked published without the publish call having happened, or
// vice versa. Whatever the cancellation leaves unpublished, a later Tick
// (standing in for a restarted process) must be able to finish cleanly.
func TestRelay_Run_ShutdownMidTickLeavesNoTornState(t *testing.T) {
	pool := openEmptyPool(t)
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = enqueueOne(t, pool, fmt.Sprintf("topic.%d", i), []byte("x"))
	}

	publisher := newFakePublisher()
	ctx, cancel := context.WithCancel(context.Background())
	cc := &cancelingConn{pgxConn: pool, cancel: cancel}

	relay, err := outbox.NewRelay(cc, outbox.Options{
		Publisher:    publisher,
		PollInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)

	assert.ErrorIs(t, runWithin(ctx, t, relay, 5*time.Second), context.Canceled)

	publishedAtBroker := map[string]bool{}
	for _, call := range publisher.calls() {
		publishedAtBroker[call.env.ID] = true
	}
	for _, id := range ids {
		var markedPublished bool
		require.NoError(t, pool.QueryRow(context.Background(),
			"select published_at is not null from outbox_events where id = $1", id,
		).Scan(&markedPublished))
		if markedPublished {
			assert.True(t, publishedAtBroker[id],
				"row %s is marked published but was never actually published — a torn write", id)
		}
	}

	// Stand in for the process restarting: a fresh Relay over the same,
	// now-uncanceled pool must finish off whatever the shutdown left
	// unpublished, without any corruption from the partial tick above.
	restarted, err := outbox.NewRelay(pool, outbox.Options{Publisher: publisher})
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		if _, tickErr := restarted.Tick(context.Background()); tickErr != nil {
			require.NoError(t, tickErr)
		}
	}
	for _, id := range ids {
		assertPublished(t, pool, id)
	}
}

// runWithin runs relay.Run and fails the test if it has not returned within
// limit, so a Run that never stops fails here instead of at the package
// timeout.
func runWithin(ctx context.Context, t *testing.T, relay *outbox.Relay, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("Relay.Run did not return within %s", limit)
		return nil
	}
}

// TestRelay_TransientFailureIsNotStarvedBySustainedArrivals fails one row
// once, then enqueues a full batch of new rows before every tick. Once its
// backoff has passed the row is the oldest due row, so it must go out in
// the next tick rather than wait behind the arrivals.
func TestRelay_TransientFailureIsNotStarvedBySustainedArrivals(t *testing.T) {
	pool := openEmptyPool(t)

	const batchSize = 5

	publisher := newFakePublisher()
	flaky := enqueueOne(t, pool, "flaky", []byte("x"))
	publisher.failOnce(flaky)

	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:      publisher,
		BatchSize:      batchSize,
		PublishRetry:   retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond},
		FailureBackoff: time.Hour,
	})
	require.NoError(t, err)

	arrive := func() {
		for range batchSize {
			enqueueOne(t, pool, "arrival", []byte("y"))
		}
	}

	arrive()
	_, err = relay.Tick(context.Background())
	require.NoError(t, err)
	assertUnpublished(t, pool, flaky)

	// Inside the backoff the row is hidden, so a tick must not retry it.
	arrive()
	_, err = relay.Tick(context.Background())
	require.NoError(t, err)
	assertUnpublished(t, pool, flaky)

	// Stand in for the backoff passing, without waiting an hour.
	_, err = pool.Exec(context.Background(),
		"update outbox_events set next_attempt_at = now() where id = $1", flaky)
	require.NoError(t, err)

	arrive()
	_, err = relay.Tick(context.Background())
	require.NoError(t, err)
	assertPublished(t, pool, flaky)
}

// TestRelay_TickTakesTheOldestBatchSizeRows pins the fetch's limit and order.
func TestRelay_TickTakesTheOldestBatchSizeRows(t *testing.T) {
	pool := openEmptyPool(t)

	ids := make([]string, 7)
	for i := range ids {
		ids[i] = enqueueOne(t, pool, fmt.Sprintf("order.%d", i), []byte("x"))
	}

	publisher := newFakePublisher()
	relay, err := outbox.NewRelay(pool, outbox.Options{Publisher: publisher, BatchSize: 3})
	require.NoError(t, err)

	published, err := relay.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, published)

	calls := publisher.calls()
	require.Len(t, calls, 3)
	for i, call := range calls {
		assert.Equal(t, ids[i], call.env.ID, "rows must publish oldest first")
	}
	for _, id := range ids[3:] {
		assertUnpublished(t, pool, id)
	}
}

// TestRelay_FailureBackoffGrowsAndIsCapped reads next_attempt_at after each
// failed tick of a poisoned row.
func TestRelay_FailureBackoffGrowsAndIsCapped(t *testing.T) {
	pool := openEmptyPool(t)
	ctx := context.Background()

	publisher := newFakePublisher()
	id := enqueueOne(t, pool, "poison", []byte("bad"))
	publisher.failAlways(id)

	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:         publisher,
		PublishRetry:      retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond},
		FailureBackoff:    time.Hour,
		MaxFailureBackoff: 3 * time.Hour,
	})
	require.NoError(t, err)

	for _, want := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour, 3 * time.Hour} {
		_, err := relay.Tick(ctx)
		require.NoError(t, err)

		var wait time.Duration
		require.NoError(t, pool.QueryRow(ctx,
			"select next_attempt_at - now() from outbox_events where id = $1", id).Scan(&wait))
		assert.InDelta(t, want.Seconds(), wait.Seconds(), 60)

		// Make the row due again without waiting an hour.
		_, err = pool.Exec(ctx, "update outbox_events set next_attempt_at = now() where id = $1", id)
		require.NoError(t, err)
	}
}

// TestRelay_PermanentlyFailingRowParksAndOthersFlow is issue #16: a row that
// can never publish must park after a configured max attempts instead of
// being retried forever, and rows behind it must keep flowing. A parked row
// is never attempted again.
func TestRelay_PermanentlyFailingRowParksAndOthersFlow(t *testing.T) {
	pool := openEmptyPool(t)
	ctx := context.Background()

	poisoned := enqueueOne(t, pool, "poison", []byte("bad"))
	healthy := enqueueOne(t, pool, "good", []byte("ok"))

	publisher := newFakePublisher()
	publisher.failAlways(poisoned)

	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:    publisher,
		PublishRetry: retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond},
		MaxAttempts:  3,
	})
	require.NoError(t, err)

	// The healthy row flows on the first tick despite the poisoned one.
	published, err := relay.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, published)
	assertPublished(t, pool, healthy)
	assertUnpublished(t, pool, poisoned)

	// Tick until the poisoned row parks. The loop is bounded, so a
	// regression that never parks fails here instead of hanging.
	parked := false
	for i := 0; i < 10 && !parked; i++ {
		// Stand in for the backoff passing, without waiting for it.
		_, err = pool.Exec(ctx,
			"update outbox_events set next_attempt_at = now() where id = $1", poisoned)
		require.NoError(t, err)

		_, err = relay.Tick(ctx)
		require.NoError(t, err)

		state, err := relay.RowState(ctx, poisoned)
		require.NoError(t, err)
		parked = state.Parked
	}
	require.True(t, parked, "a permanently failing row must park after MaxAttempts")

	state, err := relay.RowState(ctx, poisoned)
	require.NoError(t, err)
	assert.Equal(t, 3, state.Attempts)
	assert.False(t, state.Published)

	callsFor := func(id string) int {
		n := 0
		for _, c := range publisher.calls() {
			if c.env.ID == id {
				n++
			}
		}
		return n
	}

	// A parked row is never attempted again.
	before := callsFor(poisoned)
	_, err = relay.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, callsFor(poisoned), "a parked row must not be published again")

	// A row enqueued behind the parked row still flows.
	late := enqueueOne(t, pool, "late", []byte("ok"))
	published, err = relay.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, published)
	assertPublished(t, pool, late)
}

// enqueueOneWithPartition is enqueueOne for a row carrying a partition key.
// Rows sharing a key belong to one aggregate whose delivery order matters;
// see Options.OrderedPartitions.
func enqueueOneWithPartition(t *testing.T, pool *pgxpool.Pool, topic string, payload []byte, partition string) string {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, keelpg.InTx(ctx, pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.Event{Topic: topic, Payload: payload, PartitionKey: partition})
	}))

	var id string
	require.NoError(t, pool.QueryRow(ctx,
		"select id from outbox_events where topic = $1 order by created_at desc limit 1", topic,
	).Scan(&id))
	return id
}

// TestRelay_OrderedPartitionsHoldLaterRowUntilHeadSucceeds is issue #36: in
// ordered mode two rows for one aggregate publish in creation order. The
// head row fails once, and the later row must not publish until the head
// has actually succeeded — not on the tick the head fails, and not on the
// tick the head succeeds either, because the later row was already held
// out of that tick's batch.
func TestRelay_OrderedPartitionsHoldLaterRowUntilHeadSucceeds(t *testing.T) {
	pool := openEmptyPool(t)
	ctx := context.Background()

	head := enqueueOneWithPartition(t, pool, "orders.events", []byte(`{"seq":1}`), "order-1")
	tail := enqueueOneWithPartition(t, pool, "orders.events", []byte(`{"seq":2}`), "order-1")

	publisher := newFakePublisher()
	publisher.failOnce(head)

	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:         publisher,
		PublishRetry:      retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond},
		FailureBackoff:    time.Hour,
		OrderedPartitions: true,
	})
	require.NoError(t, err)

	// The head fails; the tail must not be attempted while the head is out.
	published, err := relay.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, published)
	assertUnpublished(t, pool, head)
	assertUnpublished(t, pool, tail)
	for _, call := range publisher.calls() {
		assert.Equal(t, head, call.env.ID, "only the head row may be attempted while it is unpublished")
	}

	// Stand in for the backoff passing, without waiting an hour.
	_, err = pool.Exec(ctx, "update outbox_events set next_attempt_at = now() where id = $1", head)
	require.NoError(t, err)

	// The head succeeds, but the tail was already held out of this tick's
	// batch, so it stays unpublished until the next tick.
	published, err = relay.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, published)
	assertPublished(t, pool, head)
	assertUnpublished(t, pool, tail)

	published, err = relay.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, published)
	assertPublished(t, pool, tail)

	calls := publisher.calls()
	require.Len(t, calls, 3)
	assert.Equal(t, head, calls[0].env.ID)
	assert.Equal(t, head, calls[1].env.ID)
	assert.Equal(t, tail, calls[2].env.ID, "the later row publishes only after the head succeeds")
}

// TestRelay_UnorderedByDefaultPublishesAroundBlockedHead pins the default:
// without OrderedPartitions, rows sharing a partition key still publish
// independently, so a blocked head does not hold the rows behind it.
func TestRelay_UnorderedByDefaultPublishesAroundBlockedHead(t *testing.T) {
	pool := openEmptyPool(t)
	ctx := context.Background()

	head := enqueueOneWithPartition(t, pool, "orders.events", []byte(`{"seq":1}`), "order-1")
	tail := enqueueOneWithPartition(t, pool, "orders.events", []byte(`{"seq":2}`), "order-1")

	publisher := newFakePublisher()
	publisher.failAlways(head)

	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:      publisher,
		PublishRetry:   retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond},
		FailureBackoff: time.Hour,
	})
	require.NoError(t, err)

	published, err := relay.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, published)
	assertUnpublished(t, pool, head)
	assertPublished(t, pool, tail)
}
