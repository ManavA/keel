package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/httpapi"
	"github.com/ManavA/keel/httpx"
)

const eventsPath = "/runs/%s/events"

// stream1 is the path of the stream of run 1.
var stream1 = fmt.Sprintf(eventsPath, uid(1))

// runHandler serves req to h on w and fails the test if the handler does not
// return. The request's context ends when it gives up, so the handler goes.
func runHandler(t *testing.T, h http.Handler, w http.ResponseWriter, req *http.Request) {
	t.Helper()
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, req.WithContext(ctx))
	}()
	select {
	case <-done:
	case <-time.After(waitFor):
		cancel()
		<-done
		t.Fatal("the handler did not return")
	}
}

// streamOf serves a GET of the stream of run 1 to a recorder, for a run whose
// stream ends by itself.
func streamOf(t *testing.T, f *fakeRuns, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, stream1, nil)
	require.Zero(t, len(headers)%2)
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	runHandler(t, newAPI(t, f, quick), rec, req)
	return rec
}

// endedRun is run 1, completed at revision 6, with the changes a stream is
// built from. Steps come in journal order, and not in the order they changed.
//
//	rev 3  approval A
//	rev 4  step 2
//	rev 5  step 3, approval B
//	rev 6  step 1
func endedRun(status agent.Status) *fakeRuns {
	f := newFake().addRun(mkRun(uid(1), withStatus(status), withRev(6)))
	f.edit(func(f *fakeRuns) {
		f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 6), mkStep(uid(1), 2, 4), mkStep(uid(1), 3, 5)}
		f.approvals = []agent.Approval{mkApproval(uid(51), uid(1), 2, 3), mkApproval(uid(52), uid(1), 3, 5)}
	})
	return f
}

// label names an event as a test reads it: the type, and what it is about.
func label(t *testing.T, ev sseEvent) string {
	t.Helper()
	switch ev.Type {
	case "step":
		var st agent.Step
		require.NoError(t, json.Unmarshal([]byte(ev.Data), &st))
		return fmt.Sprintf("step %d", st.Seq)
	case "approval":
		var a agent.Approval
		require.NoError(t, json.Unmarshal([]byte(ev.Data), &a))
		return "approval " + a.ID[len(a.ID)-2:]
	case "run":
		var r agent.Run
		require.NoError(t, json.Unmarshal([]byte(ev.Data), &r))
		return fmt.Sprintf("run rev %d", r.Rev)
	}
	return ev.Type
}

func labels(t *testing.T, events []sseEvent) []string {
	t.Helper()
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = label(t, ev)
	}
	return out
}

func TestStream_UnknownRunIs404BeforeAnyEvent(t *testing.T) {
	f := newFake()
	rec := do(t, newAPI(t, f), http.MethodGet, fmt.Sprintf(eventsPath, uid(9)), "")
	requireError(t, rec, http.StatusNotFound)
	assert.NotContains(t, rec.Header().Get("Content-Type"), "event-stream")
	assert.NotContains(t, rec.Body.String(), "event:")
}

func TestStream_RunIdsAreTheStoresToJudge(t *testing.T) {
	for _, id := range []string{"abc", strings.ToUpper(letterID)} {
		f := newFake().addRun(mkRun(letterID))
		rec := do(t, newAPI(t, f), http.MethodGet, fmt.Sprintf(eventsPath, id), "")
		requireError(t, rec, http.StatusNotFound)
		calls := f.changesCalls()
		require.Len(t, calls, 1)
		assert.Equal(t, id, calls[0].RunID, "the store is given the name as it was written")
	}
}

func TestStream_AFailureBeforeTheStreamOpensIsAnOrdinaryError(t *testing.T) {
	f := newFake().addRun(mkRun(uid(1))).fail("Changes", errStoreDown)
	rec := do(t, newAPI(t, f), http.MethodGet, stream1, "")
	requireError(t, rec, http.StatusInternalServerError)
	requireNoLeak(t, rec)
	assert.NotContains(t, rec.Header().Get("Content-Type"), "event-stream")
}

