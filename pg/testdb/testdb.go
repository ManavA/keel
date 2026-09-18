// Package testdb runs a real Postgres in Docker for tests, with a readiness
// check that does not report a database ready before it is.
//
// The obvious probe is `docker exec <container> pg_isready`, and it is wrong.
// The official Postgres entrypoint starts the server twice on first run: once
// on a Unix socket only, to execute the init scripts, then again on the
// published TCP port. A probe going in through `docker exec` talks to the local
// socket and sees the first server; the tests connect over the published port
// and cannot. The gap is about a second and a half, which is long enough for
// the first migration to fail in a way that reads as a migration bug.
//
// So Ready connects the way the tests do: over host TCP, using the same
// connection string, pinned to 127.0.0.1 rather than localhost. The pinning
// matters on its own, because `docker run -p` can publish the IPv4 side before
// the IPv6 side and a host where localhost resolves to ::1 would have the probe
// and the application on different sockets.
//
// Ready also reads pg_postmaster_start_time() twice and requires the two to
// agree. That covers a restart after the port is already up — a `docker
// restart`, a crash-recovery cycle — and not the init sequence above, which
// host TCP cannot observe at all.
package testdb

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultImage is the Postgres image started when Options.Image is empty.
// Pinned to a version rather than :latest, so the database version does not
// change when a mirror updates.
const DefaultImage = "postgres:16-alpine"

// Options configures Start. The zero value starts DefaultImage on a free port
// with a session-unique container name.
type Options struct {
	// Image defaults to DefaultImage. Any image with the official Postgres
	// entrypoint works, including postgis/postgis.
	Image string

	// Name is the container name. The default is unique per process, so two
	// suites can run at once; with a fixed name the second would remove the
	// first one's container.
	Name string

	// Port on the host. Zero picks a free one, for the same reason.
	Port int

	// User, Password and Database default to "keel".
	User     string
	Password string
	Database string

	// ReadyTimeout bounds the wait, default 90 seconds.
	ReadyTimeout time.Duration

	// StabilityGap is how far apart the two postmaster start-time reads are,
	// default 1 second.
	StabilityGap time.Duration

	// Logf receives progress. Pass t.Logf.
	Logf func(format string, args ...any)
}

// DB is a running Postgres container.
type DB struct {
	// URL is the connection string, pinned to 127.0.0.1, and is the string the
	// readiness probe used.
	URL string

	Name string
	Port int

	stop     func()
	stopOnce sync.Once
}

// Start launches a container and waits for it to be ready.
//
// The caller must call Close. Start also removes the container on SIGINT and
// SIGTERM, since `go test` interrupted at the keyboard runs no deferred
// function and no t.Cleanup.
func Start(ctx context.Context, opts Options) (*DB, error) {
	if err := Available(); err != nil {
		return nil, err
	}

	image := orString(opts.Image, DefaultImage)
	user := orString(opts.User, "keel")
	password := orString(opts.Password, "keel")
	database := orString(opts.Database, "keel")
	name := opts.Name
	if name == "" {
		name = fmt.Sprintf("keel-testdb-%d-%d", os.Getpid(), time.Now().UnixNano()%1e6)
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	port := opts.Port
	if port == 0 {
		var err error
		port, err = freePort()
		if err != nil {
			return nil, err
		}
	}

	logf("testdb: starting %s as %s on port %d", image, name, port)
	// The arguments come from Options, which is the caller's own test code, not
	// from anything a request or a file could reach.
	run := exec.CommandContext(ctx, "docker", "run", "--detach", "--rm", //nolint:gosec // G204: arguments are the caller's own configuration
		"--name", name,
		"--env", "POSTGRES_USER="+user,
		"--env", "POSTGRES_PASSWORD="+password,
		"--env", "POSTGRES_DB="+database,
		// 127.0.0.1 explicitly: not reachable from outside this machine, and
		// published on the address family the probe uses.
		"--publish", fmt.Sprintf("127.0.0.1:%d:5432", port),
		image,
	)
	if out, err := run.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("testdb: docker run: %w: %s", err, strings.TrimSpace(string(out)))
	}

	db := &DB{
		URL: fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable",
			user, password, port, database),
		Name: name,
		Port: port,
	}
	db.stop = func() { removeContainer(name) }
	unregister := registerForSignals(name)

	readyCtx, cancel := context.WithTimeout(ctx, orDuration(opts.ReadyTimeout, 90*time.Second))
	defer cancel()

	if err := Ready(readyCtx, pgxProbe{url: db.URL, container: name}, orDuration(opts.StabilityGap, time.Second)); err != nil {
		unregister()
		removeContainer(name)
		return nil, err
	}

	prevStop := db.stop
	db.stop = func() {
		unregister()
		prevStop()
	}
	logf("testdb: %s ready", name)
	return db, nil
}

