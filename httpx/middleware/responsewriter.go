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
}

func (w *recorder) WriteHeader(status int) {
	// net/http sends a 1xx as it is given and still waits for the final
	// status, so it is neither recorded nor counted as the first.
	if isInformational(status) {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	// Past the first status the response is on the wire. Forwarding another
	// only makes net/http log it as superfluous, as it does for the 504 chi's
	// Timeout writes after a stream the deadline ended.
	if w.wroteHeader {
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

func (w *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("middleware: underlying ResponseWriter does not support hijacking")
	}
	return h.Hijack()
}

func (w *recorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		w.impliedOK()
		n, err := rf.ReadFrom(src)
		w.bytes += n
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, src)
	w.bytes += n
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
