package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/httpx"
)

// Defaults and bounds.
const (
	defaultPollInterval = 500 * time.Millisecond
	defaultHeartbeat    = 15 * time.Second

	// maxBody is the most a request body may hold. The only thing a body
	// carries is a reason.
	maxBody = 4 << 10

	// defaultLimit and maxLimit are the bounds on a listing's length. They are
	// the store's own, repeated here so that a limit outside them is refused
	// and not quietly clamped.
	defaultLimit = 50
	maxLimit     = 200
)

// Runs is what the API needs from an engine. *agent.Engine satisfies it.
//
// Changes must be one consistent view of the run, as the store's contract
// makes it: the run's Rev is at least that of every step and approval returned
// with it, and nothing at or below that Rev is missing. The event stream never
// skipping a change rests on it.
type Runs interface {
	GetRun(ctx context.Context, id string) (agent.Run, error)
	ListRuns(ctx context.Context, f agent.RunFilter) ([]agent.Run, error)
	Changes(ctx context.Context, runID string, since int64) (agent.Changes, error)
	ListApprovals(ctx context.Context, f agent.ApprovalFilter) ([]agent.Approval, error)
	Approve(ctx context.Context, approvalID, by, reason string) (agent.Approval, error)
	Decline(ctx context.Context, approvalID, by, reason string) (agent.Approval, error)
	Cancel(ctx context.Context, runID, by, reason string) error
}

// Options configures an API. Only Runs is required.
type Options struct {
	Runs Runs
	// Actor names who is making a request, for the record of an approval or
	// a cancellation. Nil, or a name with nothing in it but white space,
	// refuses those requests with 403, so the zero value serves a read-only
	// API. It is a name for the record and not a check: whoever it names may
	// decide anything the API can see.
	Actor func(r *http.Request) string
	// PollInterval is how often an event stream reads the journal. Default
	// 500 milliseconds; zero or less means the default.
	PollInterval time.Duration
	// Heartbeat is how often an idle event stream sends a comment line, to
	// keep proxies from closing it. Default 15 seconds; zero or less means
	// the default.
	Heartbeat time.Duration
	// CrossOrigin guards the three routes that change something against a
	// request a browser makes for a page on another origin: one that is
	// cross-origin by Sec-Fetch-Site or by Origin is refused with 403, before
	// Actor is asked. A request with neither header is not a browser acting
	// for a page, and passes. Nil means a protection with no trusted origins;
	// a service whose front end is on another origin supplies one that trusts
	// it. Its deny handler is not used: the answer is the same generic 403 as
	// any other refusal. Reading, and the event stream, are not guarded.
	CrossOrigin *http.CrossOriginProtection
	// Logger defaults to slog.Default. A logger that httpx.NewRouter put on
	// the request is preferred to it, as everywhere in this module.
	Logger *slog.Logger
}

// API is the HTTP surface of an engine.
type API struct {
	runs        Runs
	actor       func(r *http.Request) string
	crossOrigin *http.CrossOriginProtection
	poll        time.Duration
	heartbeat   time.Duration
	log         *slog.Logger
}

// New builds an API. It returns an error when Runs is nil.
func New(opts Options) (*API, error) {
	if opts.Runs == nil {
		return nil, errors.New("agent/httpapi: Options.Runs is required")
	}
	a := &API{
		runs:        opts.Runs,
		actor:       opts.Actor,
		crossOrigin: opts.CrossOrigin,
		poll:        opts.PollInterval,
		heartbeat:   opts.Heartbeat,
		log:         opts.Logger,
	}
	if a.crossOrigin == nil {
		a.crossOrigin = http.NewCrossOriginProtection()
	}
	// Negative would be a timer that is always ready, and a stream that reads
	// the store in a loop.
	if a.poll <= 0 {
		a.poll = defaultPollInterval
	}
	if a.heartbeat <= 0 {
		a.heartbeat = defaultHeartbeat
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a, nil
}

// Routes returns the router. Mount it behind whatever guards operator
// traffic: it serves the journal, which holds whatever the tools handled.
//
//	GET  /runs                  a page of runs, newest first: {"runs": [...], "next": "..."}
//	                            query: status, agent, parent, limit (1 to 200, 50), cursor
//	GET  /runs/{id}             the run
//	GET  /runs/{id}/timeline    {"run": ..., "steps": [...], "approvals": [...]}
//	GET  /runs/{id}/events      the run as a server-sent event stream
//	POST /runs/{id}/cancel      202; 409 for a run that has ended
//	GET  /approvals             {"approvals": [...]}, oldest first
//	                            query: status, run, limit (1 to 200, 50)
//	POST /approvals/{id}/approve  the approval as decided; 409 when already decided
//	POST /approvals/{id}/decline  the same
//
// A request to change something carries {"reason": "..."} or nothing, needs an
// actor, and is refused with 403 when a browser makes it for another origin. An
// id the engine does not know is 404, a request that cannot be acted on is 400,
// and anything else the engine says is 500.
func (a *API) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(a.prepare)

	r.Get("/runs", a.listRuns)
	r.Get("/runs/{id}", a.getRun)
	r.Get("/runs/{id}/timeline", a.timeline)
	r.Get("/runs/{id}/events", a.events)
	// Registered for itself so that a HEAD reaches the handler whether or not
	// the router above turns HEAD into GET: the handler must see it either way.
	r.Head("/runs/{id}/events", a.events)
	r.Get("/approvals", a.listApprovals)

	// The routes that change something, behind the cross-origin check.
	guarded := r.With(a.refuseCrossOrigin)
	guarded.Post("/runs/{id}/cancel", a.cancel)
	guarded.Post("/approvals/{id}/approve", a.decide(a.runs.Approve, "approve"))
	guarded.Post("/approvals/{id}/decline", a.decide(a.runs.Decline, "decline"))
	return r
}

