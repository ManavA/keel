// Package httpapi serves runs, their timelines and their approvals over HTTP,
// for a person or a front end to list runs, read what one has done, follow it
// as it goes, and approve, decline or cancel. It talks to the engine through
// the small [Runs] interface, which *agent.Engine satisfies, so the same
// surface serves any process that can read the store, whichever one is
// executing a run.
//
// The routes are in [API.Routes]. There is none that starts a run: what a
// run's input is, and who may start one, belong to the service.
//
// # What it serves, and to whom
//
// A run's journal holds whatever its tools handled, so [API.Routes] belongs
// behind whatever guards operator traffic. The three routes that change
// something (cancel, approve, decline) also need [Options.Actor] to name the
// caller, for the record of who decided, and answer 403 without one: the zero
// [Options] serves a read-only surface. A step's provider-private form,
// Message.Opaque, which can hold the model's own reasoning, is removed from
// every step before it is served, on the timeline and on the stream.
//
// # Ids and errors
//
// An id in a path or a filter is given to the engine as written. Whether it
// has the form of an id is the store's to say, and one that does not names
// nothing: 404, not 400. A failure answers with httpx's generic body and the
// cause goes to the log. A listing's limit and status, a cursor, and a body
// that is over 4 KiB or says more than a reason are refused with 400 rather
// than clamped or ignored.
//
// # The event stream
//
// GET /runs/{id}/events is a server-sent event stream of one run's journal. It
// does not rely on the engine's events, which are a best-effort hint: it
// reads the journal by revision, so it works whichever process executes the
// run and cannot skip a change. It sends one step or approval event for each
// that changed, then one run event whose id is the run's revision, and when
// the run has ended an end event, after which the response is over. A client
// that reconnects with that id in Last-Event-ID is sent what came after it.
// What the stream carries is state and not history: a step that started and
// completed between two reads appears once, completed.
//
// A Last-Event-ID that is not a revision of this run (not a number, negative,
// or beyond the run's own) is taken as no position, and the whole state is
// sent again. It is always safe to send again, and refusing the header would
// strand an EventSource, which cannot change what it sends.
//
// The stream has no timeout of its own: each send has its own write deadline
// ([httpx.EventStreamOptions.SendTimeout]), and the stream ends when the
// request's context does. A service that wants one connection to last builds
// its router with the timeout turned off, as [httpx.RouterOptions] says; under
// the default router the context ends after 30 seconds, and an EventSource
// reconnects with Last-Event-ID and loses nothing. A HEAD request is answered
// with the stream's headers and no stream.
package httpapi
