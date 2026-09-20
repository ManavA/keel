package webhooks_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/retry"
	"github.com/ManavA/keel/webhooks"
)

func TestSign_VerifyRoundTrip(t *testing.T) {
	body := []byte(`{"id":"abc","payload":"aGVsbG8="}`)

	sig := webhooks.Sign("secret", body)
	assert.True(t, webhooks.Verify("secret", body, sig), "a signature Sign produced must verify")
	assert.False(t, webhooks.Verify("wrong", body, sig), "the wrong secret must not verify")
	assert.False(t, webhooks.Verify("secret", []byte(`{"id":"abc","payload":"d29ybGQ="}`), sig),
		"a tampered body must not verify")
	assert.False(t, webhooks.Verify("secret", body, "not-a-signature"), "a malformed signature must not verify")
	assert.False(t, webhooks.Verify("secret", body, ""), "an empty signature must not verify")
}

type received struct {
	topic     string
	body      []byte
	signature string
}

type recorder struct {
	mu   sync.Mutex
	got  []received
	fail int64 // remaining 500s before succeeding; negative means fail forever
}

func (r *recorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.got = append(r.got, received{
			topic:     req.Header.Get("X-Keel-Topic"),
			body:      body,
			signature: req.Header.Get("X-Keel-Signature"),
		})
		fail := r.fail
		if r.fail > 0 {
			r.fail--
		}
		r.mu.Unlock()
		if fail != 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (r *recorder) calls() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]received, len(r.got))
	copy(out, r.got)
	return out
}

func testDispatcher(opts webhooks.Options) *webhooks.Dispatcher {
	if opts.Retry.MaxAttempts == 0 {
		opts.Retry = retry.Options{MaxAttempts: 1}
	}
	return webhooks.NewDispatcher(opts)
}

func TestPublish_DeliversSignedPayloadToMatchingEndpoint(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(rec.handler())
	defer server.Close()

	d := testDispatcher(webhooks.Options{})
	_, err := d.Register(webhooks.Endpoint{ID: "orders-hook", URL: server.URL, Secret: "s3cret", Topics: []string{"orders.created"}})
	require.NoError(t, err)

	err = d.Publish(context.Background(), "orders.created", map[string]any{"id": 1})
	require.NoError(t, err)

	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "orders.created", calls[0].topic)
	assert.JSONEq(t, `{"id":1}`, string(calls[0].body))
	assert.True(t, webhooks.Verify("s3cret", calls[0].body, calls[0].signature),
		"the receiver must be able to verify the payload against the shared secret")
}

func TestPublish_OnlyMatchingEndpointsReceive(t *testing.T) {
	orders := &recorder{}
	shipments := &recorder{}
	ordersServer := httptest.NewServer(orders.handler())
	defer ordersServer.Close()
	shipmentsServer := httptest.NewServer(shipments.handler())
	defer shipmentsServer.Close()

	d := testDispatcher(webhooks.Options{})
	_, err := d.Register(webhooks.Endpoint{ID: "orders", URL: ordersServer.URL, Secret: "a", Topics: []string{"orders.created"}})
	require.NoError(t, err)
	_, err = d.Register(webhooks.Endpoint{ID: "shipments", URL: shipmentsServer.URL, Secret: "b", Topics: []string{"shipments.created"}})
	require.NoError(t, err)

	require.NoError(t, d.Publish(context.Background(), "orders.created", []byte(`{}`)))

	assert.Len(t, orders.calls(), 1)
	assert.Empty(t, shipments.calls(), "an endpoint subscribed to another topic must receive nothing")
}

func TestPublish_EndpointWithoutTopicsReceivesAll(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(rec.handler())
	defer server.Close()

	d := testDispatcher(webhooks.Options{})
	_, err := d.Register(webhooks.Endpoint{ID: "all", URL: server.URL, Secret: "a"})
	require.NoError(t, err)

	require.NoError(t, d.Publish(context.Background(), "anything.happened", []byte(`{}`)))
	assert.Len(t, rec.calls(), 1, "an endpoint with no topics subscribes to every topic")
}

func TestPublish_NoMatchingEndpointIsNotAnError(t *testing.T) {
	d := testDispatcher(webhooks.Options{})
	assert.NoError(t, d.Publish(context.Background(), "orders.created", []byte(`{}`)),
		"an event nobody subscribed to is delivered vacuously, so the outbox row still resolves")
}