// Steps and approvals, then the run, which alone carries an id and carries
// the revision, so that a client reconnecting with it has everything up to it.
func TestStream_SendsStepsAndApprovalsInRevOrderThenTheRun(t *testing.T) {
	rec := streamOf(t, endedRun(agent.StatusCompleted))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))

	events := realEvents(eventsOf(rec.Body.String()))
	assert.Equal(t, []string{
		"approval 51", // rev 3
		"step 2",      // rev 4
		"step 3",      // rev 5: a step before an approval of the same revision
		"approval 52", // rev 5
		"step 1",      // rev 6, though first in the journal
		"run rev 6",
		"end",
	}, labels(t, events))

	for _, ev := range events {
		if ev.Type == "run" {
			assert.Equal(t, "6", ev.ID, "the run event's id is the run's revision")
		} else {
			assert.Empty(t, ev.ID, "only the run event has an id: %s", ev.Type)
		}
	}
}

func TestStream_FinishedRunsSendEndAndTheHandlerReturns(t *testing.T) {
	for _, status := range []agent.Status{agent.StatusCompleted, agent.StatusFailed, agent.StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			rec := streamOf(t, endedRun(status)) // returns, or runHandler fails
			events := realEvents(eventsOf(rec.Body.String()))
			require.NotEmpty(t, events)
			last := events[len(events)-1]
			assert.Equal(t, "end", last.Type)
			assert.Empty(t, last.ID)
			assert.JSONEq(t, `{}`, last.Data, "an event with no data is not dispatched by a client")
			assert.Equal(t, 1, strings.Count(rec.Body.String(), "event: end"))
			assert.Equal(t, "run", events[len(events)-2].Type, "the end follows the run event")
		})
	}
}

func TestStream_NeverServesOpaque(t *testing.T) {
	f := endedRun(agent.StatusCompleted)
	rec := streamOf(t, f)
	body := rec.Body.String()
	assert.NotContains(t, body, secretThought)
	assert.NotContains(t, body, "opaque")
	assert.Contains(t, body, "reply 1", "the rest of the step is served")

	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotNil(t, f.steps[uid(1)][0].Message.Opaque, "what the store returned is not changed")
}

func TestStream_ResumesAfterLastEventID(t *testing.T) {
	everything := []string{"approval 51", "step 2", "step 3", "approval 52", "step 1", "run rev 6", "end"}
	tests := []struct {
		name       string
		header     []string
		wantSince  []int64 // each call made to Changes
		wantEvents []string
	}{
		{"no header starts from the beginning", nil, []int64{0}, everything},
		{"an empty header is none", []string{"Last-Event-ID", ""}, []int64{0}, everything},
		{"zero is the beginning", []string{"Last-Event-ID", "0"}, []int64{0}, everything},
		{"resumes after the revision", []string{"Last-Event-ID", "4"}, []int64{4},
			[]string{"step 3", "approval 52", "step 1", "run rev 6", "end"}},
		{"resumes after the last change but one", []string{"Last-Event-ID", "5"}, []int64{5},
			[]string{"step 1", "run rev 6", "end"}},
		{"at the current revision there is nothing left but the end", []string{"Last-Event-ID", "6"}, []int64{6},
			[]string{"end"}},
		// What a stream carries is state, and all of it is always safe to
		// send again; a position that makes no sense is the beginning, never a
		// reason to refuse a client that cannot do anything about its header.
		{"not a number", []string{"Last-Event-ID", "abc"}, []int64{0}, everything},
		{"a number and more", []string{"Last-Event-ID", "4abc"}, []int64{0}, everything},
		{"a fraction", []string{"Last-Event-ID", "4.5"}, []int64{0}, everything},
		{"negative", []string{"Last-Event-ID", "-3"}, []int64{0}, everything},
		{"too large for a revision", []string{"Last-Event-ID", "99999999999999999999"}, []int64{0}, everything},
		// A revision the run has not reached belongs to some other history, or
		// to a store that was restored: waiting for the run to catch up would
		// skip every change before then.
		{"ahead of the run", []string{"Last-Event-ID", "7"}, []int64{7, 0}, everything},
		{"far ahead of the run", []string{"Last-Event-ID", "9223372036854775807"}, []int64{9223372036854775807, 0}, everything},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := endedRun(agent.StatusCompleted)
			rec := streamOf(t, f, tt.header...)
			require.Equal(t, http.StatusOK, rec.Code)

			var since []int64
			for _, c := range f.changesCalls() {
				since = append(since, c.Since)
			}
			assert.Equal(t, tt.wantSince, since)
			assert.Equal(t, tt.wantEvents, labels(t, realEvents(eventsOf(rec.Body.String()))))
		})
	}
}

