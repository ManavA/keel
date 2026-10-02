# Agents on Keel: plan for the first milestone

`DESIGN.md` beside this file is the specification. This file splits it into
tasks that separate implementers can do at the same time, each in its own
git worktree. Section numbers below refer to `DESIGN.md`.

The milestone is done when `examples/agent` does the five things in section
2 and every task here is merged.

## Stages

A task starts once every task it depends on is merged into the integration
branch. Tasks in one stage have no dependency on each other and run at the
same time.

```
Stage 0   T0  contracts

Stage 1   L1  llm: scripted model       L2  llm: wrappers
          L3  llm/anthropic             L4  llm/openai
          P1  policy                    A1  agent: memory store and test kit
          H1  httpx: server-sent events

Stage 2   A2  agent/pg                  A3  agent: engine and planner
          A4  agent: lease and tools    A5  agent/httpapi
          P2  policy/pg                 X1  app: adapters

Stage 3   A6  agent: executor           C1  Makefile and CI

Stage 4   A7  agent: crash tests        E1  examples/agent: the service
          D3  site: tour stop

Stage 5   E2  examples/agent: evaluation and resume tests

Stage 6   D1  documentation             D2  scaffolder profile
```

| Task | Depends on |
|---|---|
| T0 | Nothing |
| L1, L2, L3, L4 | T0 |
| P1, H1 | Nothing. They start with stage 1 only so the integration branch has one base |
| A1 | T0 |
| A2 | T0, A1 |
| A3 | T0, A1 |
| A4 | T0, A1 |
| A5 | T0, H1 |
| P2 | P1 |
| X1 | T0, L1, P1 |
| A6 | A1, A3, A4 |
| C1 | A2, P2 |
| A7 | A6 |
| E1 | L1, L2, L3, L4, P1, P2, A2, A5, A6, X1 |
| D3 | P1 |
| E2 | E1, A1 |
| D1 | Everything above |
| D2 | E2 |

`llm`, `policy` and `agent` proceed in parallel from stage 1. The HTTP
surface is A5, in stage 2. The example is E1 and E2. Documentation and the
scaffolder are last.

## Rules for every task

**Base and branch.** The integration branch is `ai`, which holds these two
documents. A task branches from `ai` as it stands when the task's stage
opens, works in its own worktree, and is merged back when it is done.

**Files.** A task creates and changes only the files it owns. No file is
owned by two tasks. If a task needs a change to a file it does not own,
including `go.mod` and `go.sum`, it stops and raises it. The one exception:
a task may fix a defect in a file owned by a task that is already merged,
when its own tests expose the defect, and says so in the commit message.

**No new dependency.** Everything here is built on the standard library and
what `go.mod` already requires (`pgx`, `chi`, `uuid`, `testify`). After a
task, `go mod tidy` changes nothing.

**The design is the interface.** Exported names, signatures, struct fields,
JSON tags and SQL are as `DESIGN.md` writes them. A task that finds the
design wrong or silent raises it; it does not choose differently on its
own, because another task is building against the same text.

**Tests first.** Each task lists the tests to write before the code. Write
them, see them fail, then write the code. For a check that could pass
without exercising what it names, write the case that must fail, as
`CONTRIBUTING.md` asks; the tasks name those cases.

**Style.** As `CONTRIBUTING.md` and `ARCHITECTURE.md`: a package doc comment
that says what the package does, when to use it, and the decisions the
signatures do not show; options structs whose zero value works; errors
wrapped with context, as `fmt.Errorf("agent/pg: claim: %w", err)`;
`context.Context` first; logging through a `*slog.Logger` from the options,
falling back to `slog.Default()`; table-driven tests with testify, each case
named. Nothing in a package or a test carries a domain: no customer, no
product, no real host name.

**Commands.** Every task passes all of these before it is done:

```
gofmt -l .                              # prints nothing
go build ./...                          # or: make build
go vet ./...
go test -race -count=1 ./<owned>/...    # the task names the packages
make lint                               # golangci-lint run ./...
go mod tidy && git diff --exit-code go.mod go.sum
```

A task that touches Postgres also passes:

```
KEEL_REQUIRE_DB=1 go test -race -count=1 ./<owned>/...
make migrations-check
```

`make lint` runs the linters in `.golangci.yml`. The ones that most often
catch new code: `revive` wants a comment on every exported name and a
package comment; `noctx` wants `http.NewRequestWithContext`; `bodyclose`;
`errorlint` wants `errors.Is`, `errors.As` and `%w`; `gosec`; `unparam`.

Two tasks, A3 and A4, write unexported functions whose only caller outside
their tests arrives in A6. If `unused` reports one, mark it
`//nolint:unused // called from execute.go, which lands in A6`, and A6
removes the marks.

**Commit messages.** Imperative, one line, optionally scoped, as
`CONTRIBUTING.md` shows: `add the scripted model to llm`.

**Done**, for every task, means: the commands pass; the listed tests exist
and pass; the exported API is the design's; the package doc is written; no
file outside the task's ownership changed.

---

## Stage 0

### T0. Contracts

The exported types every other task compiles against, and the small helpers
two or more of them share. Transcribed from the design, not designed again.

**Owns**

```
llm/doc.go
llm/llm.go            messages, request, response, Model, Embedder
llm/errors.go         Error, the sentinel errors, Retryable
llm/price.go          Price, Prices, Cost, CostOf
llm/estimate.go       EstimateInputTokens
llm/llm_test.go  llm/errors_test.go  llm/price_test.go  llm/estimate_test.go
llm/internal/sse/sse.go
llm/internal/sse/sse_test.go
agent/doc.go
agent/agent.go        the consumed interfaces, conversation, actions, definitions
agent/journal.go      Run, Step, Approval, filters, the request types, Store
agent/events.go       TopicRuns, the event types, Event
agent/errors.go
agent/agent_test.go  agent/journal_test.go
```

