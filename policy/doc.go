// Package policy decides what an action may do. An action has a kind, a
// target and attributes; a rule gives the actions it matches one of three
// effects, [Allow], [Ask] a person, or [Block]; and when several rules match,
// the strictest wins. Every decision a [Decider] makes is recorded with the
// rule that made it, so a person reviewing the log reads which rule, in the
// rule's own words, allowed or refused each action.
//
// Rules are data. A [Policy] is a Go value and has a JSON form that
// round-trips through [Parse], which refuses an unknown field, a key repeated
// within an object, a document that is not an object, and anything
// [Policy.Validate] refuses. Use [Policy.Decide] to ask what would happen: it
// is pure and records nothing. Use a [Decider] where the decision has to be on
// the record before anything is done about it.
//
// # The strictest rule wins
//
// Effects order as allow, ask, block. The decision is the first match of the
// greatest strictness: walking the rules in order, a match replaces the best
// so far only when it is strictly stricter. A later rule can therefore never
// loosen an earlier one, and of two equally strict rules the earlier is the
// one reported. Put the rule you most want a reviewer to read first.
//
// # An action no rule matches is blocked
//
// An action nobody wrote a rule for is not one anybody allowed, so its effect
// is [Policy.Default] and, when that is empty, [Block], reported under the
// rule name [RuleDefault] with index -1. An effect that is none of the three
// is Block wherever this package meets one.
//
// # What a rule matches
//
// A [Match] holds when every part that is set holds.
//
// Kinds are compared exactly, and an empty list is every kind.
//
// A target is a [path.Match] pattern, and an empty one is every target. A star
// matches any run of characters but a slash, including none; a question mark
// matches one character but a slash; brackets hold a class, as in [a-z] and
// [^0-9]; a backslash makes the next character literal. So "file:/data/*"
// matches "file:/data/a" and not "file:/data/a/b", and a bare "*", which
// would match no target that holds a slash, is refused: leave the target empty
// for every target. A target is matched exactly as written, with no case
// folding, no trimming and no cleaning of dots or slashes, so a block rule on
// "file:/etc/*" is passed by "file:/etc//passwd", by "file:/ETC/passwd" and by
// "file:/etc/../etc/passwd". Whatever builds an action must put its target in
// one canonical spelling before asking for a decision.
//
// # Attributes: absent, present, and not comparable
//
// Attribute names and string values are matched exactly as written, as targets
// and kinds are: "Amount" is not "amount", and "Prod" and "prod " are not
// "prod". Whatever builds an action must use one spelling.
//
// A condition on an attribute the action does not carry does not hold, under
// every operator, ne included. An attribute that is present and null is
// present: exists true holds for it, and every other operator cannot tell. The other side of that is easy to miss: a
// block rule written with ne does not block an action that lacks the
// attribute. To block a visit unless its region is home, write two rules:
//
//	{"name": "Away is blocked", "effect": "block",
//	 "when": {"kinds": ["visit"], "attrs": [{"attr": "region", "op": "ne", "value": "home"}]}}
//	{"name": "No region is blocked", "effect": "block",
//	 "when": {"kinds": ["visit"], "attrs": [{"attr": "region", "op": "exists", "value": false}]}}
//
// An attribute that is present but of a type its operator cannot compare, such
// as an amount that arrives as the string "1250", a list, a pointer or NaN, is
// neither held nor failed: it cannot be told. A condition holds, does not hold,
// or cannot be told; a rule matches for certain when every condition holds, and
// does not match when any does not hold. When none fails but one cannot be told,
// an ask or block rule matches and an allow rule does not, so what cannot be
// evaluated counts toward the stricter outcome. The [Decision] then says so in
// [Decision.Uncertain], naming the attribute, so the record explains why a rule
// whose condition looks unmet decided. The same holds for a [Policy] that
// skipped Validate and has a rule that cannot be evaluated, a target pattern
// that does not compile or a malformed condition: it matches unless it allows.
// Strings are not read as numbers. In is eq on each element joined by "or": it
// holds if any element equals the attribute, does not hold only if every
// element could be compared with it and none equalled it, and otherwise cannot
// be told, so a block rule on in [22, "ssh"] blocks the string "22".
//
// Numbers compare as numbers, exactly, whatever holds them: any integer or float
// type, or a [encoding/json.Number], which [Parse] uses for every number so a
// threshold keeps the digits it was written with. A float without an integer
// value is the shortest decimal that gives it back, so a float64 0.1 meets a
// threshold of 0.1. A float with an integer value has two readings, that integer
// and its shortest decimal, which are one number below 2^53 and two above; a
// condition is told under both, and where they disagree it cannot be told, so a
// float64 1e23 against gte 1e23 asks or blocks, and is named in Uncertain. A
// float cannot be told against a threshold within its rounding. Text of a number
// is read to a length of 4096 bytes and an exponent of 4096; past that it
// cannot be told. Strings and booleans compare with eq and ne only.
//
// A number that is meant to be exact must reach the package as a json.Number:
// one decoded into a float64 has already rounded. Decode tool input with
// [encoding/json.Decoder.UseNumber], as Parse does, and pass the json.Number on.
//
// # Ask
//
// The middle effect is called Ask, and is written "ask", because the agent
// package has an Approve that means a person said yes, and a log reading
// "approve" for a call still waiting would mislead. [Effect.UnmarshalText]
// reads "approve" as Ask so that a policy written with the older word loads.
//
// # A rule that matches nothing by mistake
//
// [Policy.Validate] refuses a rule whose condition could never hold, or would
// hold for every action, because on a block rule that fails open without a
// word: a condition with no attribute, an eq or ne with no value, an in with
// an empty list or an element that is not a number, a string or a boolean, a
// number that is not finite, an empty kind, and exists false together with any
// other condition on the same attribute, or written twice. An empty Kinds, Target or Attrs is not
// a mistake: each means every action, as the field says. It also refuses a rule
// named as a decision is named when none matched. Conditions that contradict one
// another in other ways, such as two eq on one attribute with different
// values, are not detected.
//
// # A policy that no record could hold
//
// A decision record holds the name of the rule that decided and of each that
// matched, the policy's version, and the names of the attributes a condition
// could not tell. A name and the version go to text columns, and a name is
// indexed, so [Policy.Validate] refuses what would load and then fail to record
// every decision made under it, or record something other than what decided: a
// rule name or an attribute name or the version that holds a NUL character or
// is not valid UTF-8, and a rule name or a version of more than 256 bytes. A
// kind and a target pattern are never in a record, but are refused for a NUL,
// since one that holds it can only match an action that could never be
// recorded. Each message names the rule, so that [Parse] and [NewDecider] stop
// at load and not at the first decision.
//
// # A decision that cannot be recorded is not an allow
//
// [Decider.Decide] returns an error and a zero [Decision] when the record
// cannot be written. The zero Decision's empty effect is not Allow, and a
// caller must treat an effect that is not one of the three as Block.
//
// An error for a record that can never be stored, however often it is tried
// again, wraps [ErrUnrecordable]: the error of a [Recorder] that says so, and
// the Decider's own refusal of an action with too many values to copy. An error
// that does not wrap it is the recorder's or the moment's, such as a database
// that is down, and a later call may not meet it. A caller that retries what
// fails (an agent step, say) retries on those and stops on this one, since a
// retry of a record that can never be stored never ends. The Decider returns
// no decision either way.
//
// # Immutable, and in-process by default
//
// A Decider takes a deep copy of its policy, every list and value in it, and
// hands out copies, so nothing a caller does afterwards, from any goroutine,
// changes the rules it decides under. The [Recorder] is handed a copy of each
// record too. A copy is bounded in work as well as depth: a policy whose
// conditions, or an action whose attributes, hold more than 10000 values is
// refused, by NewDecider and by Decide, which returns an error and no decision.
// The error for an action wraps [ErrUnrecordable], since no retry will make the
// action smaller; the error for a policy does not, since a policy is not a
// record and is not retried. [MemoryRecorder] is the Recorder a Decider uses
// when it is given none, and its zero value is ready to use. It keeps the most
// recent decisions, 1000 unless Options.MemoryRecords says otherwise, and no
// more, and it numbers none of them: [Record.ID] is the number a store that
// keeps one gives a record it lists, which with the time is the record's place
// in the log, and is zero in what the Decider hands a recorder and in what
// MemoryRecorder returns. policy/pg is the Postgres recorder.
//
// # Beside textpolicy
//
// This package imports nothing from textpolicy. textpolicy answers whether
// text may be stored or shown; this answers whether an action may happen. They
// meet through an attribute: a caller that checks outgoing text with textpolicy
// puts the name of the rule that matched into the action's attributes, and a
// rule here blocks on that attribute.
package policy