// refuseCrossOrigin answers 403 to a request that a browser makes for a page
// on another origin. It is not authentication: it says nothing of who is
// asking, which is Options.Actor's to say.
func (a *API) refuseCrossOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.crossOrigin.Check(r); err != nil {
			httpx.Error(w, r, http.StatusForbidden, fmt.Errorf("agent/httpapi: %w", err))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logger is the request's logger when httpx.NewRouter put one there, and the
// one in Options otherwise.
func (a *API) logger(ctx context.Context) *slog.Logger {
	if l := httpx.Logger(ctx); l != slog.Default() {
		return l
	}
	return a.log
}

// prepare is what every request starts with. It puts the logger on the request,
// so that the failures httpx's helpers log are logged through it, and says that
// nothing served here is to be kept: the journal is private to whoever may read
// it. The event stream sets its own Cache-Control over this one.
func (a *API) prepare(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx := r.Context()
		next.ServeHTTP(w, r.WithContext(httpx.WithLogger(ctx, a.logger(ctx))))
	})
}

// fail answers a failure of the engine. A client that has gone is told nothing
// and nothing is raised: whatever the engine was doing when it found out, the
// fault is not the service's. Otherwise a name the engine does not know is 404,
// whatever its form: the engine judges it. What has been settled already is
// 409. A request whose time ran out with the client still there is 503, and
// anything else is the service's fault and says nothing of itself. The cause is
// in the log.
func fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	err = fmt.Errorf("agent/httpapi: %s: %w", what, err)
	ctx := r.Context()
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		httpx.Logger(ctx).LogAttrs(ctx, slog.LevelDebug, "request abandoned",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("error", err))
	case errors.Is(err, agent.ErrNotFound):
		httpx.NotFound(w, r)
	case errors.Is(err, agent.ErrAlreadyDecided), errors.Is(err, agent.ErrFinished):
		httpx.Error(w, r, http.StatusConflict, err)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		httpx.Error(w, r, http.StatusServiceUnavailable, err)
	default:
		httpx.InternalError(w, r, err)
	}
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := a.runs.GetRun(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		fail(w, r, "get run", err)
		return
	}
	httpx.JSON(w, http.StatusOK, run)
}

func (a *API) timeline(w http.ResponseWriter, r *http.Request) {
	changes, err := a.runs.Changes(r.Context(), chi.URLParam(r, "id"), 0)
	if err != nil {
		fail(w, r, "read timeline", err)
		return
	}
	changes.Steps = withoutOpaque(changes.Steps)
	if changes.Approvals == nil {
		changes.Approvals = []agent.Approval{}
	}
	httpx.JSON(w, http.StatusOK, changes)
}

// withoutOpaque returns steps as they may be served: with no Message.Opaque,
// which is the provider's own form of a turn and can hold its reasoning. The
// slice is new and so is each message changed; what the engine returned is not
// touched, and the result is never nil, which would be served as null.
func withoutOpaque(steps []agent.Step) []agent.Step {
	out := make([]agent.Step, len(steps))
	for i, st := range steps {
		if st.Message != nil && st.Message.Opaque != nil {
			m := *st.Message
			m.Opaque = nil
			st.Message = &m
		}
		out[i] = st
	}
	return out
}

