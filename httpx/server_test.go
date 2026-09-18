package httpx_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerServesAndStopsWithItsContext(t *testing.T) {
	srv := httpx.NewServer(httpx.ServerOptions{
		Addr:   "127.0.0.1:0",
		Logger: log.New(log.Options{Output: io.Discard}),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("hello"))
		}),
	})
	require.NoError(t, srv.Listen())
	addr := srv.Addr()
	require.NotEmpty(t, addr)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	resp, err := http.Get("http://" + addr + "/")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "hello", string(body))

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "a shutdown asked for is not an error")
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after its context was cancelled")
	}

	// The return means nothing is still running, so the port is free.
	_, err = http.Get("http://" + addr + "/")
	assert.Error(t, err)
}

func TestServerFinishesInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	srv := httpx.NewServer(httpx.ServerOptions{
		Addr:   "127.0.0.1:0",
		Logger: log.New(log.Options{Output: io.Discard}),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			_, _ = w.Write([]byte("finished"))
		}),
	})
	require.NoError(t, srv.Listen())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + srv.Addr() + "/")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		got <- result{body: string(body), err: err}
	}()

	<-started
	cancel() // shutdown begins while the handler is still running
	close(release)

	select {
	case r := <-got:
		require.NoError(t, r.err)
		assert.Equal(t, "finished", r.body, "a graceful shutdown must not drop a request already in progress")
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request never completed")
	}

	require.NoError(t, <-done)
}

func TestServerReportsAListenFailure(t *testing.T) {
	first := httpx.NewServer(httpx.ServerOptions{
		Addr:    "127.0.0.1:0",
		Logger:  log.New(log.Options{Output: io.Discard}),
		Handler: http.NotFoundHandler(),
	})
	require.NoError(t, first.Listen())
	defer func() { _ = first.Shutdown() }()

	second := httpx.NewServer(httpx.ServerOptions{
		Addr:    first.Addr(),
		Logger:  log.New(log.Options{Output: io.Discard}),
		Handler: http.NotFoundHandler(),
	})
	err := second.ListenAndServe(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), first.Addr())
}

func TestServerListenTwiceIsAnError(t *testing.T) {
	srv := httpx.NewServer(httpx.ServerOptions{
		Addr:    "127.0.0.1:0",
		Logger:  log.New(log.Options{Output: io.Discard}),
		Handler: http.NotFoundHandler(),
	})
	require.NoError(t, srv.Listen())
	defer func() { _ = srv.Shutdown() }()
	assert.Error(t, srv.Listen())
}

func TestServerAddrBeforeListen(t *testing.T) {
	srv := httpx.NewServer(httpx.ServerOptions{Addr: "127.0.0.1:0"})
	assert.Empty(t, srv.Addr())
}

func TestServerHangsUpOnAnUnfinishedRequest(t *testing.T) {
	// Slowloris. A client that opens a connection and never finishes its
	// headers holds a server goroutine forever unless ReadHeaderTimeout is set,
	// and net/http leaves it unset by default.
	srv := httpx.NewServer(httpx.ServerOptions{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: 50 * time.Millisecond,
		Logger:            log.New(log.Options{Output: io.Discard}),
		Handler:           http.NotFoundHandler(),
	})
	require.NoError(t, srv.Listen())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.ListenAndServe(ctx) }()

	conn, err := net.Dial("tcp", srv.Addr())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n"))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, err = io.ReadAll(conn)
	// Either a 408 and a close, or a bare close. Both are the server refusing
	// to wait; a hang would trip the read deadline instead.
	assert.NoError(t, err, "the server waited out the read deadline instead of hanging up")
}
