package httpapi_test

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/httpapi"
)

// waitFor bounds every wait on the server in these tests. It is long on
// purpose: a test that waits for something to happen passes however slow the
// machine is, and fails only when the thing never comes.
const waitFor = 20 * time.Second

// secretThought is what a step's opaque form holds in these tests. It must
// never appear in anything the API serves.
const secretThought = "SECRET-INTERNAL-REASONING"

// letterID is an id with letters in it, so that its upper-case spelling is a
// different string. The others are all digits.
const letterID = "0a0b0c0d-0e0f-4a0b-8c0d-0e0f0a0b0c0d"

// uid is the canonical form of the nth id.
func uid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// at is a moment n seconds after the epoch, so runs made in order are
// distinguishable and sort as they were made.
func at(n int) time.Time { return epoch.Add(time.Duration(n) * time.Second) }

func mkRun(id string, mods ...func(*agent.Run)) agent.Run {
	r := agent.Run{
		ID:        id,
		Agent:     "reviewer",
		Status:    agent.StatusRunnable,
		Input:     "look at this",
		Rev:       1,
		CreatedAt: at(0),
		UpdatedAt: at(0),
	}
	for _, mod := range mods {
		mod(&r)
	}
	return r
}

func withStatus(s agent.Status) func(*agent.Run) { return func(r *agent.Run) { r.Status = s } }
func withRev(rev int64) func(*agent.Run)         { return func(r *agent.Run) { r.Rev = rev } }
func withAgent(name string) func(*agent.Run)     { return func(r *agent.Run) { r.Agent = name } }
func withCreatedAt(t time.Time) func(*agent.Run) { return func(r *agent.Run) { r.CreatedAt = t } }

// mkStep is a completed model step whose reply carries an opaque form.
func mkStep(runID string, seq int, rev int64) agent.Step {
	return agent.Step{
		RunID:  runID,
		Seq:    seq,
		Kind:   agent.StepModel,
		Status: agent.StepCompleted,
		Name:   "model-a",
		Message: &agent.Message{
			Role: agent.RoleAssistant,
			Text: fmt.Sprintf("reply %d", seq),
			Opaque: &agent.Opaque{
				Provider: "provider-a",
				Data:     json.RawMessage(`{"thinking":"` + secretThought + `"}`),
			},
		},
		Stop:      agent.StopEnd,
		Rev:       rev,
		CreatedAt: at(seq),
	}
}

func mkApproval(id, runID string, seq int, rev int64) agent.Approval {
	return agent.Approval{
		ID:          id,
		RunID:       runID,
		Seq:         seq,
		Cause:       agent.CauseGuard,
		Tool:        "send",
		Input:       json.RawMessage(`{"to":"somebody"}`),
		Action:      agent.Action{Kind: "run", Target: "send"},
		Rule:        "ask first",
		Status:      agent.ApprovalPending,
		Rev:         rev,
		RequestedAt: at(seq),
	}
}

// changesCall is one call to Changes and what it returned.
type changesCall struct {
	RunID string
	Since int64
	Rev   int64 // the run's Rev in the answer; 0 when it was an error
	Err   error
	// HasDeadline is whether the context the call was made with had one. The
	// API puts no timeout around a stream.
	HasDeadline bool
}

// fakeRuns is a Runs that keeps its answers in memory, orders a listing as a
// store does and records every call. A test changes what it holds while a
// stream is open through edit.
type fakeRuns struct {
	mu sync.Mutex

	runs      map[string]agent.Run
	steps     map[string][]agent.Step
	approvals []agent.Approval

	// errs is the error a method returns while it is set, by method name.
	errs map[string]error
	// changesErr, when set, is asked for the error the nth call to Changes
	// returns (n counts from 1); nil lets the call through. It runs with the
	// fake locked and may wait for ctx: a read the client's leaving cuts short.
	changesErr func(ctx context.Context, n int) error

	calls           []string
	getIDs          []string
	runFilters      []agent.RunFilter
	approvalFilters []agent.ApprovalFilter
	changes         []changesCall
	decisions       []decisionCall
	cancels         []cancelCall
}

