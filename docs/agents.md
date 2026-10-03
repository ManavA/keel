# Agents

`agent` runs a language-model agent to completion across process deaths. This
guide covers what the journal does when a process dies, how to make a tool
safe to run twice, approvals, rules, budgets, testing, running the worker, and
what to put in front of the HTTP surface. The package comments of `agent`,
`llm`, `policy`, `agent/httpapi` and `policy/pg` say more about each part.

Nothing in `llm/anthropic` or `llm/openai` has been run against a real
provider yet. Each has a live test behind the `live` build tag that needs an
API key (`go test -tags live ./llm/anthropic/`). Everything else in this guide
runs against a scripted model, with no network and no key.

## The journal and what a crash does

A run is a journal. Every model call and every tool call is written down
before it starts and after it finishes, in one row per step. A process that
dies mid-run is replaced by any other that has the run's agent registered;
the new one reads the journal and carries on from the step it stops at.

What "exactly once" covers:

- A completed step is never executed again, across any number of crashes and
  takeovers. A step is recorded once, and every journal write is fenced.
- An interrupted model call is made again. That costs money and nothing else.
  The lost call's cost is on no record.
- An interrupted tool call is executed again with the same `Invocation.Key`.
  That is at-least-once. Whether its effect happens once is up to the tool.
- An approval is decided once, and what is approved is what runs: the
  arguments shown are the journal's, and they are the arguments the tool gets.
- A run is started once per `StartRequest.Key`.
- An `Event` is published after the write it reports, at most once. The
  journal is the record.

Nothing here makes an effect outside Postgres happen exactly once on its own.

A run keeps what it started with. The system prompt, the tool list and the
limits are copied onto it as a `Snapshot`, and every request is built from
that copy and the journal, so a deploy cannot change a conversation under
way. Tool code comes from the running build, by name.

## Starting a run

```go
engine, err := agent.New(agent.Options{
	Model: model,
	Store: store,
	Guard: guard,
})
if err != nil {
	return err
}
err = engine.Register(agent.Definition{
	Name:   "support",
	System: "You answer questions using the tools you are given.",
	Model:  "claude-sonnet-4-5",
	Tools:  []agent.Tool{lookup},
})
if err != nil {
	return err
}
run, err := engine.Start(ctx, agent.StartRequest{
	Agent: "support",
	Input: "Where is order 1042?",
	Key:   "ticket-1042",
})
```

`Start` with the same agent and key returns the first run. `Store` defaults to
`agent.MemoryStore`, whose runs last as long as the process; use `agent/pg` for
runs that outlive it. Without a `Guard` every tool call is allowed, and the
journal records "no guard configured" as the reason.

## Making a tool idempotent

A tool is a `Run` function that receives an `Invocation`. Its `Key` is the same
on every attempt, so it is the tool's handle for doing an effect once.

A tool whose effect is a write to the same Postgres calls `agent/pg`'s `Once`
in the transaction that makes the write, and skips the write when `Once` says
this key has been seen. The key and the write commit together or not at all,
and two concurrent attempts serialise on the key's unique index.

