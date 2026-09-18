package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Default timeouts. ReadHeaderTimeout is the important one: net/http leaves it
// unset, and without it a client can hold a connection open indefinitely by
// sending headers one byte at a time.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 60 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
	DefaultShutdownTimeout   = 20 * time.Second
)

// ServerOptions configures Server. Every duration has a default; Addr and
// Handler do not.
type ServerOptions struct {
	// Addr is a listen address, "host:port". ":0" binds a free port, which is
	// what tests want — Server.Addr reports the one it got.
	Addr string

	Handler http.Handler

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// ShutdownTimeout bounds how long a graceful shutdown waits for in-flight
	// requests, after which the remaining connections are closed. Keep it under
	// the platform's own termination grace period, or the platform kills the
	// shutdown partway through and the dropped requests surface as 502s.
	ShutdownTimeout time.Duration

	// Logger defaults to slog.Default.
	Logger *slog.Logger

	// BaseContext, if set, is the context every request derives from. Use it to
	// put process-scoped values where handlers can reach them.
	BaseContext context.Context //nolint:containedctx // net/http's own BaseContext hook works this way
}

// Server is an http.Server that stops when its context does.
type Server struct {
	http     *http.Server
	shutdown time.Duration
	log      *slog.Logger
	listener net.Listener
}

// NewServer returns a Server. It does not listen; ListenAndServe does.
func NewServer(opts ServerOptions) *Server {
	orDefault := func(v, def time.Duration) time.Duration {
		if v <= 0 {
			return def
		}
		return v
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           opts.Handler,
		ReadHeaderTimeout: orDefault(opts.ReadHeaderTimeout, DefaultReadHeaderTimeout),
		ReadTimeout:       orDefault(opts.ReadTimeout, DefaultReadTimeout),
		WriteTimeout:      orDefault(opts.WriteTimeout, DefaultWriteTimeout),
		IdleTimeout:       orDefault(opts.IdleTimeout, DefaultIdleTimeout),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	if opts.BaseContext != nil {
		base := opts.BaseContext
		srv.BaseContext = func(net.Listener) context.Context { return base }
	}

	return &Server{
		http:     srv,
		shutdown: orDefault(opts.ShutdownTimeout, DefaultShutdownTimeout),
		log:      logger,
	}
}

// Listen binds the address without serving. ListenAndServe calls it; call it
// yourself when you need Addr before the server starts, such as a test building
// a URL against port 0.
func (s *Server) Listen() error {
	if s.listener != nil {
		return errors.New("httpx: already listening")
	}
	addr := s.http.Addr
	if addr == "" {
		addr = ":http"
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("httpx: listen on %s: %w", addr, err)
	}
	s.listener = ln
	return nil
}

// Addr is the address actually bound, or "" before Listen.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// ListenAndServe serves until ctx is cancelled, then shuts down gracefully and
// returns. It returns nil on a clean shutdown.
//
// Cancelling the context is how you stop it, which means the signal handling
// stays in main where you can see it:
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//	defer stop()
//	err := srv.ListenAndServe(ctx)
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.listener == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}

	serveErr := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", "addr", s.Addr())
		serveErr <- s.http.Serve(s.listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("httpx: serve: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("http server shutting down", "timeout", s.shutdown)
	if err := s.Shutdown(); err != nil {
		return err
	}
	// Serve returns ErrServerClosed once Shutdown has finished. Waiting for it
	// makes ListenAndServe's return mean that nothing is still running.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("httpx: serve: %w", err)
	}
	return nil
}

// Shutdown stops the server, waiting up to ShutdownTimeout for in-flight
// requests. It is safe to call from anywhere; ListenAndServe calls it for you
// when its context is cancelled.
func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), s.shutdown)
	defer cancel()

	if err := s.http.Shutdown(ctx); err != nil {
		// The deadline passed with requests still running. Close them rather
		// than holding connections that will never finish.
		s.log.Warn("graceful shutdown timed out, closing connections", "error", err)
		if closeErr := s.http.Close(); closeErr != nil {
			return fmt.Errorf("httpx: shutdown: %w (close: %w)", err, closeErr)
		}
		return fmt.Errorf("httpx: shutdown: %w", err)
	}
	return nil
}
