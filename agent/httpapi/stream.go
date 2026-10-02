package httpapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/httpx"
)

// The events of a stream. Only the run event has an id.
const (
	eventStep     = "step"
	eventApproval = "approval"
	eventRun      = "run"
	eventEnd      = "end"
)

// events serves GET /runs/{id}/events: the journal of one run as a server-sent
// event stream, from the revision a reconnecting client says it has.
//
// The stream has no timeout of its own. Each send is bounded by the writer,
// and the stream ends when the request's context does.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if r.Method == http.MethodHead {
		a.head(w, r, id)
		return
	}
	ctx := r.Context()

	// The first read is the lookup. An unknown run is a 404, and a store that
	// fails is a 500, while the response is still free to be either.
	changes, since, err := a.changesSince(ctx, id, resumeFrom(r))
	if err != nil {
		fail(w, r, "read changes", err)
		return
	}

	stream, err := httpx.NewEventStream(w, r, httpx.EventStreamOptions{})
	if err != nil {
		if errors.Is(err, httpx.ErrStreamUnsupported) {
			httpx.InternalError(w, r, fmt.Errorf("agent/httpapi: event stream: %w", err))
			return
		}
		// Anything else is the client gone, and the status may be out already.
		a.logger(ctx).LogAttrs(ctx, slog.LevelDebug, "event stream: not opened",
			slog.String("run_id", id), slog.Any("error", err))
		return
	}
	a.follow(ctx, stream, id, changes, since)
}

// head answers a HEAD for the stream as a GET would, but opens none: a stream
// has no end, and one started for a HEAD would wait for the client to leave.
func (a *API) head(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := a.runs.GetRun(r.Context(), id); err != nil {
		fail(w, r, "get run", err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// resumeFrom is the revision a reconnecting client has, from Last-Event-ID. A
// stream carries state and not history, so everything sent again is harmless
// and a position that makes no sense is the beginning. Refusing it would only
// strand a client that cannot change its header.
func resumeFrom(r *http.Request) int64 {
	rev, err := strconv.ParseInt(httpx.LastEventID(r), 10, 64)
	if err != nil || rev < 0 {
		return 0
	}
	return rev
}

// changesSince reads what changed after revision since, and returns the
// revision that read is from. A revision the run has not reached is no
// position in its history: waiting for the run to get there would skip every
// change in between, so the read is made again from the beginning.
func (a *API) changesSince(ctx context.Context, id string, since int64) (agent.Changes, int64, error) {
	changes, err := a.runs.Changes(ctx, id, since)
	if err == nil && since > changes.Run.Rev {
		since = 0
		changes, err = a.runs.Changes(ctx, id, 0)
	}
	return changes, since, err
}

// follow is the stream's loop. changes is a read made after revision since.
//
// Each pass sends what that read holds, then waits PollInterval, sending a
// comment whenever Heartbeat passes with nothing sent, and reads again from the
// revision it last sent. A change cannot be missed that way: whatever happens
// after the revision a read ended at is in the next read, and a read that finds
// nothing leaves the revision where it was.
//
// It returns when the run has ended and the end is sent, when the request's
// context ends, when a send fails (the stream is unusable then, and the
// client is gone), or when the store fails (the client reconnects from the
// revision it has).
func (a *API) follow(ctx context.Context, s *httpx.EventStream, id string, changes agent.Changes, since int64) {
	log := a.logger(ctx)
	sendFailed := func(err error) {
		log.LogAttrs(ctx, slog.LevelDebug, "event stream: send failed",
			slog.String("run_id", id), slog.Any("error", err))
	}

	poll := time.NewTimer(a.poll)
	defer poll.Stop()
	beat := time.NewTimer(a.heartbeat)
	defer beat.Stop()

	for {
		next, sent, err := deliver(s, changes, since)
		if err != nil {
			sendFailed(err)
			return
		}
		since = next
		if sent {
			beat.Reset(a.heartbeat)
		}
		if changes.Run.Terminal() {
			if err := s.Send(httpx.ServerEvent{Type: eventEnd, Data: []byte("{}")}); err != nil {
				sendFailed(err)
			}
			return
		}

		poll.Reset(a.poll)
		for waiting := true; waiting; {
			select {
			case <-ctx.Done():
				return
			case <-beat.C:
				if err := s.Comment("heartbeat"); err != nil {
					sendFailed(err)
					return
				}
				beat.Reset(a.heartbeat)
			case <-poll.C:
				waiting = false
			}
		}

		changes, since, err = a.changesSince(ctx, id, since)
		if err != nil {
			// A client that left cancelled the read: not a fault.
			if ctx.Err() == nil {
				log.LogAttrs(ctx, slog.LevelError, "event stream: read changes failed",
					slog.String("run_id", id), slog.Any("error", err))
			}
			return
		}
	}
}

// change is one thing an event reports.
type change struct {
	rev   int64
	event string
	value any
}

// deliver sends what changes holds that a client at revision since does not
// have, and returns the revision the client has after it, and whether anything
// was sent. It sends steps and approvals in Rev order, a step before an
// approval of the same revision, with no id, and then the run, whose id is its
// revision: a client that reconnects with that id has all that came before it,
// and one that reconnects in the middle of a batch is sent the batch again,
// which is harmless since each event is the current state of one thing.
//
// A read that finds nothing after since sends nothing, not even the run.
func deliver(s *httpx.EventStream, changes agent.Changes, since int64) (rev int64, sent bool, err error) {
	var batch []change
	for _, st := range withoutOpaque(changes.Steps) {
		batch = append(batch, change{st.Rev, eventStep, st})
	}
	for _, ap := range changes.Approvals {
		batch = append(batch, change{ap.Rev, eventApproval, ap})
	}
	if len(batch) == 0 && changes.Run.Rev <= since {
		return since, false, nil
	}

	// The store hands steps over in journal order and approvals in the order
	// they were asked, and neither is the order they changed in.
	slices.SortStableFunc(batch, func(a, b change) int { return cmp.Compare(a.rev, b.rev) })
	for _, c := range batch {
		if err := s.SendJSON("", c.event, c.value); err != nil {
			return since, false, err
		}
	}
	rev = changes.Run.Rev
	if err := s.SendJSON(strconv.FormatInt(rev, 10), eventRun, changes.Run); err != nil {
		return since, false, err
	}
	return rev, true, nil
}