```go
func refund(pool *pgxpool.Pool) agent.Tool {
	return agent.Tool{
		Name:        "refund",
		Description: "Refund an order.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"order":{"type":"string"}},"required":["order"]}`),
		Run: func(ctx context.Context, in agent.Invocation) (string, error) {
			var args struct{ Order string }
			if err := json.Unmarshal(in.Call.Input, &args); err != nil {
				return "", err
			}
			err := keelpg.InTx(ctx, pool, func(tx pgx.Tx) error {
				first, err := agentpg.Once(ctx, tx, in.Key)
				if err != nil || !first {
					return err
				}
				_, err = tx.Exec(ctx, `update orders set refunded = true where id = $1`, args.Order)
				return err
			})
			if err != nil {
				return "", fmt.Errorf("refund: %w", err)
			}
			return "refunded", nil
		},
	}
}
```

A tool whose effect is elsewhere passes `in.Key` to the receiver as its
idempotency key, or writes an outbox row under `Once` and lets `outbox.Relay`
deliver it, which is at-least-once with a stable id for the consumer to dedupe
on.

A tool that can do neither sets `AtMostOnce`. An interrupted call is then not
made again until a person, shown the call, approves it.

A tool error is returned to the model as a failed result. An error wrapping
`agent.ErrTransient` instead leaves the call to be tried again later with the
same key.

### A tool that outlives its lease

When a worker loses its lease, a tool call it started can keep running. With
the store answering, that is up to one heartbeat interval plus a round trip.
With the store failing, it is less than two heartbeat intervals past the
lease's lapse. A tool that ignores its context is not bounded at all. The
store's fence stops the stale worker writing to the journal. It cannot stop a
tool's outside effect, which is why the key matters even when no process ever
crashes: the new holder may run the same call while the old one is still in it.
A tool should watch its context and stop when it ends.

## Approvals

A tool call is put to a person when the guard says `Ask`, when the tool is
marked `Approval`, or when an `AtMostOnce` call was interrupted. The step
becomes `waiting`, an approval is recorded with the call's arguments, the
action and the rule, and when nothing else can proceed the run parks: status
`waiting`, no lease. Nothing about it is in a process, so every process can be
stopped and the run is where it was.

```go
approvals, err := engine.ListApprovals(ctx, agent.ApprovalFilter{})
// ...
_, err = engine.Approve(ctx, approvals[0].ID, "alice", "checked the order")
// or: engine.Decline(ctx, id, "alice", "not this one")
```

A declined call becomes the model's result and the run goes on. Deciding twice
returns `agent.ErrAlreadyDecided`. `Options.ApprovalTTL` lapses an unanswered
approval, which declines the call; zero never lapses. A person acts through
`agent/httpapi` or by calling the engine directly.

## Writing rules

Rules decide what each tool call may do. A tool describes a call to the guard
with `Tool.Action`, or by default as kind `run` with the tool's name as target.
The engine adds the attributes `agent`, `tool`, `run` and `seq`.

A `policy.Policy` is a list of rules, each with an effect (`allow`, `ask`,
`block`) and the actions it matches. It has a JSON form:

```go
p, err := policy.Parse([]byte(`{
  "version": "2026-10-01",
  "rules": [
    {"name": "Reading orders is allowed", "effect": "allow",
     "when": {"kinds": ["run"], "target": "lookup"}},
    {"name": "Refunds are put to a person", "effect": "ask",
     "when": {"kinds": ["run"], "target": "refund"}}
  ]
}`))
if err != nil {
	return err
}
decider, err := policy.NewDecider(p, policy.Options{Recorder: recorder})
if err != nil {
	return err
}
guard := app.AgentGuard(decider)
```

Four things to know:

- The strictest matching rule wins, and a later rule never loosens an earlier
  one. Put first the rule you want a reviewer to read first.
- An action no rule matches is blocked, under the rule name "no rule
  matched", unless `Policy.Default` says otherwise.
- A condition on an attribute the action lacks does not hold, under every
  operator, `ne` included. To block "unless the region is home", write one rule
  with `ne` and another with `exists false`. `policy`'s package comment has the
  example.
- A condition that cannot be evaluated, such as a number that arrives as a
  string, counts toward the stricter outcome: an ask or block rule matches and
  an allow rule does not. The decision says which attribute it could not tell.

Targets are matched exactly as written, with no cleaning of case or path. A
tool that builds an action must put the target in one spelling before asking.
Decode tool input with `UseNumber` when a number must be compared exactly.

`policy.MemoryRecorder` keeps the last 1000 decisions in memory. `policy/pg`
is the append-only Postgres log; apply its `MigrationsFS` with `pg/migrate`.
A decision that cannot be recorded returns an error and no decision, and the
engine does not run the call.

## Budgets

A run has four limits, copied onto it when it starts: `MaxDuration` (time in
model and tool calls, not time parked; default 15 minutes), `MaxCostMicros`,
`MaxTokens` and `MaxModelCalls` (default 50). A zero field takes its default
and a negative field is no limit. They are checked before each action that does
work, so a run can pass a limit by the size of its last step.

`Definition.Limits` sets them for an agent, and `StartRequest.Limits` replaces
them for one run. A child run's cost limit is the smaller of its own and what
its parent has left.

Cost needs prices, and `llm` ships none: prices change, and a copy in a library
goes stale unnoticed. The caller supplies `llm.Prices`, and a model the table
lacks is `llm.ErrNoPrice` rather than free. To bound a single call before it is
made, wrap the model in `llm.Budgeted`, and bound a reply with
`Definition.MaxTokens`.

Compose the `llm` wrappers with the budget outermost and the meter inside it,
so the meter records only calls the budget let through. Then any fallback
chain, and one retrying wrapper for each provider:

```go
primary := llm.NewRetrying(client, llm.RetryOptions{})
metered := llm.NewMetered(primary, llm.MeterOptions{Prices: prices})
budgeted, err := llm.NewBudgeted(metered, llm.BudgetOptions{
	MaxCostMicros: 5_000_000,
	Prices:        prices,
	Model:         "claude-sonnet-4-5",
})
if err != nil {
	return err
}
model := app.AgentModel(budgeted, app.AgentModelOptions{
	Prices: prices,
	Model:  "claude-sonnet-4-5",
})
```

`app.AgentModel` always sends a reply bound, so that a budget's hold is a real
upper bound. Give it and the budget the same model name.

## Testing an agent

`agent/agenttest` has what a test needs and no network: `Clock`, moved by hand;
`Model`, which answers from a script; `FaultStore`, which stops a process at a
chosen store call; and `RunStoreSuite`, which every `agent.Store` passes.
`llm.Scripted` does the same for code written against `llm.Model`.

```go
func TestRunAnswers(t *testing.T) {
	clock := agenttest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	model := agenttest.NewModel(agenttest.Replies(
		agenttest.Use(agenttest.Call("c1", "lookup", `{"order":"1042"}`)),
		agenttest.Say("It shipped."),
	))
	engine, err := agent.New(agent.Options{Model: model, Clock: clock})
	require.NoError(t, err)
	require.NoError(t, engine.Register(agent.Definition{Name: "support", Tools: []agent.Tool{lookup}}))

	run, err := engine.Start(t.Context(), agent.StartRequest{Agent: "support", Input: "where?"})
	require.NoError(t, err)
	run, err = engine.Execute(t.Context(), run.ID)
	require.NoError(t, err)
	require.Equal(t, agent.StatusCompleted, run.Status)
}
```

A script picks its reply from the request and the number of assistant turns
already in it, so a run resumed after a crash is sent the reply for the point
its journal reached. To test a crash, count an uninterrupted run's store calls
with `FaultStore.Calls`, then kill at each in turn with `KillBefore` and
`KillAfter`, and finish the run on the wrapped store as another process would.
For a model written against `llm`:

```go
model := llm.NewScripted(llm.Replies(llm.Reply{Text: "hello"}), llm.ScriptedOptions{})
```

## Running the worker

`engine.Work(ctx)` claims runnable runs and executes them until `ctx` ends;
`engine.Tick(ctx)` makes one pass, for a test or a scheduler. Every process
that may resume a run must register its agent.

```go
engine, err := agent.New(agent.Options{
	Model:             model,
	Store:             agentpg.New(pool),
	Guard:             guard,
	LeaseTTL:          30 * time.Second,
	HeartbeatInterval: 10 * time.Second,
	Concurrency:       4,
})
// ...
err = engine.Work(ctx)
```

`LeaseTTL` is how long a run whose process died waits to be taken over, and the
lease is extended every `HeartbeatInterval`, a third of the TTL by default
(`New` refuses an interval that is not below it). Size it from two things.
A short TTL recovers a dead worker quickly. It also tolerates less: a pause or
a slow database that outlasts it hands the run to another process while the
first is still in a tool, with the consequences above. A worker that cannot reach the store for a whole TTL cancels its execution.

Lease times use the engine's `Clock`, not the database's, so processes sharing
runs must keep their clocks within a small fraction of `LeaseTTL`. A wrong
clock costs repeated work and never a corrupt journal: every write is fenced by
the lease's epoch, which rises with each claim.

When `ctx` is cancelled, nothing more is claimed and each execution finishes
the action it is in the middle of, then gives its run back. `DrainTimeout`
bounds that wait.

A tool still running when `DrainTimeout` runs out may still be making its
effect, so nothing is written for its run and the lease is left to lapse. The
worker that takes the run over counts the lapse as one of the run's failures,
as it would for a process that died; the store cannot tell the two apart. A
run whose tool is cut off by `MaxFailures` deploys in a row is finished as
failed. With `DrainTimeout` above every tool's `Timeout` (a tool with none has
two minutes), a shutdown does not cut off a tool that keeps to its
context.

## What to mount the HTTP surface behind

`agent/httpapi` serves runs, timelines, approvals and a server-sent event
stream of one run's journal:

```go
api, err := httpapi.New(httpapi.Options{
	Runs:  engine,
	Actor: func(r *http.Request) string { return operatorName(r) },
})
if err != nil {
	return err
}
router.Mount("/agent", operatorOnly(api.Routes()))
```

It does not authenticate and it does not authorise. The mount does. `Actor`
returns a name for the record of who decided an approval; it is not a check,
and anyone for whom it returns a name can approve, decline or cancel anything
the API can see. A run's journal holds whatever its tools handled, so the
surface belongs behind whatever says who may read it and who may decide. With
the zero `Options` it is read-only: cancel, approve and decline answer 403.

The three routes that change something also refuse a browser request made for a
page on another origin. The provider-private form of a message, which can hold
the model's own reasoning, is removed from everything it serves. The stream
reads the journal by revision, so it does not skip a change and works whichever
process executes the run; a client reconnects with `Last-Event-ID` and loses
nothing. Under the default router the request's context ends after 30 seconds,
so a service that wants one connection to last builds its router with the
timeout turned off.

## What a store keeps

The two Postgres stores treat a character a column cannot hold differently.

`agent/pg`, like `agent.MemoryStore`, keeps U+FFFD in place of a NUL or a byte
that is not UTF-8 in a string it only records: a tool's result, a run's input,
output and error, a reason, a rule, and the strings in metadata and an action.
So no journal write fails for what a model or a tool wrote. A string it
compares (an agent's name, a start key, an owner, a tool effect's key) is
refused instead. Inside the JSON it keeps as written, a call's arguments and a
turn in its provider's form, a NUL stays as the escape `\u0000` and only a byte
that is not UTF-8 is replaced.

`policy/pg` refuses to record a decision that holds a NUL anywhere, or a kind,
target, rule or version that is not UTF-8, and the action is then not allowed.
Only a byte that is not UTF-8 inside a rule name or an attribute, which it
writes as JSON, is kept as U+FFFD.
