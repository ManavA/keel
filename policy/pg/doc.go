// Package pg is the Postgres-backed [policy.Recorder]: the audit log of what
// agents were and were not allowed to do. Build a [policy.Decider] over a
// [Store] wherever decisions are acted on, so that each one is on the record
// before anything is done about it, and read the log back with [Store.List].
// The in-memory recorder is lost when the process ends and seen by that process
// alone; this one survives a restart and is shared by every instance.
//
// # The log is append-only
//
// [Store.Record] inserts one row per decision and [Store.List] reads. There is
// no update and no delete, so concurrent writers cannot conflict and a row,
// once written, is what was decided. The time is the record's own, from the
// Decider's clock, never the database's.
//
// # A decision that cannot be recorded is not allowed
//
// Record returns nil only when one row was written. Any other outcome is an
// error, and a [policy.Decider] given an error returns it with a zero Decision,
// whose empty effect no caller may read as an allow. That covers an
// unreachable database and a cancelled context as much as the refusals below.
//
// What the store refuses for good is told from a database that is down. An
// error for a record that can never be stored, whatever is retried, wraps
// [policy.ErrUnrecordable]: the refusals below that Record makes itself, and
// what the server refuses of the record's own content or size (SQLSTATE class
// 22, a data exception, such as a number past numeric's range; and class 54, a
// program limit, such as a value nested too deeply or a name too large for its
// index). A refused connection, a cancelled context, a failed transaction and a
// server that is shutting down do not wrap it. Whoever retries a failed step
// retries on the second kind only; the server's own error stays in the chain
// for [errors.As].
//
// # What a reader gets
//
// Attributes, the matched rules and the uncertain attributes are stored as
// JSON, and a nil one is stored as {} or [], never as JSON null. List reads
// them back as empty and never as nil: a record written with a nil Attrs, a nil
// Matched and a nil Uncertain comes back with an empty map and two empty lists.
// List itself returns an empty list, not nil, when nothing matches.
//
// Each record carries its ID, the log's own number for it, which with its time
// is its place in the log and is what a [Cursor] is made of.
//
// A time is stored and read in UTC. The column keeps microseconds: Record drops
// what is finer, and a time read back equals the one written truncated to the
// microsecond.
//
// Attribute values come back as encoding/json decodes them into an any: maps,
// lists, strings, booleans, nil, and numbers as [encoding/json.Number]. An
// attribute written as another Go type is read back as its JSON form: an int
// as a json.Number, a time.Time as its RFC 3339 text, a []byte as base64.
//
// # Numbers are not rounded
//
// The attrs column is jsonb, which keeps a number as a numeric: every digit,
// and the scale it was written with. 1180591620717411303424 and
// 0.30000000000000004 are stored as they are, and 1.50 stays 1.50. jsonb
// prints a number without an exponent and reads minus zero as 0, so 1e23 comes
// back as 100000000000000000000000: the same decimal, spelled out. List decodes
// with [encoding/json.Decoder.UseNumber], so no number passes through a
// float64 on the way back. A number that has to be exact must reach Record as
// a json.Number: a float64 is written as encoding/json writes it, the shortest
// decimal that gives it back, and a float64 holding an integer too large to be
// exact is therefore stored as that decimal and not as its exact value.
// jsonb holds a number of up to 131071 digits before the point; past that the
// record fails.
//
// # What cannot be recorded
//
// A NUL character cannot be stored: a text column refuses the byte, and jsonb
// refuses \u0000 in a string or a key. Attribute values come from tool input a
// model wrote, so one can arrive. Record looks for it before it sends anything,
// in the kind, the target, the rule names, the version and every attribute name
// and value, and returns an error that says so. Nothing is written, the next
// record is unaffected, and because the statement was never sent, a transaction
// the caller has open is not left aborted, as it would be by the error Postgres
// gives. The action whose record failed is not allowed. Text that only contains
// a backslash followed by u0000 is not a NUL and is stored.
//
// Record also refuses, without sending anything, a decision whose effect is not
// allow, ask or block (the table would refuse it), attributes that JSON cannot
// hold (NaN, an infinity, a function, a channel, a value that contains itself),
// and an empty json.Number anywhere encoding/json would write it, since it
// would write 0 and nobody sent a zero. Each wraps [policy.ErrUnrecordable].
// Anything else Postgres refuses, such as a number past its range, is its own
// error, wrapped, and is unrecordable when the server says the fault is in the
// record.
//
// Text that is not valid UTF-8 is not refused in a rule name, an attribute name
// or an attribute value: encoding/json replaces each bad byte with U+FFFD, so
// the log holds the replacement character and not the bytes. A text column (the
// kind, the target, the rule and the version) does refuse it, and the record
// fails as unrecordable.
//
// # Reading the log
//
// List returns a page of decisions, newest first, by the time each was decided.
// Of decisions decided at the same instant the one recorded later comes first,
// so the order does not change from one call to the next. A page is at most
// 1000 decisions, and 100 when the filter sets no limit: a larger limit is read
// as 1000, and the cursor reaches what a page leaves behind.
//
// To read the whole log, or all of it that matches a filter, set Filter.Before
// to the [Cursor] of the last record of each page, {At: rec.At, ID: rec.ID},
// and stop at an empty page. A cursor is a position: it returns what is older
// than it by time and then by ID, so records that share a time are each on one
// page and only one, however the pages fall among them, and, unlike an offset,
// it does not move when records are added. A cursor whose ID is below 1 is an
// error and no query is made.
//
// Apply [MigrationsFS] with keel's pg/migrate package before use; the migration
// is safe to run twice.
package pg