// Close removes the container. Safe to call more than once.
func (db *DB) Close() {
	if db == nil {
		return
	}
	db.stopOnce.Do(func() {
		if db.stop != nil {
			db.stop()
		}
	})
}

// Available reports whether the harness can run. Check it before deciding
// between skipping and failing.
func Available() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("testdb: docker is not installed: %w", err)
	}
	cmd := exec.CommandContext(context.Background(), "docker", "info", "--format", "{{.ServerVersion}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("testdb: docker is installed but not usable: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeContainer(name string) {
	// Its own context: this runs from a signal handler and from Close, and the
	// test's context may already be gone.
	cmd := exec.CommandContext(context.Background(), "docker", "rm", "--force", name)
	_ = cmd.Run()
}

// signalCleanup removes every container this process started, on INT or TERM.
// One handler for the whole process: several racing to re-raise the signal
// would produce an exit code depending on which won.
var signalCleanup struct {
	once  sync.Once
	mu    sync.Mutex
	names map[string]struct{}
}

func registerForSignals(name string) (unregister func()) {
	signalCleanup.once.Do(func() {
		signalCleanup.names = map[string]struct{}{}
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		go func() {
			sig := <-ch
			signalCleanup.mu.Lock()
			for n := range signalCleanup.names {
				removeContainer(n)
			}
			signalCleanup.mu.Unlock()

			// Re-raise so the process dies as it would have. os.Exit here would
			// hide the signal from `go test`, which reports an interrupted run
			// differently from a failed one.
			signal.Stop(ch)
			if s, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(os.Getpid(), s)
			}
		}()
	})

	signalCleanup.mu.Lock()
	signalCleanup.names[name] = struct{}{}
	signalCleanup.mu.Unlock()

	return func() {
		signalCleanup.mu.Lock()
		delete(signalCleanup.names, name)
		signalCleanup.mu.Unlock()
	}
}

// pgxProbe is the real Probe: a host-TCP connection over the application's own
// URL.
type pgxProbe struct {
	url       string
	container string
}

func (p pgxProbe) Accepting(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, p.url)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	var one int
	return conn.QueryRow(ctx, "select 1").Scan(&one)
}

func (p pgxProbe) StartTime(ctx context.Context) (time.Time, error) {
	conn, err := pgx.Connect(ctx, p.url)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = conn.Close(ctx) }()

	var t time.Time
	if err := conn.QueryRow(ctx, "select pg_postmaster_start_time()").Scan(&t); err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func orString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// freePort asks the kernel for an unused port by binding one and closing it.
//
// Something else can take the port between the close and `docker run`, in which
// case docker fails immediately with a clear message. A fixed default port
// would instead fail later, inside the readiness wait, looking like a database
// problem.
func freePort() (int, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("testdb: find a free port: %w", err)
	}
	defer func() { _ = ln.Close() }()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("testdb: find a free port: unexpected address %s", ln.Addr())
	}
	return addr.Port, nil
}