**Consumes** Section 4.2, first block, and `EstimateInputTokens` from the
second with its rule in 4.3. The `sse` block in 4.2. Section 6.2, first
block.

**Tests first**

- `llm`: the JSON of a `Message` holding text, a tool call, a tool result
  and an `Opaque`, compared with a literal; `Usage.Total` and `Add`;
  `Prices.Cost` for a round figure, for one that must round up, and for an
  unlisted model (`ErrNoPrice`); `CostOf` with no `Attempts` and with two;
  `Retryable` over a table: a retryable `*Error`, one that is not, one
  wrapped in `fmt.Errorf`, `context.Canceled`, `context.DeadlineExceeded`,
  `ErrBudgetExceeded`, a plain error; `Error.Error` with and without a
  transport error; `EstimateInputTokens` for an empty request, and that
  adding a message, a tool or a schema never lowers it.
- `sse`: one event; two events; data over several lines joined with a line
  feed; a comment line skipped; an event with no `event:` field; an event
  with `id:`; input cut in the middle of a line across reads; a stream
  ending without the final blank line; an event larger than
  `MaxEventBytes` returning an error.
- `agent`: the JSON of a `Run`, a model `Step`, a tool `Step` and an
  `Approval`, each compared with a literal, since these are what the HTTP
  surface serves; `StepKey`; `Run.Lease`, `Terminal` and `Running` at, before
  and after the expiry; `StepStatus.Done` for each status; `Usage.Add`.

**Packages** `./llm/...`, `./agent/...`

**Done when** the general conditions hold, and every identifier, field and
tag in the two contract blocks is present exactly as written. Nothing else
is added: no engine, no store, no provider.

---

## Stage 1

### L1. `llm`: the scripted model, the hash embedder, `Decode`

**Owns** `llm/scripted.go`, `llm/embed.go`, `llm/decode.go`, and a
`_test.go` beside each.

**Consumes** Section 4.2, second block: `Reply`, `Script`, `Replies`,
`Route`, `ScriptedOptions`, `Scripted`, `HashEmbedder`, `Decode`. Their
behaviour in 4.3.

**Tests first**

- Two `Scripted` built from one script give the same `Response` to the same
  request; one `Scripted` asked the same request twice gives the same
  `Response` twice. This is the property the resume tests stand on.
- The reply is chosen by the number of assistant messages in the request:
  a table of requests with zero, one and two assistant turns.
- A turn past the end of `Replies` returns `ErrScriptExhausted`.
- `Reply.Err` is returned. `Route` picks by `Request.Model` and falls to the
  `""` entry.
- A tool call with no ID gets `call_<turn>_<index>`; `Stop` defaults by
  whether there are tool calls; the default `Usage` is the same on every
  call for the same request and reply.
- `Stream` delivers the text in more than one delta, then the tool calls,
  and returns what `Generate` returns for the same request; an error from
  `fn` stops it and is returned.
- `Requests` returns what was asked, in order, and is safe to call while
  calls are in flight (`-race`).
- `HashEmbedder`: the same text gives the same vector; two texts sharing
  most words are closer than two sharing none; every non-empty vector has
  length 1 within rounding; empty text gives the zero vector; `dims` of 0
  gives 256; `Dimensions` on the request overrides.
- `Decode`: a JSON reply into a struct; a reply with `StopMaxTokens` and
  one with `StopRefusal` are errors; text that is not JSON is an error.

**Packages** `./llm/`

### L2. `llm`: the wrappers

**Owns** `llm/retrying.go`, `llm/fallback.go`, `llm/budget.go`,
`llm/metered.go`, a `_test.go` beside each, and `llm/fake_test.go` holding
the fake model the four tests share.

**Consumes** Section 4.2, second block: `RetryOptions`, `Retrying`,
`FallbackOptions`, `Fallback`, `BudgetOptions`, `Spend`, `Budgeted`,
`CallRecord`, `MeterOptions`, `Metered`. Behaviour in 4.3 and the order of
composition in 4.7. `retry.Do` and `retry.Options` from `retry`.

**Tests first**

- `Retrying`: a retryable failure then success is two calls; a failure that
  is not retryable is one; three retryable failures give up after three,
  and `errors.As` still finds the `*llm.Error`; `RetryAfter` is waited, and
  capped at `MaxRetryAfter`; there is no wait after the last attempt; a
  cancelled context stops it; a stream whose `fn` was called is not
  retried, and one that failed before any delta is.
- `Fallback`: the first model answering means the second is never called;
  the first failing means the second answers and `Response.Model` is the
  second's; the second receives a request with `Model` empty; a cancelled
  context and `ErrBudgetExceeded` do not move on; `ShouldFallback`
  overrides; a refusal moves on only with `OnRefusal`; all failing returns
  an error in which `errors.As` finds each `*llm.Error`; no models is an
  error from `NewFallback`.
- `Budgeted`: a call that fits goes through and `Spent` rises by the real
  usage; a call whose worst case does not fit returns `ErrBudgetExceeded`
  and the inner model is not called; the same by tokens and by cost; a cost
  limit without `Prices` is an error from `NewBudgeted`; a request for an
  unpriced model is `ErrNoPrice`; a failed call adds nothing. The case that
  must fail: fifty concurrent calls against a budget that fits ten, under
  `-race`, never let `Spent` pass the limit; the test first asserts that
  the same load with no budget does pass it, so the fixture is shown to be
  able to.
