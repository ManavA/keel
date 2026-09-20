package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/app"
	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
	"github.com/ManavA/keel/webhooks"
)

// An example nobody runs stops working quietly. These tests exercise the
// receiver the way its README says to, against a real database, so a
// signature drifting in one of the packages it wires shows up as a failure
// rather than a stale file.
// The webhook profile carries no auth package: the receiver answers
// signatures, not sessions, so auth and auth/pg stay out of the imports.

func TestMain(m *testing.M) {
	slog.SetDefault(keellog.New(keellog.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// testReceiver is the example the way run() builds it — the receiver over a
// pool on its own schema — wrapped in an httptest server.
type testReceiver struct {
	srv *httptest.Server
	rc  *Receiver
}

func newTestReceiver(t *testing.T) *testReceiver {
	t.Helper()

	db := testdb.Shared(t)
	ctx := context.Background()
	schema := "webhook_example_" + randomSuffix(t)

	admin, err := pg.Open(ctx, pg.Options{URL: db.URL})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	admin.Close()

	a := app.New(app.Options{
		Logger:      slog.Default(),
		DatabaseURL: db.URL + "&search_path=" + schema,
		Migrations: []app.MigrationSource{
			{FS: migrationFiles, Dir: "migrations"},
		},
	})
	require.NoError(t, a.Open(ctx))
	t.Cleanup(a.Close)

	rc := NewReceiver(a.Pool(), "test-secret")
	r := chi.NewRouter()
	rc.Routes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &testReceiver{srv: srv, rc: rc}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return hex.EncodeToString(b[:])
}

const testSecret = "test-secret"

// postDelivery sends a delivery the way a sender would: the topic header, the
// signature over the exact bytes, and the body.
func postDelivery(t *testing.T, ts *testReceiver, topic, secret, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.srv.URL+"/hooks/events", bytes.NewBufferString(body))
	require.NoError(t, err)
	if topic != "" {
		req.Header.Set(webhooks.TopicHeader, topic)
	}
	if secret != "" {
		req.Header.Set(webhooks.SignatureHeader, webhooks.Sign(secret, []byte(body)))
	}
	resp, err := ts.srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

func deliveryCount(t *testing.T, ts *testReceiver) int {
	t.Helper()
	var count int
	require.NoError(t, ts.rc.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM webhook_deliveries`).Scan(&count))
	return count
}

// TestSignedDeliveryIsStored proves the happy path: a signed delivery for a
// topic is stored and answered 202 with the stored row.
func TestSignedDeliveryIsStored(t *testing.T) {
	ts := newTestReceiver(t)

	resp, raw := postDelivery(t, ts, "note.created", testSecret, `{"id":"abc"}`)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(raw))

	var delivery Delivery
	require.NoError(t, json.Unmarshal(raw, &delivery))
	assert.NotEmpty(t, delivery.ID)
	assert.Equal(t, "note.created", delivery.Topic)
	assert.False(t, delivery.ReceivedAt.IsZero())
	assert.Equal(t, 1, deliveryCount(t, ts))
}

// TestUnsignedDeliveryIsRefused proves the profile's point: without a valid
// signature nothing is stored, whatever the body says.
func TestUnsignedDeliveryIsRefused(t *testing.T) {
	ts := newTestReceiver(t)

	resp, _ := postDelivery(t, ts, "note.created", "", `{"id":"abc"}`)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, _ = postDelivery(t, ts, "note.created", "wrong-secret", `{"id":"abc"}`)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	assert.Equal(t, 0, deliveryCount(t, ts))
}

// TestDeliveryWithoutATopicIsRefused proves a signed body for no topic cannot
// be stored under an empty one.
func TestDeliveryWithoutATopicIsRefused(t *testing.T) {
	ts := newTestReceiver(t)

	resp, raw := postDelivery(t, ts, "", testSecret, `{"id":"abc"}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	assert.Equal(t, 0, deliveryCount(t, ts))
}

// TestNonJSONDeliveryIsRefused proves the stored payload is always JSON: the
// column is jsonb, so a body that is not JSON is refused rather than stored.
func TestNonJSONDeliveryIsRefused(t *testing.T) {
	ts := newTestReceiver(t)

	resp, raw := postDelivery(t, ts, "note.created", testSecret, `not json`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	assert.Equal(t, 0, deliveryCount(t, ts))
}

// TestConfigRefusesAnEmptySecret proves a receiver that would store anything
// does not start: without a secret every signature check is meaningless.
func TestConfigRefusesAnEmptySecret(t *testing.T) {
	var cfg Config
	cfg.Port = 8080
	cfg.DatabaseURL = "postgres://example/db"
	cfg.WebhookSecret = "  "
	assert.ErrorContains(t, cfg.Validate(), "WEBHOOK_SECRET")
}