// runPage is the body of a listing of runs.
type runPage struct {
	Runs []agent.Run `json:"runs"`
	// Next is where the following page starts, and is absent when this page was
	// not full. A full last page is followed by one empty page: the engine is
	// not asked for one run more than the limit to find out.
	Next string `json:"next,omitempty"`
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request) {
	filter, err := runFilter(r.URL.Query())
	if err != nil {
		httpx.BadRequest(w, r, fmt.Errorf("agent/httpapi: %w", err))
		return
	}
	runs, err := a.runs.ListRuns(r.Context(), filter)
	if err != nil {
		fail(w, r, "list runs", err)
		return
	}
	page := runPage{Runs: runs}
	if page.Runs == nil {
		page.Runs = []agent.Run{}
	}
	if len(runs) >= filter.Limit {
		last := runs[len(runs)-1]
		page.Next, err = encodeCursor(agent.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			httpx.InternalError(w, r, fmt.Errorf("agent/httpapi: list runs: %w", err))
			return
		}
	}
	httpx.JSON(w, http.StatusOK, page)
}

var runStatuses = []agent.Status{
	agent.StatusRunnable, agent.StatusWaiting, agent.StatusCompleted, agent.StatusFailed, agent.StatusCancelled,
}

var approvalStatuses = []agent.ApprovalStatus{
	agent.ApprovalPending, agent.ApprovalApproved, agent.ApprovalDeclined, agent.ApprovalExpired, agent.ApprovalCancelled,
}

// runFilter reads the query of a listing of runs. Whatever cannot be acted on
// is an error and never a default: a limit out of range is not clamped, and a
// cursor that does not read is not the first page.
func runFilter(q url.Values) (agent.RunFilter, error) {
	var f agent.RunFilter
	status, err := param(q, "status")
	if err != nil {
		return f, err
	}
	if status != "" {
		if !slices.Contains(runStatuses, agent.Status(status)) {
			return f, fmt.Errorf("status %q is not a run status", status)
		}
		f.Status = agent.Status(status)
	}
	if f.Agent, err = param(q, "agent"); err != nil {
		return f, err
	}
	if f.ParentID, err = param(q, "parent"); err != nil {
		return f, err
	}
	token, err := param(q, "cursor")
	if err != nil {
		return f, err
	}
	if token != "" {
		if f.Before, err = decodeCursor(token); err != nil {
			return f, err
		}
	}
	f.Limit, err = limit(q)
	return f, err
}

func (a *API) listApprovals(w http.ResponseWriter, r *http.Request) {
	filter, err := approvalFilter(r.URL.Query())
	if err != nil {
		httpx.BadRequest(w, r, fmt.Errorf("agent/httpapi: %w", err))
		return
	}
	approvals, err := a.runs.ListApprovals(r.Context(), filter)
	if err != nil {
		fail(w, r, "list approvals", err)
		return
	}
	if approvals == nil {
		approvals = []agent.Approval{}
	}
	httpx.JSON(w, http.StatusOK, struct {
		Approvals []agent.Approval `json:"approvals"`
	}{approvals})
}

func approvalFilter(q url.Values) (agent.ApprovalFilter, error) {
	var f agent.ApprovalFilter
	status, err := param(q, "status")
	if err != nil {
		return f, err
	}
	if status != "" {
		if !slices.Contains(approvalStatuses, agent.ApprovalStatus(status)) {
			return f, fmt.Errorf("status %q is not an approval status", status)
		}
		f.Status = agent.ApprovalStatus(status)
	}
	if f.RunID, err = param(q, "run"); err != nil {
		return f, err
	}
	f.Limit, err = limit(q)
	return f, err
}

// param is the value of a query parameter, empty when it is absent or empty.
// A parameter given twice is an error: one of the two would be dropped.
func param(q url.Values, name string) (string, error) {
	values := q[name]
	switch len(values) {
	case 0:
		return "", nil
	case 1:
		return values[0], nil
	}
	return "", fmt.Errorf("%s is given %d times", name, len(values))
}

// limit reads the limit, which is a whole number from 1 to 200, and 50 when
// absent. One that is given and empty is an error, as is any other: the page
// a client asked for is the page it is given or none.
func limit(q url.Values) (int, error) {
	if _, given := q["limit"]; !given {
		return defaultLimit, nil
	}
	raw, err := param(q, "limit")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLimit {
		return 0, fmt.Errorf("limit %q is not a whole number from 1 to %d", raw, maxLimit)
	}
	return n, nil
}