// A client at the run's current revision has everything: it is sent nothing
// until something changes.
func TestStream_AReconnectAtTheCurrentRevisionWaitsForAChange(t *testing.T) {
	f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(3)))
	f.edit(func(f *fakeRuns) { f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 3)} })
	srv := serve(t, newAPI(t, f, quick))

	s := openStream(t, srv, stream1, "Last-Event-ID", "3")
	// Comments prove the stream is open and polling and that nothing else came.
	for range 2 {
		ev, ok := s.next()
		require.True(t, ok)
		require.True(t, ev.Comment, "got %+v", ev)
	}

	f.edit(func(f *fakeRuns) {
		f.steps[uid(1)] = append(f.steps[uid(1)], mkStep(uid(1), 2, 4))
		run := f.runs[uid(1)]
		run.Rev = 4
		f.runs[uid(1)] = run
	})
	assert.Equal(t, "step 2", label(t, s.nextEvent()))
	assert.Equal(t, "run rev 4", label(t, s.nextEvent()))
}

// The run is followed from one revision to the next. Asking for what came
// after the last revision sent is how no change can be skipped: the next
// answer holds everything after it.
func TestStream_FollowsARunUntilItEnds(t *testing.T) {
	f := newFake().addRun(mkRun(uid(1), withRev(1)))
	f.edit(func(f *fakeRuns) { f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 1)} })
	tr := newTracker(newAPI(t, f, quick))
	srv := serve(t, tr)

	s := openStream(t, srv, stream1)
	assert.Equal(t, http.StatusOK, s.resp.StatusCode)
	assert.Equal(t, "text/event-stream", s.resp.Header.Get("Content-Type"))
	assert.Equal(t, "step 1", label(t, s.nextEvent()))
	first := s.nextEvent()
	assert.Equal(t, "run rev 1", label(t, first))
	assert.Equal(t, "1", first.ID)

	// A step is added.
	f.edit(func(f *fakeRuns) {
		f.steps[uid(1)] = append(f.steps[uid(1)], mkStep(uid(1), 2, 2))
		run := f.runs[uid(1)]
		run.Rev = 2
		f.runs[uid(1)] = run
	})
	assert.Equal(t, "step 2", label(t, s.nextEvent()))
	assert.Equal(t, "run rev 2", label(t, s.nextEvent()))

	// Only the run changes: a request to cancel, which touches no step.
	f.edit(func(f *fakeRuns) {
		run := f.runs[uid(1)]
		run.Rev, run.CancelRequested = 3, true
		f.runs[uid(1)] = run
	})
	ev := s.nextEvent()
	assert.Equal(t, "run rev 3", label(t, ev))
	assert.Equal(t, "3", ev.ID)

	// An approval is asked and a step waits, in one revision.
	f.edit(func(f *fakeRuns) {
		f.steps[uid(1)][1].Status, f.steps[uid(1)][1].Rev = agent.StepWaiting, 4
		f.approvals = append(f.approvals, mkApproval(uid(51), uid(1), 2, 4))
		run := f.runs[uid(1)]
		run.Rev, run.Status = 4, agent.StatusWaiting
		f.runs[uid(1)] = run
	})
	assert.Equal(t, []string{"step 2", "approval 51", "run rev 4"},
		[]string{label(t, s.nextEvent()), label(t, s.nextEvent()), label(t, s.nextEvent())})

	// The run ends.
	f.edit(func(f *fakeRuns) {
		run := f.runs[uid(1)]
		run.Rev, run.Status = 5, agent.StatusCompleted
		f.runs[uid(1)] = run
	})
	assert.Equal(t, []string{"run rev 5", "end"}, labels(t, s.rest()))
	tr.waitExit(t)

	calls := f.changesCalls()
	require.Greater(t, len(calls), 4)
	assert.Equal(t, int64(0), calls[0].Since)
	for i := 1; i < len(calls); i++ {
		require.NoError(t, calls[i-1].Err)
		assert.Equal(t, calls[i-1].Rev, calls[i].Since,
			"call %d asks for what came after the revision the last answer sent", i)
		assert.False(t, calls[i].HasDeadline, "the stream is not given a timeout of its own")
	}
	assert.False(t, calls[0].HasDeadline)
}

