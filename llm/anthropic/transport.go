package anthropic

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The request headers that carry the key and the caller's own choices. None
// of them goes to an origin other than the configured one.
const (
	headerKey     = "x-api-key"
	headerVersion = "anthropic-version"
	headerBeta    = "anthropic-beta"
)

const (
	// maxRedirects is where a caller's client with no redirect policy of
	// its own stops, as net/http's default policy does.
	maxRedirects = 10

	// drainBytes and drainWait bound what is read after a stream's last
	// event to let its connection be used again.
	drainBytes = 64 << 10
	drainWait  = 100 * time.Millisecond

	eventStreamType = "text/event-stream"
)

// noRedirect is the redirect policy of the clients this package builds: a
// 3xx is the response, and nothing is sent to wherever it points. net/http
// would otherwise repeat a 307 or 308 with its body and with x-api-key,
// which is not among the headers it withholds from another host.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// guarded returns a copy of a caller's client that keeps every policy of the
// original, its redirect policy included, and takes this package's headers
// off any request a redirect sends to another origin. The origin a hop is
// measured against is the configured one and not the hop before: net/http
// copies the first request's headers onto every hop, so a second hop on a
// host the first one left for would otherwise be handed the key. The
// original client is not changed.
func (c *Client) guarded(hc *http.Client) *http.Client {
	policy := hc.CheckRedirect
	home := origin(c.base)
	guarded := *hc
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if origin(req.URL) != home {
			req.Header.Del(headerKey)
			req.Header.Del(headerVersion)
			req.Header.Del(headerBeta)
		}
		var err error
		switch {
		case policy != nil:
			err = policy(req, via)
		case len(via) >= maxRedirects:
			err = fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		// net/http knows ErrUseLastResponse by identity, and a policy that
		// returns it has not refused anything: it wants the 3xx itself.
		if err == nil || err == http.ErrUseLastResponse { //nolint:errorlint // identity is what net/http tests
			return err
		}
		return &refusedRedirect{err: err}
	}
	return &guarded
}

// refusedRedirect marks the error of a redirect policy that would not follow
// a redirect. The policy would refuse it again, so the call is not worth
// another try.
type refusedRedirect struct{ err error }

func (r *refusedRedirect) Error() string { return r.err.Error() }
func (r *refusedRedirect) Unwrap() error { return r.err }

// origin is the scheme, host and port of u, with the scheme's default port
// written in, so that two addresses of one origin compare equal however
// they were spelled.
func origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// isEventStream reports whether resp says its body is an event stream.
func isEventStream(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && mediaType == eventStreamType
}

// contentType names the content type of resp for an error message.
func contentType(resp *http.Response) string {
	if value := resp.Header.Get("Content-Type"); value != "" {
		return fmt.Sprintf("%q", value)
	}
	return "of no content type"
}

// errTooLarge is what a boundedReader returns for a body past the bound.
var errTooLarge = fmt.Errorf("the response body is larger than %d bytes", maxBodyBytes)

