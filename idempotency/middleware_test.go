package idempotency_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/idempotency"
)

func countingHandler(calls *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(calls, 1)
		w.Header().Set("X-Call", strconv.Itoa(int(n)))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("call " + strconv.Itoa(int(n))))
	}
}

func doRequest(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/widgets", bytes.NewBufferString(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_FirstRequestRunsHandler(t *testing.T) {
	var calls int32
	h := idempotency.Middleware(idempotency.Options{})(countingHandler(&calls))

	rec := doRequest(t, h, "key-1", `{"a":1}`)

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "call 1", rec.Body.String())
	assert.Empty(t, rec.Header().Get(idempotency.ReplayedHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestMiddleware_ReplaysSameKeySameBody(t *testing.T) {
	var calls int32
	h := idempotency.Middleware(idempotency.Options{})(countingHandler(&calls))

	first := doRequest(t, h, "key-1", `{"a":1}`)
	second := doRequest(t, h, "key-1", `{"a":1}`)

	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "the handler must run only once")
	assert.Equal(t, first.Code, second.Code)
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, "true", second.Header().Get(idempotency.ReplayedHeader))
	assert.Empty(t, first.Header().Get(idempotency.ReplayedHeader))
}

func TestMiddleware_ConflictOnSameKeyDifferentBody(t *testing.T) {
	var calls int32
	h := idempotency.Middleware(idempotency.Options{})(countingHandler(&calls))

	doRequest(t, h, "key-1", `{"a":1}`)
	second := doRequest(t, h, "key-1", `{"a":2}`)

	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
	assert.Equal(t, http.StatusConflict, second.Code)
	assert.NotContains(t, second.Body.String(), `"a":1`)
	assert.NotContains(t, second.Body.String(), `"a":2`)
}

func TestMiddleware_NoKeyPassesThrough(t *testing.T) {
	var calls int32
	h := idempotency.Middleware(idempotency.Options{})(countingHandler(&calls))

	first := doRequest(t, h, "", `{}`)
	second := doRequest(t, h, "", `{}`)

	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "no key means no dedup")
	assert.NotEqual(t, first.Body.String(), second.Body.String())
}

// TestMiddleware_ConcurrentSameKeyRunsHandlerOnce races two requests for the
// same key and body against each other. Run with -race: the two Claim calls
// must serialize on MemoryStore's mutex so exactly one of them starts the
// handler, and the other waits for and replays its result.
func TestMiddleware_ConcurrentSameKeyRunsHandlerOnce(t *testing.T) {
	var calls int32
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		countingHandler(&calls).ServeHTTP(w, r)
	}
	h := idempotency.Middleware(idempotency.Options{
		Wait:         2 * time.Second,
		PollInterval: 5 * time.Millisecond,
	})(http.HandlerFunc(slow))

	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = doRequest(t, h, "race-key", `{"a":1}`)
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "exactly one request must have run the handler")
	assert.Equal(t, results[0].Body.String(), results[1].Body.String())

	replayed := 0
	for _, r := range results {
		if r.Header().Get(idempotency.ReplayedHeader) == "true" {
			replayed++
		}
	}
	assert.Equal(t, 1, replayed, "exactly one of the two responses must be the replay")
}

func TestMiddleware_WaitTimeoutReturnsConflict(t *testing.T) {
	release := make(chan struct{})
	blocking := func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusCreated)
	}
	h := idempotency.Middleware(idempotency.Options{
		Wait:         20 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
	})(http.HandlerFunc(blocking))

	go doRequest(t, h, "stuck-key", `{}`)
	time.Sleep(5 * time.Millisecond) // let the first request claim the key

	second := doRequest(t, h, "stuck-key", `{}`)
	assert.Equal(t, http.StatusConflict, second.Code)

	close(release)
}

func TestMiddleware_ExpiryReRunsHandler(t *testing.T) {
	var calls int32
	h := idempotency.Middleware(idempotency.Options{TTL: 10 * time.Millisecond})(countingHandler(&calls))

	first := doRequest(t, h, "key-1", `{"a":1}`)
	time.Sleep(20 * time.Millisecond)
	second := doRequest(t, h, "key-1", `{"a":1}`)

	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "an expired key must run the handler again, not replay")
	assert.NotEqual(t, first.Body.String(), second.Body.String())
	assert.Empty(t, second.Header().Get(idempotency.ReplayedHeader))
}