// A writer changes the run as fast as it can while a stream polls. A stream
// carries state, so intermediate states may be folded together, but it never
// misses the last state of anything: when it ends, each step has been seen in
// the form it ended in.
func TestStream_NeverSkipsAChange(t *testing.T) {
	const steps, updates = 5, 300

	f := newFake().addRun(mkRun(uid(1), withRev(1)))
	f.edit(func(f *fakeRuns) {
		for seq := 1; seq <= steps; seq++ {
			st := mkStep(uid(1), seq, 1)
			st.Result = "v1"
			f.steps[uid(1)] = append(f.steps[uid(1)], st)
		}
	})
	tr := newTracker(newAPI(t, f, func(o *httpapi.Options) {
		o.PollInterval = time.Millisecond
		o.Heartbeat = time.Hour
	}))
	srv := serve(t, tr)
	s := openStream(t, srv, stream1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= updates; i++ {
			rev := int64(i + 1)
			f.edit(func(f *fakeRuns) {
				seq := i%steps + 1
				f.steps[uid(1)][seq-1].Rev = rev
				f.steps[uid(1)][seq-1].Result = "v" + strconv.FormatInt(rev, 10)
				run := f.runs[uid(1)]
				run.Rev = rev
				f.runs[uid(1)] = run
			})
			if i%7 == 0 {
				time.Sleep(time.Millisecond) // let the stream in between
			}
		}
		f.edit(func(f *fakeRuns) {
			run := f.runs[uid(1)]
			run.Rev, run.Status = updates+2, agent.StatusCompleted
			f.runs[uid(1)] = run
		})
	}()

	seen := map[int]agent.Step{}
	var runIDs []int64
	var sawEnd bool
	for _, ev := range s.rest() {
		switch ev.Type {
		case "step":
			var st agent.Step
			require.NoError(t, json.Unmarshal([]byte(ev.Data), &st))
			seen[st.Seq] = st
		case "run":
			id, err := strconv.ParseInt(ev.ID, 10, 64)
			require.NoError(t, err)
			runIDs = append(runIDs, id)
		case "end":
			sawEnd = true
		}
	}
	wg.Wait()
	tr.waitExit(t)

	require.True(t, sawEnd)
	require.Len(t, seen, steps)
	f.mu.Lock()
	defer f.mu.Unlock()
	for seq, st := range seen {
		final := f.steps[uid(1)][seq-1]
		assert.Equal(t, final.Rev, st.Rev, "step %d was last seen at revision %d, and ended at %d", seq, st.Rev, final.Rev)
		assert.Equal(t, final.Result, st.Result, "step %d", seq)
	}
	require.NotEmpty(t, runIDs)
	for i := 1; i < len(runIDs); i++ {
		assert.Greater(t, runIDs[i], runIDs[i-1], "run revisions only rise")
	}
	assert.Equal(t, int64(updates+2), runIDs[len(runIDs)-1])
	for i := 1; i < len(f.changes); i++ {
		assert.Equal(t, f.changes[i-1].Rev, f.changes[i].Since,
			"each read asks for what came after the revision the last one sent")
	}
}

