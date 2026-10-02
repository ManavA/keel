// Package policy decides what an action may do. An action has a kind, a
// target and attributes; a rule gives the actions it matches one of three
// effects, [Allow], [Ask] a person, or [Block]; and when several rules match,
// the strictest wins. Every decision a [Decider] makes is recorded with the
// rule that made it, so a person reviewing the log reads which rule, in the
// rule's own words, allowed or refused each action.
//
// Rules are data. A [Policy] is a Go value and has a JSON form that
// round-trips through [Parse], which refuses an unknown field and anything
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
// A [Match] holds when every part that is set holds. Kinds are compared
// exactly. A target is a [path.Match] pattern, so a star does not cross a
// slash. A condition on an attribute the action does not carry never holds,
// except exists with the value false; that way a rule on an amount is not
// satisfied by an action that states none. Numbers compare as numbers whatever
// their Go type, exactly, so an int64 and a float64 that differ in the last
// place are different. Strings and booleans compare with eq and ne only, and
// the ordering operators hold between numbers only.
//
// # Ask
//
// The middle effect is called Ask, and is written "ask", because the agent
// package has an Approve that means a person said yes, and a log reading
// "approve" for a call still waiting would mislead. [Effect.UnmarshalText]
// reads "approve" as Ask so that a policy written with the older word loads.
//
// # A decision that cannot be recorded is not an allow
//
// [Decider.Decide] returns an error and a zero [Decision] when the record
// cannot be written. The zero Decision's empty effect is not Allow, and a
// caller must treat an effect that is not one of the three as Block.
//
// # In-process by default
//
// [MemoryRecorder] is the [Recorder] a Decider uses when it is given none, and
// its log lasts as long as the process. policy/pg is the Postgres recorder.
//
// # Beside textpolicy
//
// This package imports nothing from textpolicy. textpolicy answers whether
// text may be stored or shown; this answers whether an action may happen. They
// meet through an attribute: a caller that checks outgoing text with textpolicy
// puts the name of the rule that matched into the action's attributes, and a
// rule here blocks on that attribute.
package policy
