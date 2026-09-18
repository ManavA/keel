package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"time"
)

// ReplayedHeader is set on a response that came from the store rather than
// from running the handler.
const ReplayedHeader = "Idempotency-Replayed"

const defaultHeaderName = "Idempotency-Key"

// Options configures [Middleware]. The zero value uses [NewMemoryStore], a
// 24-hour TTL, a 1-minute claim lease, a 30-second bounded wait, and the
// header "Idempotency-Key".
type Options struct {
	Store Store

	// HeaderName is the request header carrying the idempotency key.
	HeaderName string

	// TTL is how long a completed response is replayed before the key can
	// be reused for a new request. It is only the replay window; it has no
	// bearing on how long a handler may run.
	TTL time.Duration

	// ClaimLease is how long a request holds a key while its handler runs,
	// default 1 minute. It must exceed the slowest handler behind this
	// middleware: a request still running when its lease expires loses the
	// key, a retry runs the handler a second time, and the first request's
	// response is not stored. It bounds how long a key stays stuck after a
	// process dies mid-request.
	ClaimLease time.Duration

	// Wait bounds how long a request waits for a same-key request already
	// in progress. See the package doc for why waiting is the default
	// instead of an immediate conflict.
	Wait time.Duration

	// PollInterval is how often Wait rechecks the store.
	PollInterval time.Duration
}

// Middleware makes a handler safe to retry: the first request for a given
// key runs the handler and stores its response; later requests with the
// same key and the same method, path, query and body get that stored
// response back, without
// running the handler again. A request with no key passes through
// unaffected.
func Middleware(opts Options) func(http.Handler) http.Handler {
	store := opts.Store
	if store == nil {
		store = NewMemoryStore()
	}
	headerName := opts.HeaderName
	if headerName == "" {
		headerName = defaultHeaderName
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	lease := opts.ClaimLease
	if lease <= 0 {
		lease = time.Minute
	}
	wait := opts.Wait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 50 * time.Millisecond
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(headerName)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			// The request body, and the response in responseCapture, are
			// buffered whole with no size cap. Put http.MaxBytesReader ahead
			// of this middleware where uploads can be large.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read request body", http.StatusBadRequest)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			hash := hashRequest(r.Method, r.URL.Path, r.URL.RawQuery, body)

			claimID, rec, err := store.Claim(r.Context(), key, hash, lease)
			deadline := time.Now().Add(wait)
			for errors.Is(err, ErrInProgress) && time.Now().Before(deadline) {
				time.Sleep(pollInterval)
				claimID, rec, err = store.Claim(r.Context(), key, hash, lease)
			}

			switch {
			case errors.Is(err, ErrInProgress):
				writeConflict(w, "request with this idempotency key is still in progress")
				return
			case err != nil:
				http.Error(w, "idempotency store: "+err.Error(), http.StatusInternalServerError)
				return
			case rec != nil:
				if rec.RequestHash != hash {
					writeConflict(w, "idempotency key reused with a different request")
					return
				}
				writeStored(w, *rec, true)
				return
			}

			// A client disconnect cancels r.Context(). Complete and Release
			// must still land, or the key stays claimed until its lease ends.
			bookkeepingCtx := context.WithoutCancel(r.Context())

			capture := newResponseCapture()
			completed := false
			defer func() {
				if p := recover(); p != nil {
					if !completed {
						_ = store.Release(bookkeepingCtx, key, claimID)
					}
					panic(p)
				}
			}()
			next.ServeHTTP(capture, r)

			result := Record{
				RequestHash: hash,
				Status:      capture.status(),
				Header:      capture.Header(),
				Body:        capture.body.Bytes(),
				ExpiresAt:   time.Now().Add(ttl),
			}
			cerr := store.Complete(bookkeepingCtx, key, claimID, result)
			if errors.Is(cerr, ErrClaimLost) {
				// The lease ran out and another request owns the key now.
				// This handler did run, so its client gets its response;
				// the key's record is the other request's to write.
				writeStored(w, result, false)
				return
			}
			if cerr != nil {
				_ = store.Release(bookkeepingCtx, key, claimID)
				http.Error(w, "idempotency store: "+cerr.Error(), http.StatusInternalServerError)
				return
			}
			completed = true

			writeStored(w, result, false)
		})
	}
}

// hashRequest covers everything that makes two requests different, so a key
// reused on another route or with another query is a conflict, not a replay.
// The fields are NUL-separated so they cannot run into each other.
func hashRequest(method, path, rawQuery string, body []byte) string {
	h := sha256.New()
	for _, field := range []string{method, path, rawQuery} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func writeStored(w http.ResponseWriter, rec Record, replayed bool) {
	for k, vv := range rec.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if replayed {
		w.Header().Set(ReplayedHeader, "true")
	}
	w.WriteHeader(rec.Status)
	_, _ = w.Write(rec.Body)
}

func writeConflict(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusConflict)
	_, _ = io.WriteString(w, message)
}

// responseCapture buffers a handler's response so it can be stored before
// anything reaches the real client.
//
// It does not implement http.Flusher: the response is written out only once
// the handler returns, so a streaming handler does not stream behind this
// middleware.
type responseCapture struct {
	header      http.Header
	code        int
	wroteHeader bool
	body        bytes.Buffer
}

func newResponseCapture() *responseCapture {
	return &responseCapture{header: http.Header{}}
}

func (c *responseCapture) Header() http.Header { return c.header }

func (c *responseCapture) WriteHeader(code int) {
	if c.wroteHeader {
		return
	}
	c.code = code
	c.wroteHeader = true
}

func (c *responseCapture) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	return c.body.Write(b)
}

func (c *responseCapture) status() int {
	if !c.wroteHeader {
		return http.StatusOK
	}
	return c.code
}