// An idle stream keeps itself open with a comment line, and goes on reading
// the journal while it does.
func TestStream_SendsACommentWhenNothingChanges(t *testing.T) {
	f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(2)))
	f.edit(func(f *fakeRuns) { f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 2)} })
	srv := serve(t, newAPI(t, f, quick))

	s := openStream(t, srv, stream1)
	assert.Equal(t, "step 1", label(t, s.nextEvent()))
	assert.Equal(t, "run rev 2", label(t, s.nextEvent()))

	for range 3 {
		ev, ok := s.next()
		require.True(t, ok)
		assert.True(t, ev.Comment, "got %+v", ev)
	}
	assert.Greater(t, len(f.changesCalls()), 3, "the journal is read between comments")
}

// Reads are spaced by PollInterval: with the interval long and the heartbeat
// short, the stream stays alive for several beats and reads only once.
func TestStream_ReadsOnlyOncePerPollInterval(t *testing.T) {
	f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(2)))
	srv := serve(t, newAPI(t, f, func(o *httpapi.Options) {
		o.PollInterval = time.Hour
		o.Heartbeat = 10 * time.Millisecond
	}))

	s := openStream(t, srv, stream1)
	assert.Equal(t, "run rev 2", label(t, s.nextEvent()))
	for range 3 {
		ev, ok := s.next()
		require.True(t, ok)
		require.True(t, ev.Comment)
	}
	assert.Len(t, f.changesCalls(), 1)
}

func TestStream_ReturnsWhenTheClientGoesAway(t *testing.T) {
	var sink logSink
	f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(2)))
	tr := newTracker(newAPI(t, f, quick, func(o *httpapi.Options) { o.Logger = sink.logger() }))
	srv := serve(t, tr)

	s := openStream(t, srv, stream1)
	assert.Equal(t, "run rev 2", label(t, s.nextEvent()))
	ev, ok := s.next() // the stream is idle and polling
	require.True(t, ok)
	require.True(t, ev.Comment)

	s.disconnect()
	tr.waitExit(t)

	// The handler is gone and so is anything it started: nothing reads the
	// journal for a client that has left.
	before := len(f.changesCalls())
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, f.changesCalls(), before)
	assert.Empty(t, sink.atLeast(slog.LevelWarn), "a client leaving is not a fault")
}

// A read the client's leaving cuts short fails with the request's own error,
// and that is not the store's fault.
func TestStream_AReadCutShortByTheClientLeavingIsNotAFault(t *testing.T) {
	var sink logSink
	f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(2)))
	reading := make(chan struct{})
	f.edit(func(f *fakeRuns) {
		f.changesErr = func(ctx context.Context, n int) error {
			if n < 2 {
				return nil
			}
			// The second read is under way and waits for the client to leave.
			close(reading)
			<-ctx.Done()
			return ctx.Err()
		}
	})
	tr := newTracker(newAPI(t, f, quick, func(o *httpapi.Options) { o.Logger = sink.logger() }))
	srv := serve(t, tr)

	s := openStream(t, srv, stream1)
	assert.Equal(t, "run rev 2", label(t, s.nextEvent()))
	select {
	case <-reading:
	case <-time.After(waitFor):
		t.Fatal("the stream never read again")
	}
	s.disconnect()
	tr.waitExit(t)

	assert.Empty(t, sink.atLeast(slog.LevelWarn), "a client leaving is not a fault")
}

