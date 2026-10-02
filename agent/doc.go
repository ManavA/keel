// Package agent runs an agent to completion across process deaths. A run is
// a journal: every model call and every tool call is written down before it
// starts and after it finishes, so a process that dies mid-run is replaced
// by any other, which carries on from the step the journal stops at.
//
// The package owns the loop (ask the model, judge each tool call, execute
// it, feed the results back), the journal that makes the loop resumable, the
// lease that lets any process pick a run up, the budgets that stop it, the
// approvals that park it for a person, and child runs. It imports no model
// and no rule engine. It declares the four small interfaces it consumes,
// [Model], [Guard], [Clock] and [Publisher], and the app package has the
// adapters that put llm and policy behind the first two.
//
// # In-process by default
//
// [MemoryStore] is the [Store] an [Engine] uses when it is given none, and
// its runs last as long as the process. agent/pg is the Postgres Store, for
// runs that outlive it. agent/httpapi serves runs and approvals over HTTP,
// and agent/agenttest has the clock, the scripted model and the
// fault-injecting store that tests of an agent are built from.
//
// # What a crash repeats
//
// A completed step is never executed again, across any number of crashes and
// takeovers. An interrupted model call is made again, which costs money and
// nothing else. An interrupted tool call is executed again with the same
// [Invocation.Key]: that is at-least-once, and whether the effect happens
// once is the tool's to arrange with the key. A write to the same Postgres
// records the key in its own transaction with agent/pg's Once. An effect
// elsewhere passes the key to the receiver as its idempotency key. A tool
// that can do neither sets [Tool.AtMostOnce], and a person is asked before
// an interrupted call is made again. Nothing here makes an effect outside
// Postgres happen exactly once on its own.
//
// # A run keeps what it started with
//
// The system prompt, the tool list and the limits are copied onto the run
// when it starts, as a [Snapshot], and every request to the model is built
// from that copy and the journal. The conversation is only ever appended to:
// a deploy cannot edit one already under way, which a provider that checks a
// replayed turn against what came before it would reject. Tool code still
// comes from the running build, looked up by name.
//
// # Time and the lease
//
// Every timestamp, lease and budget is measured on the engine's [Clock] and
// not the database's, so a test moves time by hand. Processes sharing runs
// must keep their clocks within a small fraction of the lease's length of
// each other. A wrong clock costs repeated work and never a corrupt journal:
// every write is fenced by [Lease.Epoch], which rises with each claim, and
// not by time.
//
// # Events are a hint
//
// An [Event] is published after the write it reports, at most once, and
// carries nothing from the journal. A consumer that needs every change keeps
// the last [Run.Rev] it saw and reads the changes since, using events only
// as the reason to look.
package agent