// boundedReader reads up to left bytes of r and fails if r holds more, so a
// body past the bound is an error and not an allocation without limit.
// io.LimitReader would end such a body quietly, and a reply cut off at the
// bound would then pass for a whole one.
type boundedReader struct {
	r    io.Reader
	left int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		n, err := b.r.Read(one[:])
		if n > 0 {
			return 0, errTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// readFailure marks an error that came from reading a stream's body, so that
// a connection that failed can be told from the event reader refusing what
// it read.
type readFailure struct{ err error }

func (f *readFailure) Error() string { return f.err.Error() }
func (f *readFailure) Unwrap() error { return f.err }

// streamBody is a stream's body. It tells the idle watch of every byte that
// arrives, and marks its read errors.
type streamBody struct {
	r     io.Reader
	watch *idleWatch
}

func (b streamBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.watch.touch()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		err = &readFailure{err: err}
	}
	return n, err
}

// idleWatch ends a stream that has gone quiet. A wait is the time Stream
// spends blocked on the server: for the response to begin, then for each
// event. It may last limit between one byte from the server and the next, a
// keep-alive ping or a comment line as much as an event. The time between
// waits, which is the caller's fn at work on a delta, is not watched, so a
// slow consumer is not taken for a silent server.
//
// The watch is one goroutine, started by watchIdle and gone by the time close
// returns. It uses a timer of its own and no timer callback, so nothing it
// started can outlive the Stream that made it.
type idleWatch struct {
	// limit is how long a wait for the stream may go without a byte, zero or
	// less for ever.
	limit time.Duration
	// expire ends the stream's context, which ends whatever read is waiting.
	expire func()

	mu sync.Mutex
	// waiting says a wait is under way. length is how long it may go without
	// a byte, and deadline when it next runs out; both are zero for a wait
	// with no limit.
	waiting  bool
	length   time.Duration
	deadline time.Time
	// armed is when the goroutine will next look, zero when only wake will
	// make it.
	armed   time.Time
	expired bool

	// wake tells the goroutine that a deadline is now earlier than armed.
	wake chan struct{}
	done chan struct{}
	gone chan struct{}
}

// watchIdle starts a watch. The caller must close it.
func watchIdle(limit time.Duration, expire func()) *idleWatch {
	w := &idleWatch{
		limit:  limit,
		expire: expire,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		gone:   make(chan struct{}),
	}
	go w.run()
	return w
}

// run ends the stream when a wait's deadline passes, and returns when the
// watch is closed.
func (w *idleWatch) run() {
	defer close(w.gone)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		w.mu.Lock()
		// Between waits, look again no later than the next wait could run
		// out: it will begin after now, and last at least limit.
		sleep := w.limit
		if w.waiting {
			sleep = 0
			if !w.deadline.IsZero() {
				if sleep = time.Until(w.deadline); sleep <= 0 {
					w.expired = true
					w.mu.Unlock()
					w.expire()
					return
				}
			}
		}
		var fire <-chan time.Time
		w.armed = time.Time{}
		if sleep > 0 {
			if timer == nil {
				timer = time.NewTimer(sleep)
			} else {
				timer.Reset(sleep)
			}
			fire = timer.C
			w.armed = time.Now().Add(sleep)
		}
		w.mu.Unlock()

		select {
		case <-w.done:
			return
		case <-w.wake:
		case <-fire:
		}
	}
}

// begin starts a wait that may go d without a byte, or for ever when d is
// zero or less. Every begin is followed by one end.
func (w *idleWatch) begin(d time.Duration) {
	w.mu.Lock()
	w.waiting, w.length, w.deadline = true, 0, time.Time{}
	sooner := false
	if d > 0 {
		w.length, w.deadline = d, time.Now().Add(d)
		// A wait of the usual length ends no sooner than the goroutine will
		// next look. One that is shorter has to tell it.
		sooner = w.armed.IsZero() || w.deadline.Before(w.armed)
	}
	w.mu.Unlock()
	if sooner {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// touch notes that the server sent something, which starts the wait's time
// again.
func (w *idleWatch) touch() {
	w.mu.Lock()
	if w.waiting && w.length > 0 {
		w.deadline = time.Now().Add(w.length)
	}
	w.mu.Unlock()
}

// end finishes the wait and reports whether the watch gave the stream up.
func (w *idleWatch) end() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waiting = false
	return w.expired
}

// close stops the watch and waits for its goroutine to be gone.
func (w *idleWatch) close() {
	close(w.done)
	<-w.gone
}

// drain reads the little that follows a stream's last event, so that the
// connection goes back to the pool: net/http reuses a connection only when
// its body was read to the end. The reply is already whole, so nothing here
// can fail the call. A body that has not ended within drainBytes or
// drainWait is left unread, and its connection is closed with it.
func (w *idleWatch) drain(body io.Reader) {
	w.begin(drainWait)
	_, _ = io.CopyN(io.Discard, body, drainBytes)
	w.end()
}