// A store that fails in the middle of a stream cannot be told to the client,
// whose status is long since sent. The stream ends without an end event, which
// is how a client knows to reconnect from the revision it has, and the cause
// is logged.
func TestStream_AStoreErrorInTheMiddleEndsTheStream(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"a store error", errStoreDown},
		{"the run is gone", fmt.Errorf("changes: %w", agent.ErrNotFound)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var sink logSink
			f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(2)))
			f.edit(func(f *fakeRuns) {
				f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 2)}
				f.changesErr = func(_ context.Context, n int) error {
					if n >= 3 {
						return tt.err
					}
					return nil
				}
			})
			tr := newTracker(newAPI(t, f, quick, func(o *httpapi.Options) { o.Logger = sink.logger() }))
			srv := serve(t, tr)

			s := openStream(t, srv, stream1)
			assert.Equal(t, http.StatusOK, s.resp.StatusCode)
			assert.Equal(t, []string{"step 1", "run rev 2"}, labels(t, s.rest()), "what was sent stands, and no end follows")
			tr.waitExit(t)

			calls := f.changesCalls()
			require.Len(t, calls, 3, "the store is not asked again")
			time.Sleep(30 * time.Millisecond)
			assert.Len(t, f.changesCalls(), 3)

			logged := sink.atLeast(slog.LevelError)
			require.Len(t, logged, 1)
			assert.Contains(t, logged[0].Attrs["error"], tt.err.Error())
			assert.Equal(t, uid(1), logged[0].Attrs["run_id"])
		})
	}
}

// failingWriter is a response a client reads until it breaks: after failAfter
// writes, every write fails.
type failingWriter struct {
	*httptest.ResponseRecorder
	failAfter int
	writes    int
}

func (w *failingWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, errors.New("broken pipe")
	}
	return w.ResponseRecorder.Write(b)
}

// After a send fails the stream is unusable, and the client may be gone
// without the request's context having ended: the handler returns at once and
// does not go on reading the journal.
func TestStream_ASendThatFailsEndsTheHandler(t *testing.T) {
	for _, tt := range []struct {
		name      string
		failAfter int
		status    agent.Status
		withStep  bool
		onlyRead  bool // the stream had no reason to read twice
	}{
		{"the first event", 0, agent.StatusWaiting, true, true},
		{"the run event", 1, agent.StatusWaiting, true, true},
		{"a comment", 1, agent.StatusWaiting, false, false}, // the run event, then the first comment
		{"the end event", 2, agent.StatusCompleted, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake().addRun(mkRun(uid(1), withStatus(tt.status), withRev(2)))
			if tt.withStep {
				f.edit(func(f *fakeRuns) { f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 2)} })
			}
			w := &failingWriter{ResponseRecorder: httptest.NewRecorder(), failAfter: tt.failAfter}
			req := httptest.NewRequest(http.MethodGet, stream1, nil) // a context that never ends

			runHandler(t, newAPI(t, f, quick), w, req)

			before := len(f.changesCalls())
			require.NotZero(t, before, "the stream began")
			if tt.onlyRead {
				assert.Equal(t, 1, before, "a send that fails ends the handler before the next read")
			}
			time.Sleep(30 * time.Millisecond)
			assert.Len(t, f.changesCalls(), before, "nothing reads the journal after the handler has returned")
			assert.Equal(t, tt.failAfter+1, w.writes, "nothing is written after a write fails")
		})
	}
}

// noFlush is a response that cannot be streamed to.
type noFlush struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *noFlush) Header() http.Header { return w.header }
func (w *noFlush) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *noFlush) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}

func TestStream_AWriterThatCannotFlushGetsAnOrdinaryError(t *testing.T) {
	var sink logSink
	f := endedRun(agent.StatusCompleted)
	w := &noFlush{header: http.Header{}}
	req := httptest.NewRequest(http.MethodGet, stream1, nil)
	runHandler(t, newAPI(t, f, func(o *httpapi.Options) { o.Logger = sink.logger() }), w, req)

	assert.Equal(t, http.StatusInternalServerError, w.status)
	assert.Contains(t, w.header.Get("Content-Type"), "application/json")
	assert.NotContains(t, w.header.Get("Content-Type"), "event-stream")
	assert.NotContains(t, w.body.String(), "event:", "nothing was streamed")
	assert.NotContains(t, w.body.String(), "data:")
	var body httpx.ErrorBody
	require.NoError(t, json.Unmarshal([]byte(w.body.String()), &body))
	assert.Equal(t, "internal server error", body.Error)

	logged := sink.atLeast(slog.LevelError)
	require.Len(t, logged, 1)
	assert.Contains(t, logged[0].Attrs["error"], httpx.ErrStreamUnsupported.Error())
}