type decisionCall struct{ Method, ID, By, Reason string }
type cancelCall struct{ RunID, By, Reason string }

func newFake() *fakeRuns {
	return &fakeRuns{
		runs:  map[string]agent.Run{},
		steps: map[string][]agent.Step{},
		errs:  map[string]error{},
	}
}

// edit runs fn with the fake locked, to change what it holds.
func (f *fakeRuns) edit(fn func(f *fakeRuns)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeRuns) addRun(r agent.Run) *fakeRuns {
	f.edit(func(f *fakeRuns) { f.runs[r.ID] = r })
	return f
}

func (f *fakeRuns) fail(method string, err error) *fakeRuns {
	f.edit(func(f *fakeRuns) { f.errs[method] = err })
	return f
}

func (f *fakeRuns) record(method string) error {
	f.calls = append(f.calls, method)
	return f.errs[method]
}

func (f *fakeRuns) countOf(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (f *fakeRuns) GetRun(ctx context.Context, id string) (agent.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getIDs = append(f.getIDs, id)
	if err := f.record("GetRun"); err != nil {
		return agent.Run{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.Run{}, err
	}
	run, ok := f.runs[id]
	if !ok {
		return agent.Run{}, agent.ErrNotFound
	}
	return run, nil
}

// ListRuns lists as a store does: newest first, an id breaking a tie in the
// other direction, from after the cursor, at most Limit (50 when zero, 200 at
// most).
func (f *fakeRuns) ListRuns(_ context.Context, filter agent.RunFilter) ([]agent.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runFilters = append(f.runFilters, filter)
	if err := f.record("ListRuns"); err != nil {
		return nil, err
	}

	var found []agent.Run
	for _, r := range f.runs {
		switch {
		case filter.Status != "" && r.Status != filter.Status:
		case filter.Agent != "" && r.Agent != filter.Agent:
		case filter.ParentID != "" && r.ParentID != filter.ParentID:
		case filter.Before != nil && !olderThan(r, *filter.Before):
		default:
			found = append(found, r)
		}
	}
	slices.SortFunc(found, func(a, b agent.Run) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(b.ID, a.ID))
	})
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	return found[:min(len(found), min(limit, 200))], nil
}

// olderThan is whether r comes after position c in a listing, which is newest
// first and then by id, descending.
func olderThan(r agent.Run, c agent.Cursor) bool {
	if order := r.CreatedAt.Compare(c.CreatedAt); order != 0 {
		return order < 0
	}
	return r.ID < c.ID
}

func (f *fakeRuns) Changes(ctx context.Context, runID string, since int64) (agent.Changes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.changes) + 1
	_, hasDeadline := ctx.Deadline()
	call := changesCall{RunID: runID, Since: since, HasDeadline: hasDeadline}
	defer func() { f.changes = append(f.changes, call) }()

	failed := func(err error) error {
		call.Err = err
		return err
	}
	f.calls = append(f.calls, "Changes")
	if err := f.errs["Changes"]; err != nil {
		return agent.Changes{}, failed(err)
	}
	if f.changesErr != nil {
		if err := f.changesErr(ctx, n); err != nil {
			return agent.Changes{}, failed(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return agent.Changes{}, failed(err)
	}
	run, ok := f.runs[runID]
	if !ok {
		return agent.Changes{}, failed(agent.ErrNotFound)
	}
	out := agent.Changes{Run: run}
	for _, st := range f.steps[runID] {
		if st.Rev > since {
			out.Steps = append(out.Steps, st)
		}
	}
	for _, a := range f.approvals {
		if a.RunID == runID && a.Rev > since {
			out.Approvals = append(out.Approvals, a)
		}
	}
	call.Rev = run.Rev
	return out, nil
}

func (f *fakeRuns) ListApprovals(_ context.Context, filter agent.ApprovalFilter) ([]agent.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvalFilters = append(f.approvalFilters, filter)
	if err := f.record("ListApprovals"); err != nil {
		return nil, err
	}
	var found []agent.Approval
	for _, a := range f.approvals {
		switch {
		case filter.Status != "" && a.Status != filter.Status:
		case filter.RunID != "" && a.RunID != filter.RunID:
		default:
			found = append(found, a)
		}
	}
	slices.SortFunc(found, func(a, b agent.Approval) int {
		return cmp.Or(a.RequestedAt.Compare(b.RequestedAt), cmp.Compare(a.ID, b.ID))
	})
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	return found[:min(len(found), min(limit, 200))], nil
}

func (f *fakeRuns) decide(method string, approved bool, id, by, reason string) (agent.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, decisionCall{method, id, by, reason})
	err := f.record(method)
	for _, a := range f.approvals {
		if a.ID != id {
			continue
		}
		if err != nil {
			// As a store does for an approval already answered: the answer
			// comes back with the error.
			return a, err
		}
		a.Status = agent.ApprovalDeclined
		if approved {
			a.Status = agent.ApprovalApproved
		}
		a.DecidedBy, a.Reason = by, reason
		decided := at(100)
		a.DecidedAt = &decided
		return a, nil
	}
	if err != nil {
		return agent.Approval{}, err
	}
	return agent.Approval{}, agent.ErrNotFound
}

