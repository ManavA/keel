package middleware

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
)

// recorder wraps an http.ResponseWriter to remember the status and byte count,
// which a request log needs and net/http does not expose.
//
// The delegating methods matter. A wrapper implementing only ResponseWriter
// removes streaming from every handler beneath it (no Flush), breaks websocket
// upgrades (no Hijack) and turns io.Copy into a byte-by-byte loop (no
// ReadFrom). Unwrap lets http.ResponseController reach the original writer.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
	hijacked    bool
}

func (w *recorder) WriteHeader(status int) {
	// Past the first status the response is on the wire, whether the status
	// was written or implied, and a hijacked connection is no longer
	// net/http's. Forwarding anything more, a 1xx included, only makes net/http
	// log it as superfluous, as it does for the 504 chi's Timeout writes after
	// a stream its deadline ended.
	if w.wroteHeader {
		return
	}
	// net/http sends a 1xx as it is given and still waits for the final
	// status, so it is neither recorded nor counted as the first.
	if isInformational(status) {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func isInformational(status int) bool {
	return status >= 100 && status < 200 && status != http.StatusSwitchingProtocols
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.hijacked {
		return 0, http.ErrHijacked
	}
	w.impliedOK()
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush is for callers that hold only an http.Flusher, which cannot return an
// error. http.ResponseController prefers FlushError.
func (w *recorder) Flush() { _ = w.FlushError() }

// FlushError flushes whatever below can, and reports what happened to it. A
// recorder with only Flush would hide a failed flush from the
// http.ResponseController a handler flushes through, and a stream could never
// learn that its client stopped reading.
func (w *recorder) FlushError() error {
	if w.hijacked {
		// net/http drops its buffer on a hijack, so a flush now would panic.
		return http.ErrHijacked
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		w.impliedOK()
	}
	return err
}

// impliedOK records the 200 net/http sends when a handler writes or flushes
// before choosing a status.
func (w *recorder) impliedOK() {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
}

// Hijack hands the connection over. After that nothing the handler or a
// middleware above it writes to the response is for the client, so none of it
// is passed on: a status is dropped, as it would be once one was sent, and a
// write or a flush reports http.ErrHijacked, as net/http does.
func (w *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("middleware: underlying ResponseWriter does not support hijacking")
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		w.hijacked = true
		w.wroteHeader = true
	}
	return conn, rw, err
}

func (w *recorder) ReadFrom(src io.Reader) (int64, error) {
	if w.hijacked {
		return 0, http.ErrHijacked
	}
	var (
		n   int64
		err error
	)
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(w.ResponseWriter, src)
	}
	w.bytes += n
	// A body that went out carried the status net/http implies. A copy of
	// nothing wrote none, so the handler's own status is still to come.
	if n > 0 {
		w.impliedOK()
	}
	return n, err
}

func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusOrOK reports the status actually sent. A handler that returns without
// writing anything has sent 200 by the time net/http is done with it.
func (w *recorder) statusOrOK() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
