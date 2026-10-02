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
// This package does not authenticate, and it does not decide who may decide
// which approval. Anyone for whom [Options.Actor] returns a name can approve,
// decline or cancel anything the API can see: Actor is a name for the record
// of who decided, not an authorisation, and authorising is the mount's. A
// run's journal holds whatever its tools handled, so [API.Routes] belongs
// behind whatever says who may read it and who may decide.
//
// The three routes that change something (cancel, approve, decline) need Actor
// to name the caller and answer 403 without a name, or with one that is
// nothing but white space, or with one that cannot be recorded (it is not text,
// holds a control character, is over 256 bytes, or has nothing to see in it,
// as a name of only zero-width characters has not): the zero [Options] serves a
// read-only surface. A reason that is not valid UTF-8 or has a NUL in it is
// refused with 400, since a database could not keep it. Everything but the stream is sent with
// Cache-Control: no-store. A step's provider-private form,
// Message.Opaque, which can hold the model's own reasoning, is removed from
// every step before it is served, on the timeline and on the stream.
//
// Those three routes also refuse a browser request made for a page on another
// origin, with 403 and before [Options.Actor] is asked, using the standard
// library's [net/http.CrossOriginProtection] ([Options.CrossOrigin]; the
// default trusts no origin, and a service whose front end is on another origin
// supplies one that trusts it). A request is cross-origin when Sec-Fetch-Site
// says so or, with no such header, when its Origin is not its host; one with
// neither header is a server-side client or a tool such as curl, and passes.
// This closes the hole of a cookie session being driven by a form on another
// site. It is not authentication, and says nothing of who is asking: Actor
// only names the caller, for the record. It does not guard reading or the
// stream, which change nothing.
//
// # Ids and errors
//
// An id in a path or a filter is given to the engine as written. Whether it
// has the form of an id is the store's to say, and one that does not names
// nothing: 404, not 400. A failure answers with httpx's generic body and the
// cause goes to the log. What the engine answered is told whatever became of
// the request. A request that was cut off (its context cancelled, which a
// server shutting down, a client that half-closes after sending and a mount's
// middleware all do with the client still reading) or whose time ran out is
// 503, the first logged at debug level and not as a fault; no failure leaves
// the status a handler starts with, since that would read as success. A listing's limit and status, a cursor, and a body
// that is over 4 KiB or says more than a reason are refused with 400 rather
// than clamped or ignored. A cursor is the client's input, and is read
// strictly: one that does not decode, or whose time does not parse, or whose
// id is not a UUID in the canonical lower-case hyphenated form, is refused
// before the engine is asked.
//
// # The event stream
//
// GET /runs/{id}/events is a server-sent event stream of one run's journal. It
// does not rely on the engine's events, which are a best-effort hint: it
// reads the journal by revision, so it works whichever process executes the
// run and cannot skip a change. It sends one step or approval event for each
// that changed, then one run event whose id is the run's revision, and when
// the run has ended an end event, after which the response is over. A client
// that reconnects with that id in Last-Event-ID is sent what came after it. A
// client closes its event source on end: a reconnect to a finished run is sent
// end again, so one that goes on reconnecting would loop at the browser's retry
// interval. A read that finds nothing new sends nothing, and an idle stream
// sends a comment every Heartbeat. What the stream carries is state and not
// history: a step that started and completed between two reads appears once,
// completed.
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