// A client disconnect cancels the request context while the handler is still
// running. The response must still be stored. The store here fails any call
// made with a canceled context, as a database does.
func TestMiddleware_ClientDisconnectDuringHandlerDoesNotWedgeKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := ctxCheckingStore{idempotency.NewMemoryStore()}
	h := idempotency.Middleware(idempotency.Options{
		Store: store,
		Wait:  100 * time.Millisecond, // a stuck key fails this test quickly
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel() // the client is gone; r.Context() is now Done
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("finished anyway"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/widgets", bytes.NewBufferString(`{"a":1}`)).WithContext(ctx)
	req.Header.Set("Idempotency-Key", "disconnect-key")
	first := httptest.NewRecorder()
	h.ServeHTTP(first, req)

	require.Equal(t, http.StatusCreated, first.Code)

	second := doRequest(t, h, "disconnect-key", `{"a":1}`)
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, "true", second.Header().Get(idempotency.ReplayedHeader))
}

// The same key and body sent to another route is a different request: it must
// get the 409 a reused key gets, never the first route's stored response.
func TestMiddleware_SameKeySameBodyDifferentRouteIsNotReplayed(t *testing.T) {
	var chargeCalls, refundCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/charge", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&chargeCalls, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("charged"))
	})
	mux.HandleFunc("/refund", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refundCalls, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("refunded"))
	})
	h := idempotency.Middleware(idempotency.Options{})(mux)

	postWithKey := func(path, key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	charge := postWithKey("/charge", "shared-key", `{"amount":100}`)
	refund := postWithKey("/refund", "shared-key", `{"amount":100}`)

	assert.Equal(t, http.StatusCreated, charge.Code)
	assert.Equal(t, "charged", charge.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&chargeCalls))

	assert.NotEqual(t, "true", refund.Header().Get(idempotency.ReplayedHeader),
		"the refund must not be served from the charge's stored response")
	assert.NotEqual(t, "charged", refund.Body.String(),
		"the client must never be told the refund succeeded by replaying the charge's response")
	assert.Equal(t, http.StatusConflict, refund.Code,
		"reusing a key across routes is a key/request mismatch, the same as reusing it with a different body")
	assert.Equal(t, int32(0), atomic.LoadInt32(&refundCalls),
		"a rejected mismatch must not run the refund handler either — the key means something else already")
}

// Two empty-body POSTs to different routes differ only by path.
func TestMiddleware_SameKeyEmptyBodyDifferentRouteIsNotReplayed(t *testing.T) {
	var chargeCalls, refundCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/charge", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&chargeCalls, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("charged"))
	})
	mux.HandleFunc("/refund", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refundCalls, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("refunded"))
	})
	h := idempotency.Middleware(idempotency.Options{})(mux)

	postWithKey := func(path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	postWithKey("/charge", "shared-key")
	refund := postWithKey("/refund", "shared-key")

	assert.NotEqual(t, "true", refund.Header().Get(idempotency.ReplayedHeader))
	assert.NotEqual(t, "charged", refund.Body.String())
	assert.Equal(t, http.StatusConflict, refund.Code)
	assert.Equal(t, int32(0), atomic.LoadInt32(&refundCalls))
}

func TestMiddleware_CustomStoreIsUsed(t *testing.T) {
	store := idempotency.NewMemoryStore()
	var calls int32
	h := idempotency.Middleware(idempotency.Options{Store: store})(countingHandler(&calls))

	doRequest(t, h, "key-1", `{}`)

	_, rec, err := store.Claim(context.Background(), "key-1", "irrelevant", time.Hour)
	require.NoError(t, err)
	assert.NotNil(t, rec, "the response should already be recorded in the store passed via Options")
}

// Middleware reads the body to hash it and must hand the handler a fresh
// reader over the same bytes.
func TestMiddleware_WrappedHandlerCanReadRequestBody(t *testing.T) {
	var gotBody string
	h := idempotency.Middleware(idempotency.Options{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}))

	rec := doRequest(t, h, "key-1", `{"a":1,"b":"two"}`)

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, `{"a":1,"b":"two"}`, gotBody,
		"the handler must be able to read the same body Middleware hashed, not an already-drained reader")
}