// flushFails is a response whose flush reports the client gone.
type flushFails struct{ *httptest.ResponseRecorder }

func (flushFails) FlushError() error { return errors.New("connection reset") }

// A stream that cannot even be opened has lost its client: the status may be
// sent already, so the handler writes nothing more and returns.
func TestStream_AClientGoneAtTheOpeningEndsTheHandlerQuietly(t *testing.T) {
	var sink logSink
	f := endedRun(agent.StatusCompleted)
	w := flushFails{httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, stream1, nil)
	runHandler(t, newAPI(t, f, func(o *httpapi.Options) { o.Logger = sink.logger() }), w, req)

	assert.Empty(t, w.Body.String(), "no error body after a stream was begun, and no event")
	assert.Empty(t, sink.atLeast(slog.LevelWarn))
	assert.Len(t, f.changesCalls(), 1, "the journal is not read for a client that is gone")
}

// An event that cannot be encoded, a step whose stored arguments are not JSON,
// fails the stream for a reason no client can mend: it reconnects to the same
// step and fails again. Nothing but the log shows it, so it is logged as a
// fault, which a client that leaves is not.
func TestStream_AnEventThatCannotBeEncodedIsAFaultAndIsLogged(t *testing.T) {
	notJSON := json.RawMessage("this is not JSON")
	for _, tt := range []struct {
		name  string
		build func(f *fakeRuns)
	}{
		{"a step", func(f *fakeRuns) {
			st := mkStep(uid(1), 1, 2)
			st.Call = &agent.Call{ID: "c1", Name: "send", Input: notJSON}
			f.steps[uid(1)] = []agent.Step{st}
		}},
		{"an approval", func(f *fakeRuns) {
			a := mkApproval(uid(51), uid(1), 1, 2)
			a.Input = notJSON
			f.approvals = []agent.Approval{a}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var sink logSink
			f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(2)))
			f.edit(tt.build)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, stream1, nil) // a context that never ends
			h := newAPI(t, f, quick, func(o *httpapi.Options) { o.Logger = sink.logger() })

			runHandler(t, h, rec, req) // the handler returns, or the test fails

			assert.Empty(t, realEvents(eventsOf(rec.Body.String())), "nothing was sent, and no end")
			logged := sink.atLeast(slog.LevelError)
			require.Len(t, logged, 1)
			assert.Equal(t, uid(1), logged[0].Attrs["run_id"])
			assert.Contains(t, logged[0].Attrs["error"], "encode")
			assert.Len(t, f.changesCalls(), 1, "the journal is not read again for a stream that has ended")
		})
	}
}

// A client that is gone while a send is under way is the stream's own failure,
// and is not logged as one.
func TestStream_ABrokenConnectionIsNotLoggedAsAFault(t *testing.T) {
	var sink logSink
	f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(2)))
	f.edit(func(f *fakeRuns) { f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 2)} })
	w := &failingWriter{ResponseRecorder: httptest.NewRecorder(), failAfter: 0}
	req := httptest.NewRequest(http.MethodGet, stream1, nil)
	runHandler(t, newAPI(t, f, quick, func(o *httpapi.Options) { o.Logger = sink.logger() }), w, req)
	assert.Empty(t, sink.atLeast(slog.LevelWarn))
}