func TestPublish_FailingEndpointIsRetriedNotDropped(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	d := testDispatcher(webhooks.Options{Retry: retry.Options{MaxAttempts: 3, BaseDelay: time.Microsecond}})
	id, err := d.Register(webhooks.Endpoint{URL: server.URL, Secret: "a", Topics: []string{"orders.created"}})
	require.NoError(t, err)

	err = d.Publish(context.Background(), "orders.created", []byte(`{}`))
	require.Error(t, err, "an endpoint that fails every attempt must surface an error, so the outbox row stays unpublished")
	assert.Equal(t, int64(3), attempts.Load(), "the delivery must be retried, not dropped after the first failure")

	failures, ok := d.Failures(id)
	require.True(t, ok)
	assert.Equal(t, 1, failures, "one failed Publish is one failure, however many HTTP attempts it took")
}

func TestPublish_FailureCountResetsOnSuccess(t *testing.T) {
	rec := &recorder{fail: 1}
	server := httptest.NewServer(rec.handler())
	defer server.Close()

	d := testDispatcher(webhooks.Options{Retry: retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond}})
	id, err := d.Register(webhooks.Endpoint{URL: server.URL, Secret: "a"})
	require.NoError(t, err)

	require.Error(t, d.Publish(context.Background(), "orders.created", []byte(`{}`)))
	failures, ok := d.Failures(id)
	require.True(t, ok)
	assert.Equal(t, 1, failures)

	require.NoError(t, d.Publish(context.Background(), "orders.created", []byte(`{}`)))
	failures, ok = d.Failures(id)
	require.True(t, ok)
	assert.Equal(t, 0, failures, "a success resets the consecutive failure count")
}

func TestPublish_OneFailingEndpointDoesNotBlockOthers(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	goodRec := &recorder{}
	good := httptest.NewServer(goodRec.handler())
	defer good.Close()

	d := testDispatcher(webhooks.Options{Retry: retry.Options{MaxAttempts: 1, BaseDelay: time.Microsecond}})
	_, err := d.Register(webhooks.Endpoint{ID: "bad", URL: bad.URL, Secret: "a"})
	require.NoError(t, err)
	_, err = d.Register(webhooks.Endpoint{ID: "good", URL: good.URL, Secret: "b"})
	require.NoError(t, err)

	err = d.Publish(context.Background(), "orders.created", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad")
	assert.Len(t, goodRec.calls(), 1, "the healthy endpoint must still receive the event")
}

func TestPublish_UnmarshalableEventIsAnError(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(rec.handler())
	defer server.Close()

	d := testDispatcher(webhooks.Options{})
	_, err := d.Register(webhooks.Endpoint{URL: server.URL, Secret: "a"})
	require.NoError(t, err)

	assert.Error(t, d.Publish(context.Background(), "orders.created", func() {}),
		"an event that cannot marshal must fail before any delivery is attempted")
	assert.Empty(t, rec.calls())
}

func TestRegister_Validation(t *testing.T) {
	d := testDispatcher(webhooks.Options{})

	for name, ep := range map[string]webhooks.Endpoint{
		"empty URL":    {URL: "", Secret: "a"},
		"bad URL":      {URL: "://nope", Secret: "a"},
		"no scheme":    {URL: "example.com/hook", Secret: "a"},
		"no host":      {URL: "https://", Secret: "a"},
		"empty secret": {URL: "https://example.com/hook", Secret: ""},
	} {
		_, err := d.Register(ep)
		assert.Error(t, err, name)
	}

	id, err := d.Register(webhooks.Endpoint{ID: "hook", URL: "https://example.com/hook", Secret: "a"})
	require.NoError(t, err)
	assert.Equal(t, "hook", id)

	_, err = d.Register(webhooks.Endpoint{ID: "hook", URL: "https://example.com/other", Secret: "b"})
	assert.Error(t, err, "registering the same id twice must fail")

	generated, err := d.Register(webhooks.Endpoint{URL: "https://example.com/hook", Secret: "a"})
	require.NoError(t, err)
	assert.NotEmpty(t, generated, "an endpoint without an id gets one assigned")

	_, ok := d.Failures("hook")
	assert.True(t, ok)
	_, ok = d.Failures("missing")
	assert.False(t, ok)

	endpoints := d.Endpoints()
	require.Len(t, endpoints, 2)
	for _, ep := range endpoints {
		assert.Empty(t, ep.Secret, "listing endpoints must not leak secrets")
	}

	d.Unregister("hook")
	_, ok = d.Failures("hook")
	assert.False(t, ok, "unregistering removes the endpoint and its failure count")
	assert.Len(t, d.Endpoints(), 1)
}

func TestPublish_ConcurrentPublishersDoNotCorruptState(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(rec.handler())
	defer server.Close()

	d := testDispatcher(webhooks.Options{})
	_, err := d.Register(webhooks.Endpoint{URL: server.URL, Secret: "a"})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Publish(context.Background(), fmt.Sprintf("topic.%d", i), []byte(`{}`))
		}()
	}
	wg.Wait()
	assert.Len(t, rec.calls(), 10)
}
