package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ManavA/keel/httpx/buildinfo"
	"github.com/ManavA/keel/perf"
)

// A Check reports whether one dependency is usable right now, returning nil
// when it is.
//
// Give it a real query. A check that only opens a TCP connection reports green
// against a database that refuses every statement. pg.HealthCheck returns one.
type Check func(context.Context) error

// HealthOptions configures Health.
type HealthOptions struct {
	// Checks are the dependencies /readyz reports on, by name. /healthz never
	// runs them.
	Checks map[string]Check

	// Timeout bounds the whole set of checks. Default 3 seconds. Without it the
	// prober's own timeout decides the outcome, and it cannot say which
	// dependency it was waiting for.
	Timeout time.Duration

	// CacheTTL is how long a result is reused. Default 5 seconds. Readiness is
	// polled by every prober and load balancer on its own schedule, and without
	// a cache all of it reaches the database.
	//
	// A failed result is cached too, including one caused by Timeout: a
	// dependency that cannot answer inside the budget this endpoint set is not
	// ready by that endpoint's own definition, and caching it stops a
	// struggling dependency being asked again by every prober in turn.
	//
	// The one result not cached is a check that failed with context.Canceled.
	// That is the prober giving up, and it says nothing about the dependency.
	CacheTTL time.Duration

	// LivenessPath and ReadinessPath default to /healthz and /readyz.
	LivenessPath  string
	ReadinessPath string

	// ExposeCheckErrors puts the error text from a failing check into the
	// response. Off by default: a driver error names the host, the database and
	// sometimes the credentials. The error is logged either way. Turn it on
	// when the endpoint is unreachable from outside.
	ExposeCheckErrors bool

	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// healthResponse is what both endpoints return.
type healthResponse struct {
	Status string `json:"status"`

	// Build names the deployment that answered. Always present, reading
	// "unknown" rather than being omitted: a missing field cannot be told apart
	// from a deployment older than the field itself.
	Build buildinfo.Info `json:"build"`

	Checks map[string]string `json:"checks,omitempty"`
}

// Health returns a handler serving liveness and readiness.
//
// They answer different questions. Liveness asks whether the process is
// running; a failure means restart it. Readiness asks whether it can serve; a
// failure means route traffic elsewhere for now. A restart policy pointed at a
// readiness endpoint restarts every instance at once during a database blip.
//
// So /healthz checks nothing and answers 200 while the process can serve a
// request, and /readyz runs the checks and answers 503 when one fails.
func Health(opts HealthOptions) http.Handler {
	h := &healthHandler{
		checks:   opts.Checks,
		timeout:  opts.Timeout,
		cacheTTL: opts.CacheTTL,
		expose:   opts.ExposeCheckErrors,
		log:      opts.Logger,
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.timeout <= 0 {
		h.timeout = 3 * time.Second
	}
	if h.cacheTTL <= 0 {
		h.cacheTTL = 5 * time.Second
	}

	livenessPath := opts.LivenessPath
	if livenessPath == "" {
		livenessPath = "/healthz"
	}
	readinessPath := opts.ReadinessPath
	if readinessPath == "" {
		readinessPath = "/readyz"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+livenessPath, h.live)
	mux.HandleFunc("GET "+readinessPath, h.ready)
	return mux
}

type healthHandler struct {
	checks   map[string]Check
	timeout  time.Duration
	cacheTTL time.Duration
	expose   bool
	log      *slog.Logger

	mu       sync.Mutex
	cached   healthResponse
	cachedOK bool
	cachedAt time.Time

	// flight collapses concurrent check runs into one: when the cache has
	// expired, N probes arriving together would otherwise each run every
	// check before one result is cached.
	flight perf.SingleFlight
}

// readinessResult is the shared outcome of one check run.
type readinessResult struct {
	resp healthResponse
	ok   bool
}

func (h *healthHandler) live(w http.ResponseWriter, _ *http.Request) {
	noStore(w)
	JSON(w, http.StatusOK, healthResponse{Status: "ok", Build: buildinfo.Get()})
}

func (h *healthHandler) ready(w http.ResponseWriter, r *http.Request) {
	noStore(w)

	resp, ok := h.evaluate(r.Context())
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	JSON(w, status, resp)
}

func (h *healthHandler) evaluate(ctx context.Context) (healthResponse, bool) {
	h.mu.Lock()
	if !h.cachedAt.IsZero() && time.Since(h.cachedAt) < h.cacheTTL {
		resp, ok := h.cached, h.cachedOK
		h.mu.Unlock()
		return resp, ok
	}
	h.mu.Unlock()

	// One probe runs the checks and the rest wait on its result. The wait is
	// detached from any one request and bounded by Timeout, like the run
	// itself: a prober hanging up must neither cancel the shared run nor wait
	// on it forever. A wait that outlasts Timeout is a hung check, and a
	// probe that cannot get an answer inside the budget this endpoint set
	// fails closed.
	waitCtx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	v, _, err := h.flight.Do(waitCtx, "readyz", func(context.Context) (any, error) {
		resp, ok, cacheable := h.run(ctx)
		if cacheable {
			h.mu.Lock()
			h.cached, h.cachedOK, h.cachedAt = resp, ok, time.Now()
			h.mu.Unlock()
		}
		return readinessResult{resp: resp, ok: ok}, nil
	})
	if err != nil {
		return healthResponse{Status: "degraded", Build: buildinfo.Get()}, false
	}
	r, ok := v.(readinessResult)
	if !ok {
		return healthResponse{Status: "degraded", Build: buildinfo.Get()}, false
	}
	return r.resp, r.ok
}

// run executes the checks. The result is cacheable unless a check failed
// because a context was cancelled.
func (h *healthHandler) run(ctx context.Context) (resp healthResponse, healthy, cacheable bool) {
	// Detached from the request. Deriving the check context from r.Context()
	// means one prober hanging up mid-check makes every check return
	// context.Canceled, and that result is then served from the cache to
	// healthy probers for the rest of CacheTTL — a client disconnect taking the
	// instance out of rotation with nothing wrong with it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.timeout)
	defer cancel()

	results := make(map[string]string, len(h.checks))
	healthy = true
	cacheable = true

	// Sequential, in name order. Checks are few and cached, and a fixed order
	// makes the failure log reproducible.
	for _, name := range sortedKeys(h.checks) {
		if err := h.checks[name](ctx); err != nil {
			h.log.ErrorContext(ctx, "readiness check failed", "check", name, "error", err)
			results[name] = "down"
			if h.expose {
				results[name] = "down: " + err.Error()
			}
			healthy = false
			// A cancellation says nothing about the dependency, so it must not
			// be remembered. A deadline does, and is cached like any failure.
			if errors.Is(err, context.Canceled) {
				cacheable = false
			}
			continue
		}
		results[name] = "ok"
	}

	status := "ok"
	if !healthy {
		status = "degraded"
	}
	return healthResponse{Status: status, Build: buildinfo.Get(), Checks: results}, healthy, cacheable
}

func sortedKeys(m map[string]Check) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// noStore keeps a health response out of every cache: it describes this
// instance right now, and a cached "ok" outlives what it described.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}