- `Metered`: one `CallRecord` per call, including failed ones; totals;
  `Priced` false and tokens still counted for an unpriced model; `Now` is
  used for `At` and `Duration`.

**Packages** `./llm/`

### L3. `llm/anthropic`

**Owns** everything under `llm/anthropic/`.

**Consumes** Section 4.4 whole. `llm.Model` and the contract types. The
event reader in `llm/internal/sse`. The addresses in section 3 are the
source for anything 4.4 does not state; record in the package doc the
addresses and the date they were read.

**Tests first**, each against an `httptest.Server`:

- One case per row of the request table in 4.4, asserting the JSON body the
  server received: model resolution in its three forms; `max_tokens` in its
  three forms for `Generate` and for `Stream`; `system` omitted when empty;
  each message kind; an assistant turn with `Opaque` from this provider
  sent unchanged, and one with `Opaque` from another provider rebuilt from
  text and tool calls; a `Malformed` call sent with `{}`; tool results in
  one user message with nothing else in it; `tools`, with and without a
  schema and `strict`; `tool_choice` for none and absent for auto;
  `output_config.format`; `output_config.effort`; `temperature` absent when
  nil; `stop_sequences`; `fallbacks` and its beta header; `Betas` joined;
  `Extra` merged. Headers: `x-api-key`, `anthropic-version`,
  `content-type`.
- One case per row of the response table, from canned bodies under
  `testdata/` taken from the reference's examples: text; a tool call whose
  `input` keys are not in alphabetical order, read back in the order sent;
  a reply with a `thinking` block whose text is empty, kept in `Opaque`;
  each stop reason; a refusal with and without a category; every usage
  field; `usage.iterations` into `Attempts`; a reply holding a `fallback`
  block, with the echo rule applied.
- The case that must fail when the mapping is wrong: send a reply holding a
  `thinking` block back as the next request's assistant turn and assert the
  server receives a content array equal, as JSON values, to the one it
  sent.
- Streaming, replaying recorded event sequences: text only; text then a
  tool call split across deltas; a thinking block with no text and a
  signature; a `ping`; an unknown event type ignored; an unknown delta type
  leaving `Opaque` nil and the reply usable; an `error` event mid-stream;
  `fn` returning an error; cumulative usage from `message_delta`. For each,
  the `Response` equals what `Generate` builds from the equivalent body.
- Errors: each status in 4.4 with its `Retryable`; `retry-after`;
  `request-id`; a 429 with and without `retry-after`; a body that is not
  JSON; a transport failure; a cancelled context is not retryable.
- `New` refuses an empty `APIKey`.
- `anthropic_live_test.go` behind `//go:build live`, skipping without
  `ANTHROPIC_API_KEY`: one `Generate` with a tool, one `Stream`, against
  `claude-haiku-4-5-20251001`. Run with
  `go test -tags=live ./llm/anthropic/ -run Live`. It is not part of the
  commands above and never runs in CI.

**Packages** `./llm/anthropic/`

### L4. `llm/openai`

**Owns** everything under `llm/openai/`.

**Consumes** Section 4.5 whole. `llm.Model`, `llm.Embedder`, the contract
types, `llm/internal/sse`. Section 3 for anything 4.5 does not state, with
the addresses and date in the package doc.

**Tests first**, each against an `httptest.Server`:

- One case per row of the request table in 4.5: model resolution; the
  system prompt as the first message under `SystemRole`, and absent when
  empty; each message kind; `arguments` as a string; one `tool` message per
  result; an error result prefixed `ERROR: `; `tools`; `tool_choice`;
  `response_format` with the default name; `max_completion_tokens`, and
  `max_tokens` with `LegacyMaxTokens`, and neither when zero;
  `temperature`; `reasoning_effort`; `stop`; `Authorization` present with a
  key and absent without; `Header` added.
- One case per row of the response table: text; a tool call; arguments
  that are not valid JSON giving `Malformed` with `Input` a JSON string;
  `refusal`; each `finish_reason`; usage with and without the details
  objects, including cached tokens never taking `InputTokens` below zero.
- Streaming: text; a tool call whose `id` and name arrive on the first
  chunk and whose arguments arrive in pieces; two tool calls by `index`;
  the usage chunk with empty `choices`; no usage chunk at all; `[DONE]`;
  `fn` returning an error. For each, the `Response` equals what `Generate`
  builds from the equivalent body.
- Embeddings: one input; several inputs returned out of order and put back
  in order by `index`; `dimensions` sent when set; the embedding model
  resolved from the request, then `EmbeddingModel`.
- Errors: each status in 4.5 with its `Retryable`; a 429 for each code that
  is not retryable; `Retry-After`; `x-request-id`; a body that is not JSON.
- `New` refuses an empty `Model`.
- `openai_live_test.go` behind `//go:build live`, skipping without
  `OPENAI_BASE_URL`, so it can be pointed at a local runtime.

**Packages** `./llm/openai/`

### P1. `policy`