func (f *fakeRuns) Approve(_ context.Context, id, by, reason string) (agent.Approval, error) {
	return f.decide("Approve", true, id, by, reason)
}

func (f *fakeRuns) Decline(_ context.Context, id, by, reason string) (agent.Approval, error) {
	return f.decide("Decline", false, id, by, reason)
}

func (f *fakeRuns) Cancel(_ context.Context, runID, by, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, cancelCall{runID, by, reason})
	if err := f.record("Cancel"); err != nil {
		return err
	}
	if _, ok := f.runs[runID]; !ok {
		return agent.ErrNotFound
	}
	return nil
}

// snapshot copies what the tests assert on, so a test reads it without
// holding the lock while a stream goes on calling.
func (f *fakeRuns) changesCalls() []changesCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.changes)
}

// actorFromHeader names the caller by the X-Actor header.
func actorFromHeader(r *http.Request) string { return r.Header.Get("X-Actor") }

// newAPI builds an API over runs and returns its routes.
func newAPI(t *testing.T, runs httpapi.Runs, mods ...func(*httpapi.Options)) chi.Router {
	t.Helper()
	opts := httpapi.Options{Runs: runs, Actor: actorFromHeader, Logger: discardLogger()}
	for _, mod := range mods {
		mod(&opts)
	}
	api, err := httpapi.New(opts)
	require.NoError(t, err)
	return api.Routes()
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// quick makes a stream poll and beat fast, so a test waits for a handful of
// milliseconds where the defaults would make it wait for seconds.
func quick(o *httpapi.Options) {
	o.PollInterval = 5 * time.Millisecond
	o.Heartbeat = 25 * time.Millisecond
}

// do sends one request to h without a server.
func do(t *testing.T, h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doCtx(t, context.Background(), h, method, target, body, headers...)
}

// doCtx is do with the request made under ctx.
func doCtx(t *testing.T, ctx context.Context, h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(ctx, method, target, rd)
	require.Zero(t, len(headers)%2, "headers come in pairs")
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), "body: %s", rec.Body.String())
	return v
}

// sseEvent is one thing a client reads off a stream: an event, or a comment.
type sseEvent struct {
	ID      string
	Type    string
	Data    string
	Comment bool
}

// parseSSE reads a stream the way a browser does, as far as these tests need.
// emit is called for each event and each comment, in order.
func parseSSE(r io.Reader, emit func(sseEvent)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var ev sseEvent
	var data []string
	var open bool
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if open {
				ev.Data = strings.Join(data, "\n")
				emit(ev)
			}
			ev, data, open = sseEvent{}, nil, false
		case strings.HasPrefix(line, ":"):
			emit(sseEvent{Comment: true, Data: strings.TrimSpace(line[1:])})
		default:
			name, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			open = true
			switch name {
			case "id":
				ev.ID = value
			case "event":
				ev.Type = value
			case "data":
				data = append(data, value)
			}
		}
	}
}