// ctxCheckingStore fails Complete and Release on a canceled context, the way
// a database-backed store does. MemoryStore alone ignores its context.
type ctxCheckingStore struct{ *idempotency.MemoryStore }

func (s ctxCheckingStore) Complete(ctx context.Context, key, claimID string, rec idempotency.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.MemoryStore.Complete(ctx, key, claimID, rec)
}

func (s ctxCheckingStore) Release(ctx context.Context, key, claimID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.MemoryStore.Release(ctx, key, claimID)
}

// A different query string is a different request.
func TestMiddleware_SameKeySameBodyDifferentQueryIsNotReplayed(t *testing.T) {
	var calls int32
	h := idempotency.Middleware(idempotency.Options{})(countingHandler(&calls))

	post := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, bytes.NewBufferString(`{}`))
		req.Header.Set("Idempotency-Key", "shared-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	first := post("/widgets?account=1")
	second := post("/widgets?account=2")

	assert.Equal(t, http.StatusCreated, first.Code)
	assert.Equal(t, http.StatusConflict, second.Code)
	assert.Empty(t, second.Header().Get(idempotency.ReplayedHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))

	assert.Equal(t, "true", post("/widgets?account=1").Header().Get(idempotency.ReplayedHeader),
		"the same query must still replay")
}

func TestMiddleware_ReplayCarriesTheStoredHeaders(t *testing.T) {
	var calls int32
	h := idempotency.Middleware(idempotency.Options{})(countingHandler(&calls))

	first := doRequest(t, h, "key-1", `{}`)
	second := doRequest(t, h, "key-1", `{}`)

	require.Equal(t, "1", first.Header().Get("X-Call"))
	assert.Equal(t, "1", second.Header().Get("X-Call"), "a replay must carry the first response's headers")
}

// A TTL shorter than the handler must not let a retry take the key: the
// claim is held for ClaimLease, and TTL only starts once the response is
// stored.
func TestMiddleware_ShortTTLDoesNotReleaseARunningClaim(t *testing.T) {
	var calls int32
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		countingHandler(&calls).ServeHTTP(w, r)
	})
	h := idempotency.Middleware(idempotency.Options{
		TTL:          30 * time.Millisecond,
		Wait:         2 * time.Second,
		PollInterval: 5 * time.Millisecond,
	})(slow)

	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 60 * time.Millisecond)
			results[i] = doRequest(t, h, "key-1", `{}`)
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "the handler must run once")
	assert.Equal(t, results[0].Body.String(), results[1].Body.String())
}

// When a handler does outlive its lease, a retry takes the key and runs the
// handler again. Each client must get its own handler's response, and the
// stored record must come whole from the request that holds the key.
func TestMiddleware_HandlerOutlivingItsLeaseDoesNotCorruptTheRecord(t *testing.T) {
	store := idempotency.NewMemoryStore()
	var calls int32
	slowFirst := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			time.Sleep(150 * time.Millisecond)
		}
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(append([]byte("echo "), body...))
	})
	h := idempotency.Middleware(idempotency.Options{
		Store:        store,
		ClaimLease:   30 * time.Millisecond,
		Wait:         time.Second,
		PollInterval: 5 * time.Millisecond,
	})(slowFirst)

	var wg sync.WaitGroup
	var slowResp *httptest.ResponseRecorder
	wg.Add(1)
	go func() {
		defer wg.Done()
		slowResp = doRequest(t, h, "key-1", `A`)
	}()

	time.Sleep(60 * time.Millisecond) // past A's lease, inside A's handler
	fastResp := doRequest(t, h, "key-1", `B`)
	wg.Wait()

	assert.Equal(t, "echo B", fastResp.Body.String())
	assert.Equal(t, "echo A", slowResp.Body.String(), "the late request still answers its own client")

	replay := doRequest(t, h, "key-1", `B`)
	assert.Equal(t, "true", replay.Header().Get(idempotency.ReplayedHeader))
	assert.Equal(t, "echo B", replay.Body.String(), "the record must be B's response under B's hash")

	assert.Equal(t, http.StatusConflict, doRequest(t, h, "key-1", `A`).Code)
}