**Owns** `policy/doc.go`, `policy/policy.go` (the types), `policy/decide.go`
(`Decide`, `Group`, matching), `policy/parse.go` (`Parse`, `Validate`,
`Effect`'s text methods), `policy/recorder.go`, `policy/decider.go`, and
`_test.go` files beside them, including `policy/reference_test.go`.

**Consumes** Section 5.2 and 5.3. The TypeScript reference, read only:
`/home/manava/hanaML/web/src/demos/permissions/logic.ts` and
`/home/manava/hanaML/web/test/demos/permissions.test.ts`.

**Tests first**

- `reference_test.go`: every `decide` and `sortActions` case in the
  reference's test file, in the same order, with a helper that builds the
  rule list in the table in 5.2. Expected effects and rule names are those
  of the reference, with `approve` written `Ask`. The file's header names
  the file it mirrors and says a change to either needs the other.
- `Match`, table-driven: each operator; `int`, `int64`, `float64` and
  `json.Number` compared with each other; an absent attribute under each
  operator; `OpExists` true and false; `OpIn`; kinds; target patterns
  including one that matches nothing.
- No match: `Block`, `RuleDefault`, `Index` -1; `Policy.Default` honoured.
- Tie: two matching rules of one effect report the earlier; a later
  stricter rule wins; a later looser rule does not.
- `Matched` lists every matching rule in order.
- `Validate`: one case per refusal in 5.3.
- `Parse`: the JSON in 5.3 parses; `Parse(Marshal(p))` equals `p`; an
  unknown field is an error; `"approve"` decodes as `Ask` and encodes as
  `"ask"`; a `Decision` encodes as `{"decision": …, "rule": …, …}`.
- `Decider`: `NewDecider` refuses an invalid policy; a decision is recorded
  once with the version and the injected time; the zero `Options` records
  in memory. The case that must fail: with a recorder that returns an
  error, `Decide` returns that error and a `Decision` whose `Effect` is not
  `Allow`.
- `MemoryRecorder` under `-race`.

**Packages** `./policy/`

### A1. `agent`: the memory store and the test kit

**Owns** `agent/memory.go`, `agent/memory_test.go`, and everything under
`agent/agenttest/`.

**Consumes** Section 6.2: the `Store` interface, the table of what each
method does, and the `agenttest` block. Section 6.6 for claiming, 6.10 for
`Park` and `DecideApproval`.

`agenttest.RunStoreSuite` is the contract. It is written from the table in
6.2 and from `Store`'s comments, one subtest per sentence, and it is what
A2 must also pass. Write it so that it assumes nothing about the store but
the interface: every time it needs comes from an `agenttest.Clock` it
passes in requests.

**Tests first**

- The suite itself, run against `MemoryStore` from `agent/memory_test.go`:
  - `CreateRun`: stored with `Rev` 1; a second create with the same agent
    and key returns the first with `created` false; the same key under
    another agent creates.
  - `Claim`: oldest first; only the named agents; nothing while a lease is
    live; a lapsed lease is taken, the epoch rises and `Failures` rises; a
    released lease is taken with `Failures` unchanged; `NextAttemptAt` in
    the future hides a run; a waiting or ended run is never claimed; with
    `RunID`, `ErrNotClaimable` in each of those cases.
  - `Heartbeat`: extends the expiry; reports a cancel request; with a stale
    epoch, `ErrLeaseLost`; leaves `Rev` alone.
  - Every method taking a `Lease`, with a stale epoch: `ErrLeaseLost` and
    nothing changed.
  - `BeginModel`, `CompleteModel`: the step, the proposed tool steps with
    their `Turn`, `Call` and `Key`, the run's usage, `ModelCalls`,
    `ActiveMillis`; a second `BeginModel` on a started step counts an
    attempt; a wrong `seq` is `ErrConflict`.
  - `UpdateStep`: each effect in the table; `ErrConflict` for a wrong
    `From`; usage added to step and run; `Failures` reset by a final
    status.
  - `RequestApproval`: step waiting, approval pending with the step's tool,
    arguments and attempt; asked again, the same approval and no change.
  - `Park`: true with a pending approval; true with an unfinished child;
    false with neither; false when cancellation is requested.
  - `DecideApproval`: approved and declined; a waiting run becomes
    runnable; a second decision returns the first with
    `ErrAlreadyDecided`.
  - `ExpireApprovals`: only those past their time; their runs runnable.
  - `Finish`: the fields set; the lease cleared; pending approvals
    cancelled; a waiting parent made runnable; a parent that is not waiting
    left alone.
  - `Yield`: with and without `Failed`.
  - `RequestCancel`: marks; wakes a waiting run; `ErrFinished` for an ended
    one.
  - `Changes`: only what is newer than the revision; everything at 0.
  - `ListRuns` and `ListApprovals`: order, each filter, the limit and its
    cap, the cursor.
  - `ErrNotFound` for an unknown id on every method that takes one.
- `MemoryStore` copies: changing a `Run` or `Step` after storing or reading
  it does not change the store.
- `agenttest.Clock`: `Advance`; safe under `-race`.
- `agenttest.Model`: the reply depends on the request alone; `ByAgent`;
  `Requests`.
- `agenttest.FaultStore`: `KillBefore(n)` fails the nth call without
  reaching the inner store; `KillAfter(n)` reaches it and then fails;
  every later call fails with `ErrKilled`; `Calls` counts.

**Packages** `./agent/...`

### H1. `httpx`: server-sent events

**Owns** `httpx/sse.go`, `httpx/sse_test.go`.

**Consumes** Section 7.

**Tests first**

- The bytes written for: data only; with a type; with an id; with both;
  data of three lines; empty data; `SendJSON`; `Comment`; `Retry` set.
- The three headers are set, and the status is 200.
- An id or type containing a line feed is refused, and nothing is written.
- A response writer that cannot flush gives `ErrStreamUnsupported`, with
  no header written.
- `LastEventID` with and without the header.
- The case that must fail: against a real `httpx.Server` with a 100
  millisecond `WriteTimeout`, a handler that sends an event every 50
  milliseconds for half a second delivers all of them. The same handler
  writing without `NewEventStream` is cut off, and the test asserts that
  too, so the fixture is shown to be able to fail.
- Through `httpx.NewRouter` with its default middleware, events still
  arrive one at a time, not at the end.

**Packages** `./httpx/`

---

## Stage 2

### A2. `agent/pg`

**Owns** everything under `agent/pg/`: `doc.go`, `store.go`,
`migrations.go`, `once.go`, `migrations/001_agent_journal.up.sql`,
`migrations/001_agent_journal.down.sql`, and the tests.

**Consumes** Section 6.3 for the DDL, used as written. Section 6.2 for
`Store` and the table of what each method does. Section 6.6 for the claim
statement and the fenced write. Section 6.10 for the lock order of `Park`
and `DecideApproval`. `pg.Beginner` and `pg.InTx` from `pg`.
`agenttest.RunStoreSuite` from A1.

Every method is one `pg.InTx`. Timestamps come from the request, never from
`now()`.

**Tests first**, with `pg/testdb`, `TestMain` as `idempotency/pg` has it,
and a schema per test as `examples/worker/main_test.go` makes one:

- `agenttest.RunStoreSuite` against `New(pool)`.
- Eight goroutines calling `Claim` for one run: one gets it, seven get nil.
- Eight goroutines calling `Claim` with eight runs: each gets a different
  run.
- A journal write under a lease, then a second `Claim` after the clock
  passes the expiry, then a write under the first lease: `ErrLeaseLost`,
  and the journal unchanged.
- `Park` racing `DecideApproval`, three hundred rounds: afterwards the run
  is never waiting with no pending approval. The case that must fail: the
  same race against a `Park` that checks without first locking the run's
  row, written as a test-only helper, does strand a run within the rounds;
  assert that, so the race is shown to be reachable.
- A message whose `Opaque` holds an object with keys in the order `z`, `a`
  reads back with them in that order.
- `Once`: true the first time and false the second; from two transactions
  at once, one true and one false; a rolled-back transaction leaves the
  key unrecorded.
- `Purge`: removes an ended root run older than the cut with its steps,
  approvals and children; leaves a newer one, a run still in flight, and a
  child whose parent remains.

**Packages** `./agent/pg/`

### A3. `agent`: the engine's operations and the planner

**Owns** `agent/engine.go` (`Engine`, the option defaults, `New`,
`Register`, `Start`, `GetRun`, `ListRuns`, `Timeline`, `Changes`,
`ListApprovals`, `Approve`, `Decline`, `Cancel`, and the unexported
`publish`), `agent/plan.go` (`conversation`, `next`, `action`), and
`agent/engine_test.go`, `agent/plan_test.go`.

**Consumes** Section 6.2 for `Options`, `StartRequest`, the method
signatures and what `Register` and `Start` do. Section 6.4 for
`conversation`. Section 6.5 for `next`, as declared and as its rules read.
Section 6.9 for the budget checks inside `next`. Section 6.12 for events.
`MemoryStore` and `agenttest` from A1.

`Execute`, `Tick` and `Work` are A6's. `Engine` is declared here with the
unexported fields A6 will need: the options with defaults applied, the
store, the clock, the logger, and the registered definitions behind a
mutex.

**Tests first**

- `next`, table-driven, one named case per rule in 6.5 and per row of 6.7.
  Each gives a run, a definition, a journal, approvals and children, and
  the action expected. Among them: an empty journal; a started model step;
  a reply with two proposed calls; one call final and one proposed; a
  waiting step whose approval is pending, approved, declined, expired;
  two waiting steps, one pending and one approved, giving `actRun` for the
  second; everything pending giving `actPark` with each reason; a started
  step for a plain tool, a delegating tool, and an `AtMostOnce` tool with
  and without an approved approval for its attempt; a waiting step whose
  child has and has not ended; a final reply for each `Stop`; cancellation
  requested with work outstanding; each budget spent with work
  outstanding, and spent with the work already done, which completes.
- `conversation`: the golden in 6.15; a reply whose calls are not all
  final contributes no tool message; the fixed result texts of 6.4 appear
  as stored.
- The property the provider depends on: for a journal grown one step at a
  time, each `conversation` has the one before it as a prefix.
- `New` refuses a nil `Model`; the defaults in `Options` are applied.
- `Register`: one case per refusal in 6.2.
- `Start`: `ErrUnknownAgent`; the snapshot has the tools sorted by name and
  the limits filled; `StartRequest.Limits` replaces them; the same key
  returns the same run and publishes once; `EventRunStarted` is published.
- `Approve`, `Decline`, `Cancel`: an empty `by` is refused; each reaches
  the store and publishes; `ErrAlreadyDecided` and `ErrFinished` pass
  through.

**Packages** `./agent/`

### A4. `agent`: the lease keeper and tool invocation

**Owns** `agent/lease.go`, `agent/invoke.go`, `agent/lease_test.go`,
`agent/invoke_test.go`.

**Consumes** Section 6.5, the block declaring `keep`, `keepOptions`,
`errCancelRequested`, `outcome`, `invoke` and `actionFor`. Section 6.4 for
the fixed result texts. Section 6.6 for the heartbeat. `MemoryStore` and
`agenttest` from A1.

These are free functions over the contract types, so this task does not
wait for A3.

**Tests first**

- `keep`: the lease's expiry moves forward while it runs; `stop` ends the
  heartbeats; a second claim of the run cancels the held context with
  cause `ErrLeaseLost`; a cancel request cancels it with cause
  `errCancelRequested`; a store that fails every heartbeat cancels it with
  `ErrLeaseLost` once a whole TTL has passed on the clock, and not before;
  cancelling the parent context ends it.
- `invoke`: a result; an error becomes an error result with its text; an
  error wrapping `ErrTransient` sets `retry` and no result; a panic becomes
  `tool panicked` and is logged; a tool that outlives its timeout becomes
  `timed out after …`; a result over 1 MiB is refused with its size; the
  tool receives the `Invocation` it was given.
- `actionFor`: the default for a plain tool; the default for a delegating
  tool; the tool's own `Action`; the four attributes added where absent and
  left alone where the tool set them.

**Packages** `./agent/`

### A5. `agent/httpapi`

**Owns** everything under `agent/httpapi/`.

**Consumes** Section 6.13 whole. `httpx.JSON`, `httpx.NotFound`,
`httpx.BadRequest`, `httpx.Error`, and `NewEventStream`, `LastEventID` from
H1. The `agent` types from T0.

The tests use a fake `Runs` declared in the test file. The check that
`*agent.Engine` satisfies `Runs` is E2's, since the engine's methods land
in A3 and this task does not wait for them.

**Tests first**

- Each route: the status and the body for success, for an unknown id, and
  for a store error. `GET /runs` with each filter, a limit of 0, 201 and
  `abc` (400), an unknown status (400), and a cursor carried from one page
  to the next. An empty list is `[]`, not `null`.
- Approve, decline and cancel: 403 with no `Actor`, and with one that
  returns `""`; the name `Actor` returns reaches `Runs`; 409 for
  `ErrAlreadyDecided` and `ErrFinished`; a body over 4 KiB is 400; a body
  with an unknown field is 400; no body is accepted.
- No served step carries `opaque`, on the timeline or the stream.
- The stream: 404 before any event for an unknown run; steps and approvals
  before the `run` event; only the `run` event has an id, and it is the
  revision; `Last-Event-ID` resumes after that revision; a finished run
  sends `end` and the handler returns; a comment arrives when nothing
  changes for `Heartbeat`; the handler returns when the client goes away.
- `New` refuses nil `Runs`.

**Packages** `./agent/httpapi/`

### P2. `policy/pg`

**Owns** everything under `policy/pg/`: `doc.go`, `store.go`,
`migrations.go`, `migrations/001_policy_decisions.up.sql`,
`migrations/001_policy_decisions.down.sql`, and the tests.

**Consumes** Section 5.3, second block, and 5.4 for the DDL, used as
written. `policy.Record` and `policy.Recorder` from P1.

**Tests first**, with `pg/testdb`, as `jobs/pg/store_test.go` is set up:

- A record is listed back with its action, attributes of each JSON type,
  decision, matched rules and version.
- `List`: newest first; each filter alone and together; the default limit.
- Twenty concurrent records all land.
- A `policy.Decider` over the store records each decision it makes.

**Packages** `./policy/pg/`

### X1. `app`: the adapters

**Owns** `app/agents.go`, `app/agents_test.go`. Nothing else in `app/`.

**Consumes** Section 8. `llm.Model`, `llm.Prices`, `llm.Scripted` (L1),
`policy.Decider` (P1), `agent.Model`, `agent.Guard` and the `agent` types
(T0).

**Tests first**, without a database:

- `AgentModel`, request: each field in the first table of section 8,
  asserted on what an `llm.Scripted` received; an assistant turn's `Opaque`
  passed through unchanged in both directions; `Effort` and `StrictTools`
  applied; `Output` wrapped with the name `answer`.
- `AgentModel`, response: each `Stop`; `InputTokens` summing cache tokens;
  `CostMicros` from the table, and from `Attempts` when there are some.
- Errors: `ErrBudgetExceeded`, `ErrNoPrice` and an `*llm.Error` that is not
  retryable satisfy `errors.Is(err, agent.ErrPermanent)` and still unwrap
  to themselves; a retryable `*llm.Error` and a plain error do not.
- The case that must fail: with `Prices` set and a reply from a model the
  table lacks, the call is an error wrapping `ErrPermanent`, and is not a
  reply costing zero.
- `AgentGuard`: each effect and its rule come back; the action's kind,
  target and attributes reach the `Decider`; a `Decider` whose recorder
  fails gives an error.
- `app`'s existing tests still pass.

**Packages** `./app/`

---

## Stage 3

### A6. `agent`: the executor and the worker

**Owns** `agent/execute.go` (`Execute` and performing each action),
`agent/work.go` (`Tick`, `Work`), `agent/execute_test.go`,
`agent/work_test.go`.

**Consumes** Section 6.5, the loop and the table of how each action is
performed. Section 6.6 for how an execution ends, the backoff and the
shutdown. Sections 6.9 to 6.12. `next` and `conversation` from A3; `keep`,
`invoke` and `actionFor` from A4; `MemoryStore` and `agenttest` from A1.

**Tests first**, on `MemoryStore`, `agenttest.Clock` and `agenttest.Model`,
one named case each:

- A run with no tools completes with the model's text.
- A tool's result reaches the model in the next request.
- A tool error reaches the model as an error result, and the run goes on.
- `ErrTransient`: the execution ends failed; the next runs the tool again
  with the same `Key` and `Attempt` 2.
- A malformed call and a tool missing from the registered definition each
  get their fixed result without running anything.
- The guard blocks: the step is blocked with the rule; the tool never runs.
- The guard asks: the run parks with `ReasonApproval`; `Execute` again
  parks again and asks nothing new; after `Approve` the tool runs once with
  the arguments in the journal; after `Decline` it does not and the model
  is told who and why.
- A tool marked `Approval` parks though the guard allowed, with
  `RuleToolApproval`.
- Two calls in one reply, the first asking and the second allowed: the
  second runs before the run parks.
- An approval past `ApprovalTTL` is lapsed by `Tick` and declines the call.
- `AtMostOnce`: a started step left by a killed execution is asked about,
  not run; approved, it runs; declined, it gets the fixed result.
- Each budget stops the run with its reason: time, driven by the clock and
  a tool that advances it; cost; tokens; model calls.
- `StopRefusal`, `StopMaxTokens` and `StopContextWindow` fail the run with
  their reasons; `StopPause` calls the model again.
- A guard error ends the execution failed, and the step is still proposed.
- A model error wrapping `ErrPermanent` fails the run at once; any other
  yields with `NextAttemptAt` set, doubling, and at `MaxFailures` fails
  the run.
- Child runs: three delegations in one reply are all created before the
  parent parks with `ReasonChildren`; executing the children and then the
  parent collects their outputs; a child's usage is added to the parent's;
  a failed child is an error result; the child's cost limit is the
  parent's remainder; a delegation past `MaxDepth`, and one to an agent
  not registered, are error results.
- Cancellation: a waiting run is cancelled on its next execution, its
  pending approval cancelled and its children asked to cancel; a run being
  executed stops through the heartbeat and no later step is written.
- `Execute` on a run another engine holds returns `ErrNotClaimable`.
- Events: the types, in order, for a run with one approval.
- `Tick`: claims no more than `Concurrency`; the `Report` counts.
- `Work`: picks up a run started after it began; on cancel, a step in
  flight finishes and is recorded and the run is yielded; with a step that
  outlives `DrainTimeout`, `Work` returns and nothing is written for it.

**Packages** `./agent/...`

### C1. Makefile and CI

**Owns** `Makefile`, `.github/workflows/ci.yml`.

**Consumes** Nothing from the design but the package paths.

**Changes**

- `Makefile`, `test-db`: add `./agent/... ./policy/pg/...` to the packages.
- `Makefile`: a `run-agent` target beside `run-local`, starting the same
  throwaway Postgres and running `./examples/agent` with
  `OPERATOR_TOKEN=demo` and `LEASE_TTL=5s`. It may land before the example
  does; the target is not run by any check.
- `ci.yml`, the step "Database-backed tests": add `./agent/...
  ./policy/pg/...`. `./examples/...` is already there and will cover the
  example.

**Tests first** `make test-db` runs the two new packages and fails with
Docker stopped. `actionlint .github/workflows/ci.yml` passes.

**Done when** `make test-db` and `make migrations-check` pass.

---

## Stage 4

### A7. `agent`: crash, lease and snapshot tests

A task of tests. The code it tests is merged.

**Owns** `agent/crash_test.go`, `agent/takeover_test.go`,
`agent/snapshot_test.go`.

**Consumes** Sections 6.6, 6.7, 6.8 and the crash test in 6.15.
`agenttest.FaultStore`.

**Tests**

- Crash at every point, as 6.15 describes it: the scripted run, the count
  of store calls N, then for every n and both kill flavours, engine A to
  the kill and engine B to the end. The four assertions: the same final
  status and output as the uninterrupted run; each tool key executed once
  or twice, and exactly once when its completed write landed before the
  kill; every model request has every earlier request of that run as a
  prefix, with the same system prompt and tools; `seq` from 1 with no
  gaps.
- The control: over a store wrapper that discards the `started →
  completed` write, the same loop reports a tool executed more than twice.
  The test asserts the report, so a loop that stopped checking would be
  seen.
- Takeover, two engines on one store: no claim while the lease is live; a
  claim after the clock passes `LeaseTTL`; the first engine's next write
  is `ErrLeaseLost` and its execution ends without writing; a clean yield
  frees the lease at once; a run that kills every execution is finished
  `ReasonAbandoned` at `MaxFailures`.
- The snapshot: between two executions of one run, register the agent
  again in a new engine with a different prompt, a tool removed and a tool
  added. The model is sent the prompt and tools the run started with; a
  call to the removed tool gets `tool is not available`; the added tool is
  not offered.

**Packages** `./agent/`

**Done when** the tests pass. If one exposes a defect in A3, A4 or A6, fix
it there and say so in the commit.

### E1. `examples/agent`: the service

**Owns** under `examples/agent/`: `main.go`, `config.go`, `agents.go`,
`documents.go`, `policy.json`, `policy.go`, `model.go`, `script.go`,
`prices.go`, `handlers.go`, `migrations/001_agent_example.up.sql`,
`migrations/001_agent_example.down.sql`, `main_test.go`, `README.md`.

**Consumes** Section 9 whole, and the wiring in section 8. `app`, `config`,
`log`, `httpx`, `events`, `textpolicy`, `llm` with both providers,
`policy`, `policy/pg`, `agent`, `agent/pg`, `agent/httpapi`.

The other examples are the model for shape: `run()` returning an exit code,
`config.Load` with a `Validate`, `config.Redacted` logged at startup,
migrations embedded, `MIGRATE_ON_START`. `examples/fullstack/main.go` is
the nearest.

**Tests first**, in `main_test.go`, with `pg/testdb`, a schema per test,
the scripted model, and `STEP_DELAY` of zero:

- The service is built as `run()` builds it and served with `httptest`.
- `POST /api/batches` without the token is 401; with it, 202 and a run.
- The same `Idempotency-Key` twice gives the same run.
- The run reaches waiting; `GET /agent/approvals` shows one pending
  approval for `send_digest` with the rule that asked; approving it lets
  the run complete; the timeline shows `delete_document` blocked.
- The event stream for the run delivers a `run` event and ends.
- `/readyz` is ready.
- `Config.Validate`: `OPERATOR_TOKEN` missing; an unknown `LLM_PROVIDER`;
  `anthropic` without a key; `openai` without a model.
- The script: for a request as the coordinator sends it at each turn, the
  reply expected, including the document ids taken from the request.

**Packages** `./examples/agent/`

**Done when** the README's six steps work as written when followed by
hand, and the tests pass.

### D3. Site: a tour stop for `policy`

**Owns** `site/tour.html`, `site/tour_test.go`, `site/site_test.go`,
`site/index.html`.

**Consumes** `policy.Policy.Decide` from P1.

**Changes** A seventh stop in the tour, in the form of the other six: what
the package is for in three sentences, a sample taken from the tree, and
its output as comments. The sample builds a three-rule policy and decides
two actions. An executable copy goes in `tour_test.go`, and
`policy.Policy` joins the markers in `TestTourQuotesKeptInSync`. The
overview page's package map gains `llm`, `policy` and `agent`.

**Tests first** The executable copy of the sample, asserting the output
the page quotes.

**Packages** `./site/...`

---

## Stage 5

### E2. `examples/agent`: evaluation and resume tests

The milestone's acceptance.

**Owns** `examples/agent/eval_test.go`.

**Consumes** Section 9, the tests. `agenttest.FaultStore` and
`agenttest.Clock`.

**Tests**

- `TestBatchEval`, as section 9 describes it: the named checks, each
  reported by name when it fails.
- `TestKillAndResume`, as section 9 describes it, with its third engine.
- `TestBlockedActionIsRecorded`.
- `var _ httpapi.Runs = (*agent.Engine)(nil)`.
- The control for the evaluation: the same checks run against a script in
  which the coordinator never delegates must fail the "every document has
  a summary" check. Assert that it does.

**Packages** `./examples/agent/`

**Done when** `go test -count=1 ./examples/agent/` passes with the network
unplugged, and `KEEL_REQUIRE_DB=1` does not skip it.

---

## Stage 6

### D1. Documentation

**Owns** `README.md`, `ARCHITECTURE.md`, `CHANGELOG.md`, `docs/agents.md`.

**Consumes** Sections 1, 6.8, 8 and 10.

**Changes**

- `README.md`: the opening paragraph names agents; the package table gains
  `llm`, `llm/anthropic`, `llm/openai`, `policy`, `policy/pg`, `agent`,
  `agent/pg`, `agent/httpapi`, `agent/agenttest`; the `httpx` row mentions
  event streams; the `app` row mentions the adapters; the `cmd/keel` row
  lists the `agent` profile; the backends table gains `llm` (`Scripted`;
  Anthropic, OpenAI-compatible), `policy` (`MemoryRecorder`; `policy/pg`)
  and `agent` (`MemoryStore`; `agent/pg`).
- `ARCHITECTURE.md`: the layout gains the new directories; the import
  layers gain the rows in section 10; `examples/` lists the example.
- `CHANGELOG.md`, under 0.1.0, the contracts worth knowing: what "exactly
  once" covers; that an unmatched action is blocked; that lease times use
  the engine's clock; that a run keeps the prompt and tools it started
  with; that `llm` ships no prices.
- `docs/agents.md`, a guide in the manner of `docs/testing.md`: the journal
  and what a crash does; making a tool idempotent with `Once`; approvals;
  writing rules; budgets; testing an agent with `agenttest` and
  `llm.Scripted`; running the worker and sizing `LeaseTTL`; what to mount
  the HTTP surface behind.

**Tests first** None of its own. `site/site_test.go` and every package's
tests still pass; every identifier the guide quotes exists, checked by
compiling its samples in a scratch file that is not committed.

**Done when** each statement in the changed documents is true of the
merged code.

### D2. Scaffolder: the `agent` profile

**Owns** `cmd/keel/new.go`, `cmd/keel/new_test.go`, and everything under
`cmd/keel/testdata/agent/`.

**Consumes** `examples/agent` as E1 and E2 leave it.

**Changes** `testdata/agent/` is a copy of `examples/agent`, file for file.
`profiles` gains:

```
name:       agent
example:    agent
root:       testdata/agent
blurb:      durable agents on Postgres: a journaled run that survives a
            kill, waits for approval, and is held to rules
packages:   agent, app, config, events, httpx, llm, log, pg, policy, textpolicy
migrations: agent: 001_agent_journal; policy: 001_policy_decisions;
            001_agent_example
bootEnvs:   OPERATOR_TOKEN
```

and the `//go:embed` line gains `all:testdata/agent`.

**Tests first**

- `TestTemplateMatchesExample` covers the new profile as it stands, by
  ranging over `profiles`. See it fail before the copy exists.
- `TestProfilesDescribePackagesAndMigrations` pins the new entry.
- `TestNewAgentProfileGeneratesAgentShape`, as the worker profile's test.
- `TestAgentProfileBuildsAndBoots`, as `TestWorkerProfileBuildsAndBoots`:
  generate, point the `replace` at the checkout, build, start against the
  shared database with `OPERATOR_TOKEN` set, post a batch, and poll until
  `agent_runs` holds a waiting run.
- `TestRunNewUsage` shows the profile.

**Packages** `./cmd/keel/`
