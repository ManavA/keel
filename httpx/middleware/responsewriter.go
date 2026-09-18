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
	if !w.wroteHeader {
		w.status = status
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		// net/http implies 200 on the first write.
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wroteHeader {
			w.status = http.StatusOK
			w.wroteHeader = true
		}
		f.Flush()
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
		if !w.wroteHeader {
			w.status = http.StatusOK
			w.wroteHeader = true
		}
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