// eventsOf is every event and comment in a finished response body.
func eventsOf(body string) []sseEvent {
	var out []sseEvent
	parseSSE(strings.NewReader(body), func(ev sseEvent) { out = append(out, ev) })
	return out
}

// realEvents is eventsOf without the comments.
func realEvents(events []sseEvent) []sseEvent {
	return slices.DeleteFunc(slices.Clone(events), func(ev sseEvent) bool { return ev.Comment })
}

// tracker reports each time a request's handler returns.
type tracker struct {
	h      http.Handler
	exited chan struct{}
}

func newTracker(h http.Handler) *tracker {
	return &tracker{h: h, exited: make(chan struct{}, 256)}
}

func (tr *tracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() { tr.exited <- struct{}{} }()
	tr.h.ServeHTTP(w, r)
}

// waitExit waits for one handler to return.
func (tr *tracker) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-tr.exited:
	case <-time.After(waitFor):
		t.Fatal("the handler did not return")
	}
}

// serve starts a real server over h and stops it when the test ends.
func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

// liveStream is a client reading a stream from a real server.
type liveStream struct {
	t      *testing.T
	resp   *http.Response
	events chan sseEvent
	cancel context.CancelFunc
}

// openStream connects to path on srv. headers come in pairs.
func openStream(t *testing.T, srv *httptest.Server, path string, headers ...string) *liveStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)
	require.Zero(t, len(headers)%2, "headers come in pairs")
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)

	s := &liveStream{t: t, resp: resp, events: make(chan sseEvent, 4096), cancel: cancel}
	go func() {
		defer close(s.events)
		parseSSE(resp.Body, func(ev sseEvent) { s.events <- ev })
	}()
	t.Cleanup(func() {
		cancel()
		_ = resp.Body.Close()
	})
	return s
}

// next is the next event or comment, or false once the stream has ended.
func (s *liveStream) next() (sseEvent, bool) {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		return ev, ok
	case <-time.After(waitFor):
		s.t.Fatal("nothing arrived on the stream")
		return sseEvent{}, false
	}
}

// nextEvent is the next event, skipping comments.
func (s *liveStream) nextEvent() sseEvent {
	s.t.Helper()
	for {
		ev, ok := s.next()
		require.True(s.t, ok, "the stream ended")
		if !ev.Comment {
			return ev
		}
	}
}

// rest reads what is left until the stream ends, comments left out.
func (s *liveStream) rest() []sseEvent {
	s.t.Helper()
	var out []sseEvent
	for {
		ev, ok := s.next()
		if !ok {
			return out
		}
		if !ev.Comment {
			out = append(out, ev)
		}
	}
}

// disconnect closes the client's side of the connection.
func (s *liveStream) disconnect() {
	s.cancel()
	_ = s.resp.Body.Close()
}

// logSink keeps the records a logger was handed.
type logSink struct {
	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

func (s *logSink) logger() *slog.Logger { return slog.New(&sinkHandler{sink: s}) }

func (s *logSink) all() []logRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.records)
}

// atLeast is the records at level or above.
func (s *logSink) atLeast(level slog.Level) []logRecord {
	return slices.DeleteFunc(s.all(), func(r logRecord) bool { return r.Level < level })
}

type sinkHandler struct {
	sink  *logSink
	attrs []slog.Attr
}

func (h *sinkHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *sinkHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{Level: r.Level, Msg: r.Message, Attrs: map[string]string{}}
	for _, a := range h.attrs {
		rec.Attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.String()
		return true
	})
	h.sink.mu.Lock()
	h.sink.records = append(h.sink.records, rec)
	h.sink.mu.Unlock()
	return nil
}

func (h *sinkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &sinkHandler{sink: h.sink, attrs: append(slices.Clone(h.attrs), attrs...)}
}

func (h *sinkHandler) WithGroup(string) slog.Handler { return h }
