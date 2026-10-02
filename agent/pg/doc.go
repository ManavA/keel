// Package pg is the Postgres-backed agent.Store: the one that keeps a run and
// its journal when the process executing it does not survive.
//
// Use it wherever a run must outlive a restart or more than one process may
// execute runs. agent.MemoryStore is the in-process store, for tests and for
// a service that needs neither; both pass agenttest.RunStoreSuite, so a test
// written against one holds for the other.
//
// Apply the schema in this package's migrations directory before using the
// store. MigrationsFS embeds it:
//
//	migrate.Run(ctx, pool, migrate.Options{FS: agentpg.MigrationsFS, Dir: "migrations"})
//	store := agentpg.New(pool)
//
// # The run's row is the lock
//
// Every method is one transaction, and every transaction that changes a run
// begins by locking the run's row. What it then decides on, it reads in a
// later statement, under the lock. Three guarantees rest on that.
//
// A journal write is fenced: it compares the caller's Lease with the owner
// and epoch on the locked row, and writes nothing unless they are equal. A
// claim takes the same lock and raises the epoch, so a write either lands
// before a claim, and is in the journal the new holder reads, or is refused.
// The comparison is of owner and epoch and never of time.
//
// Park looks for something to wait for after it holds the row. An answer, a
// lapse, a request to cancel and the end of a child run each take that row
// before they change what Park reads, so each lands wholly before the look
// or wholly after the write, and a run is never left waiting for something
// that has already happened. This is why a child's Finish locks its parent's
// row even when the parent is not waiting.
//
// A claim that names its run waits for the row and then decides, so it is
// not refused because the run's last holder was in the middle of a write. A
// claim that names none passes over a row that is locked and takes the next,
// so two workers looking for work never wait on each other.
//
// Rows are always taken child before parent. A child's Finish takes its own
// and then its parent's; ExpireApprovals and Purge take every run they will
// change before they change any, deepest first and by id within a depth. The
// order uses the Depth a run was created with, which CreateRun holds to one
// more than its parent's.
//
// The store asks for read committed by name in each of these transactions.
// The look after the lock sees what was committed during the wait only at
// that level, and a database whose default is stricter would have it decide
// on a snapshot taken before the wait. Changes alone reads under repeatable
// read, so that the run and the steps it returns are one moment's.
//
// # What is kept, and as what
//
// An id is a UUID in the form uuid.NewString writes, and that string alone
// names the run or the approval: another spelling of the same UUID is
// refused where an id is to be kept and finds nothing where one is looked
// up, without a query. A uuid column would take any spelling and hand back
// the canonical one.
//
// Every time comes from the request. The store reads no clock, in Go or in
// SQL. A time is kept to the microsecond below it and read back in UTC.
//
// JSON that somebody else wrote is kept as the bytes it came as: a call's
// arguments, a turn in its provider's own form, a tool's schema and the
// output schema. Those are json columns, which keep text, and the store
// writes the surrounding object itself, because encoding/json would respace
// and re-escape a raw value on the way in. A raw value that is not JSON, or
// not UTF-8, is refused before the transaction is opened. Metadata and an
// action are jsonb columns and read back equal in value: a number as a
// float64, no attributes as an empty map.
//
// A TEXT column holds neither a NUL character nor a byte that is not UTF-8,
// and a jsonb column no NUL. A model, a tool or a person can put either in
// anything they write, and the store never sends Postgres a string it would
// refuse. A string the store only records is kept with each such character
// as the replacement character U+FFFD: a tool's result, a run's input,
// output and error, a reason, a rule, the name of a tool or a model, who
// decided or cancelled and why, and the strings inside metadata and an
// action. So no journal write fails, and no run is wedged, for what a model
// or a tool wrote. A string the store compares is refused before a
// transaction is opened, since it could only be kept as another name: an
// agent's name, a start key, an owner and a tool effect's key. Inside the
// json columns a NUL is kept, as the escape \u0000, and only a byte that is
// not UTF-8 is replaced. agent.MemoryStore does the same, and the suite
// holds both to it.
//
// # Once
//
// A completed step is never executed again, but a tool call that was
// interrupted is, with the same Invocation.Key. A tool whose effect is a
// write to this database makes it exactly once by calling Once with that key
// in the transaction that makes the write. Purge removes a run's keys with
// the run.
package pg