// cursor is what a cursor token holds: the position of a run in a listing,
// which is newest first and then by id. The token is that, in JSON and base64,
// and a client is told to treat it as opaque.
type cursor struct {
	T  time.Time `json:"t"`
	ID string    `json:"id"`
}

func encodeCursor(c agent.Cursor) (string, error) {
	raw, err := json.Marshal(cursor{T: c.CreatedAt, ID: c.ID})
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeCursor reads a token that encodeCursor made. A cursor comes from the
// client, so it is input and is read strictly: a time that is not one, or an id
// that is not a UUID in the canonical form the stores keep (uuid.Parse also
// reads upper case, braces, a URN and no hyphens, which would reach a store as
// a name it does not know), is an error here and is never given to the engine.
func decodeCursor(token string) (*agent.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("cursor is not one this API made: %w", err)
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("cursor is not one this API made: %w", err)
	}
	if c.T.IsZero() {
		return nil, errors.New("cursor has no time")
	}
	if id, err := uuid.Parse(c.ID); err != nil || id.String() != c.ID {
		return nil, errors.New("cursor's id is not a UUID in the canonical form")
	}
	return &agent.Cursor{CreatedAt: c.T, ID: c.ID}, nil
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	by, reason, ok := a.actorAndReason(w, r)
	if !ok {
		return
	}
	if err := a.runs.Cancel(r.Context(), chi.URLParam(r, "id"), by, reason); err != nil {
		fail(w, r, "cancel run", err)
		return
	}
	// Accepted, not done: a run being executed learns of it from its next
	// heartbeat, and a run waiting is finished by the next execution.
	httpx.JSON(w, http.StatusAccepted, nil)
}

// decide is the handler of approve and of decline, which differ in the call
// they make.
func (a *API) decide(call func(ctx context.Context, approvalID, by, reason string) (agent.Approval, error), what string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		by, reason, ok := a.actorAndReason(w, r)
		if !ok {
			return
		}
		approval, err := call(r.Context(), chi.URLParam(r, "id"), by, reason)
		if err != nil {
			// An approval already answered comes back with that answer and the
			// error. It is not served: a client told 409 learns no more than that.
			fail(w, r, what, err)
			return
		}
		httpx.JSON(w, http.StatusOK, approval)
	}
}

// actorAndReason is what the three routes that change something start with. It
// names the actor and reads the reason, and answers the request itself,
// returning false, when no one is named or the body is not acceptable. A name
// is what is left after the white space round it, and nothing left is no one:
// a decision recorded under no name is a decision by nobody. The actor is asked
// first: a request with no one to record is not read a body. Whether the actor
// may do this is not asked here at all, and is the mount's to decide.
func (a *API) actorAndReason(w http.ResponseWriter, r *http.Request) (by, reason string, ok bool) {
	if a.actor != nil {
		by = strings.TrimSpace(a.actor(r))
	}
	if by == "" {
		httpx.Error(w, r, http.StatusForbidden, errors.New("agent/httpapi: the request names no actor"))
		return "", "", false
	}
	reason, err := readReason(w, r)
	if err != nil {
		httpx.BadRequest(w, r, fmt.Errorf("agent/httpapi: %w", err))
		return "", "", false
	}
	return by, reason, true
}

// readReason reads the one thing a body may say. No body, an empty one and an
// empty object all say nothing; anything else that is not an object with only
// a reason in it, or that is over 4 KiB, is an error. So is a reason a database
// could not keep: the body is checked as bytes, since decoding would turn bytes
// that are not UTF-8 into U+FFFD without a word, and a NUL is refused after.
// The reason is no longer than the body, which is what bounds its length.
func readReason(w http.ResponseWriter, r *http.Request) (string, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return "", nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if !utf8.Valid(body) {
		return "", errors.New("read body: not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var in struct {
		Reason string `json:"reason"`
	}
	if err := dec.Decode(&in); err != nil {
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		return "", fmt.Errorf("read body: %w", err)
	}
	// Nothing may follow the object.
	switch _, err := dec.Token(); {
	case err == nil:
		return "", errors.New("read body: more than one JSON value")
	case !errors.Is(err, io.EOF):
		return "", fmt.Errorf("read body: %w", err)
	}
	if strings.ContainsRune(in.Reason, 0) {
		return "", errors.New("read body: the reason has a NUL character")
	}
	return in.Reason, nil
}