// A HEAD says what a GET would, and what a GET of a stream says is httpx's to
// decide: the two are compared, so that a header added there is not forgotten
// here.
func TestStream_AHeadRequestSendsTheHeadersAStreamDoes(t *testing.T) {
	opened := httptest.NewRecorder()
	_, err := httpx.NewEventStream(opened, httptest.NewRequest(http.MethodGet, "/", nil), httpx.EventStreamOptions{})
	require.NoError(t, err)
	want := opened.Header().Clone()
	require.NotEmpty(t, want)

	f := endedRun(agent.StatusCompleted)
	h := newAPI(t, f, quick)
	head := do(t, h, http.MethodHead, stream1, "")
	require.Equal(t, http.StatusOK, head.Code)
	assert.Equal(t, want, head.Header(), "a HEAD carries the headers httpx.NewEventStream sets, and no others")

	get := do(t, h, http.MethodGet, stream1, "")
	assert.Equal(t, want, get.Header(), "and so does the stream itself")
}

// A HEAD asks what a GET would answer and wants no body. A stream has no end,
// so one started for it would only wait for the client to leave.
func TestStream_AHeadRequestDoesNotStartAStream(t *testing.T) {
	t.Run("answers for a run that exists", func(t *testing.T) {
		f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting)))
		req := httptest.NewRequest(http.MethodHead, stream1, nil)
		rec := httptest.NewRecorder()
		runHandler(t, newAPI(t, f, quick), rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
		assert.Empty(t, rec.Body.String())
		assert.Zero(t, f.countOf("Changes"), "no stream, so no journal read")
		assert.Equal(t, 1, f.countOf("GetRun"), "the run is looked up, as for a GET")
	})
	t.Run("an unknown run is 404", func(t *testing.T) {
		f := newFake()
		req := httptest.NewRequest(http.MethodHead, fmt.Sprintf(eventsPath, uid(9)), nil)
		rec := httptest.NewRecorder()
		runHandler(t, newAPI(t, f), rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, 1, f.countOf("GetRun"))
		assert.Zero(t, f.countOf("Changes"))
	})
	t.Run("a store error is 500", func(t *testing.T) {
		f := newFake().addRun(mkRun(uid(1))).fail("GetRun", errStoreDown)
		req := httptest.NewRequest(http.MethodHead, stream1, nil)
		rec := httptest.NewRecorder()
		runHandler(t, newAPI(t, f), rec, req)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
	t.Run("through the router, which sends a HEAD to the GET route", func(t *testing.T) {
		f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting)))
		api, err := httpapi.New(httpapi.Options{Runs: f, Logger: discardLogger()})
		require.NoError(t, err)
		router, err := httpx.NewRouter(httpx.RouterOptions{Timeout: -1, Logger: discardLogger()})
		require.NoError(t, err)
		router.Mount("/", api.Routes())
		srv := serve(t, router)

		ctx, cancel := context.WithTimeout(t.Context(), waitFor)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, srv.URL+stream1, nil)
		require.NoError(t, err)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err, "a stream started for a HEAD would not answer")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Zero(t, f.countOf("Changes"))
	})
}

// Behind the router's own middleware, whose wrappers sit between the handler
// and the connection, a stream still reaches the client event by event.
func TestStream_ThroughTheRouter(t *testing.T) {
	f := newFake().addRun(mkRun(uid(1), withRev(1)))
	f.edit(func(f *fakeRuns) { f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 1)} })
	api, err := httpapi.New(httpapi.Options{
		Runs: f, Logger: discardLogger(),
		PollInterval: 5 * time.Millisecond, Heartbeat: 25 * time.Millisecond,
	})
	require.NoError(t, err)
	router, err := httpx.NewRouter(httpx.RouterOptions{Timeout: -1, Logger: discardLogger()})
	require.NoError(t, err)
	router.Mount("/", api.Routes())
	tr := newTracker(router)
	srv := serve(t, tr)

	s := openStream(t, srv, stream1)
	assert.Equal(t, "step 1", label(t, s.nextEvent()))
	assert.Equal(t, "run rev 1", label(t, s.nextEvent()))

	f.edit(func(f *fakeRuns) {
		run := f.runs[uid(1)]
		run.Rev, run.Status = 2, agent.StatusCompleted
		f.runs[uid(1)] = run
	})
	assert.Equal(t, []string{"run rev 2", "end"}, labels(t, s.rest()))
	tr.waitExit(t)
}
