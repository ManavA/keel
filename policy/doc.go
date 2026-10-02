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
// A condition on an attribute the action does not carry does not hold, under
// every operator, ne included. The other side of that is easy to miss: a
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
// Strings are not read as numbers.
//
// Numbers compare as numbers, exactly, whatever holds them: any integer or float
// type, or a [encoding/json.Number], which [Parse] uses for every number so a
// threshold keeps the digits it was written with. A float is read as the
// shortest decimal that gives it back, so a float64 0.1 meets a threshold of 0.1.
// Text of a number is read to a length of 4096 bytes and an exponent of 4096;
// past that it cannot be told. Strings and booleans compare with eq and ne only.
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
// other condition on the same attribute. An empty Kinds, Target or Attrs is not
// a mistake: each means every action, as the field says. It also refuses a rule
// named as a decision is named when none matched. Conditions that contradict one
// another in other ways, such as two eq on one attribute with different
// values, are not detected.
//
// # A decision that cannot be recorded is not an allow
//
// [Decider.Decide] returns an error and a zero [Decision] when the record
// cannot be written. The zero Decision's empty effect is not Allow, and a
// caller must treat an effect that is not one of the three as Block.
//
// # Immutable, and in-process by default
//
// A Decider takes a deep copy of its policy, every list and value in it, and
// hands out copies, so nothing a caller does afterwards, from any goroutine,
// changes the rules it decides under. The [Recorder] is handed a copy of each
// record too. [MemoryRecorder] is the Recorder a Decider uses when it is given
// none. It keeps the most recent decisions, 1000 unless Options.MemoryRecords
// says otherwise, and no more. policy/pg is the Postgres recorder.
//
// # Beside textpolicy
//
// This package imports nothing from textpolicy. textpolicy answers whether
// text may be stored or shown; this answers whether an action may happen. They
// meet through an attribute: a caller that checks outgoing text with textpolicy
// puts the name of the rule that matched into the action's attributes, and a
// rule here blocks on that attribute.
package policy
