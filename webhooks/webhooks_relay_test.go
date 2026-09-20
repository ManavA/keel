package webhooks_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/log"
	"github.com/ManavA/keel/outbox"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
	"github.com/ManavA/keel/retry"
	"github.com/ManavA/keel/webhooks"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// openRelayPool applies the outbox migrations over the shared test database
// and truncates the table, because Relay.Tick fetches every unpublished row
// rather than one identified by its own topic.
func openRelayPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Shared(t)

	pool, err := keelpg.Open(context.Background(), keelpg.Options{URL: db.URL, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	entries, err := os.ReadDir("../outbox/pg/migrations")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		migration, err := os.ReadFile("../outbox/pg/migrations/" + entry.Name())
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(migration))
		require.NoError(t, err, "apply migration %s", entry.Name())
	}

	_, err = pool.Exec(context.Background(), "truncate table outbox_events")
	require.NoError(t, err)
	return pool
}

func enqueueRelayEvent(t *testing.T, pool *pgxpool.Pool, topic string, payload []byte) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, keelpg.InTx(ctx, pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.Event{Topic: topic, Payload: payload})
	}))
}

// TestDispatcher_DeliversOutboxEventWithVerifiableSignature is the acceptance
// check for the webhooks package: an endpoint is registered, an event is
// published through the outbox relay path, and the receiving server gets the
// payload with a signature it can verify against the shared secret.
func TestDispatcher_DeliversOutboxEventWithVerifiableSignature(t *testing.T) {
	const secret = "whsec-test"

	var gotBody []byte
	var gotSig, gotTopic string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var err error
		gotBody, err = io.ReadAll(req.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotSig = req.Header.Get("X-Keel-Signature")
		gotTopic = req.Header.Get("X-Keel-Topic")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	dispatcher := webhooks.NewDispatcher(webhooks.Options{})
	_, err := dispatcher.Register(webhooks.Endpoint{
		URL: server.URL, Secret: secret, Topics: []string{"orders.created"},
	})
	require.NoError(t, err)

	pool := openRelayPool(t)
	enqueueRelayEvent(t, pool, "orders.created", []byte(`{"id":7}`))

	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:    dispatcher,
		PublishRetry: retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond},
	})
	require.NoError(t, err)

	published, err := relay.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, published)

	require.NotEmpty(t, gotBody, "the endpoint server must have received the delivery")
	assert.Equal(t, "orders.created", gotTopic)
	assert.True(t, webhooks.Verify(secret, gotBody, gotSig),
		"the receiver must verify the delivery against the shared secret")

	var env struct {
		ID      string `json:"id"`
		Payload []byte `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &env))
	assert.NotEmpty(t, env.ID, "the delivery carries the outbox id so the receiver can dedupe redeliveries")
	assert.JSONEq(t, `{"id":7}`, string(env.Payload))
}

// TestDispatcher_FailingEndpointIsRetriedNotDropped publishes through the
// relay to an endpoint that always answers 500, and asserts the dispatcher
// retried the HTTP delivery within the publish and the outbox row stayed
// unpublished, so the relay will attempt it again rather than lose it.
func TestDispatcher_FailingEndpointIsRetriedNotDropped(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	dispatcher := webhooks.NewDispatcher(webhooks.Options{
		Retry: retry.Options{MaxAttempts: 3, BaseDelay: time.Microsecond},
	})
	id, err := dispatcher.Register(webhooks.Endpoint{URL: server.URL, Secret: "s"})
	require.NoError(t, err)

	pool := openRelayPool(t)
	enqueueRelayEvent(t, pool, "orders.created", []byte(`{"id":8}`))

	relay, err := outbox.NewRelay(pool, outbox.Options{
		Publisher:    dispatcher,
		PublishRetry: retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond},
	})
	require.NoError(t, err)

	published, err := relay.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, published, "the failed delivery must not count as published")

	assert.Equal(t, int64(3), attempts.Load(),
		"the dispatcher must retry the failing endpoint rather than drop it after one attempt")

	var isPublished bool
	require.NoError(t, pool.QueryRow(context.Background(),
		"select published_at is not null from outbox_events").Scan(&isPublished))
	assert.False(t, isPublished, "the outbox row stays unpublished, so the next tick retries it")

	failures, ok := dispatcher.Failures(id)
	require.True(t, ok)
	assert.Equal(t, 1, failures)
}
