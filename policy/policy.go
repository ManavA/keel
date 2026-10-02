package policy

// Effect is what a rule says about an action.
type Effect string

// The effects, from least strict to most.
const (
	Allow Effect = "allow"
	Ask   Effect = "ask"
	Block Effect = "block"
)

// Valid reports whether e is one of the three effects.
func (e Effect) Valid() bool {
	return e == Allow || e == Ask || e == Block
}

// rank orders the effects by strictness.
func (e Effect) rank() int {
	switch e {
	case Allow:
		return 0
	case Ask:
		return 1
	default:
		return 2
	}
}

// effective is the effect an unvetted value stands for: itself when it is one
// of the three, and Block otherwise. Whatever cannot be read as an allowance
// is a refusal, here as in every caller of this package.
func (e Effect) effective() Effect {
	if e.Valid() {
		return e
	}
	return Block
}

// Strictest returns the strictest of effects, or "" for none. An effect that
// is none of the three counts as Block.
func Strictest(effects ...Effect) Effect {
	if len(effects) == 0 {
		return ""
	}
	strictest := effects[0].effective()
	for _, e := range effects[1:] {
		if e = e.effective(); e.rank() > strictest.rank() {
			strictest = e
		}
	}
	return strictest
}

// Action is something an agent is about to do.
type Action struct {
	// Kind is the sort of action: "read", "write", "send", "pay", "delete",
	// "run", or any word a service's rules use.
	Kind string `json:"kind"`
	// Target is what it is done to, such as "email:ap@example.com".
	Target string `json:"target,omitempty"`
	// Attrs are the facts rules decide on: an amount, whether the action
	// reaches outside, whether it touches sensitive data.
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Op is a comparison a Cond makes.
type Op string

// The comparisons.
const (
	OpEq     Op = "eq"
	OpNe     Op = "ne"
	OpGt     Op = "gt"
	OpGte    Op = "gte"
	OpLt     Op = "lt"
	OpLte    Op = "lte"
	OpIn     Op = "in"
	OpExists Op = "exists"
)

// Cond is one condition on an attribute.
type Cond struct {
	Attr  string `json:"attr"`
	Op    Op     `json:"op"`
	Value any    `json:"value,omitempty"`
}

// Match says which actions a rule applies to. Every part that is set must
// hold; the zero Match applies to every action.
type Match struct {
	// Kinds lists the kinds the rule applies to. Empty is every kind.
	Kinds []string `json:"kinds,omitempty"`
	// Target is a path.Match pattern on Action.Target. Empty is every target.
	Target string `json:"target,omitempty"`
	// Attrs must all hold.
	Attrs []Cond `json:"attrs,omitempty"`
}

// Rule gives an effect to the actions it matches.
type Rule struct {
	// Name is recorded as the reason for a decision, so it must say what the
	// rule is in words a person reviewing the decision can read.
	Name   string `json:"name"`
	Effect Effect `json:"effect"`
	When   Match  `json:"when"`
}

// Policy is an ordered list of rules.
type Policy struct {
	// Version labels the rule set in every recorded decision.
	Version string `json:"version,omitempty"`
	// Default is the effect when no rule matches. Empty is Block.
	Default Effect `json:"default,omitempty"`
	Rules   []Rule `json:"rules"`
}

// RuleDefault is the rule name recorded when no rule matched.
const RuleDefault = "no rule matched"

// Decision is the outcome for one action.
type Decision struct {
	Effect Effect `json:"decision"`
	// Rule names the rule that decided, or RuleDefault.
	Rule string `json:"rule"`
	// Index is that rule's position in Policy.Rules, or -1.
	Index int `json:"index"`
	// Matched names every rule that matched, in order.
	Matched []string `json:"matched,omitempty"`
}

// Judged is an action with its decision.
type Judged struct {
	Action   Action   `json:"action"`
	Decision Decision `json:"decision"`
}

// Grouped is a batch of actions sorted by effect, each group in input order.
type Grouped struct {
	Allow []Judged `json:"allow"`
	Ask   []Judged `json:"ask"`
	Block []Judged `json:"block"`
}
