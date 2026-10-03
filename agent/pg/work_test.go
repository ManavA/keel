package pg_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// The worker loop over this store, on the wall clock. Unlike the executor's
// tests, these run Work itself, with real keepers making real heartbeats, so
// leases, polls and drains are real time. Every TTL here is short and every
// bound generous: a test waits for what it expects for up to waitLimit, and
// none passes because something happened within a tight margin.

// workTool records, per idempotency key, how many invocations have been made
// and how many were ever in flight at once.
type workTool struct {
	mu      sync.Mutex
	calls   map[string][]agent.Invocation
	by      map[string][]string
	running map[string]int
	most    map[string]int
}

func newWorkTool() *workTool {
	return &workTool{
		calls: map[string][]agent.Invocation{}, by: map[string][]string{},
		running: map[string]int{}, most: map[string]int{},
	}
}

// tool is a tool for the engine named worker. body is what it does once
// counted, nil for nothing.
func (w *workTool) tool(name, worker string, body func(context.Context, agent.Invocation) error) agent.Tool {
	return agent.Tool{Name: name, Run: func(ctx context.Context, in agent.Invocation) (string, error) {
		w.mu.Lock()
		w.calls[in.Key] = append(w.calls[in.Key], in)
		w.by[in.Key] = append(w.by[in.Key], worker)
		w.running[in.Key]++
		w.most[in.Key] = max(w.most[in.Key], w.running[in.Key])
		w.mu.Unlock()
		defer func() {
			w.mu.Lock()
			w.running[in.Key]--
			w.mu.Unlock()
		}()
		if body != nil {
			if err := body(ctx, in); err != nil {
				return "", err
			}
		}
		return name + " done by " + worker, nil
	}}
}

func (w *workTool) snapshot() (calls map[string][]agent.Invocation, by map[string][]string, most map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	calls, by, most = map[string][]agent.Invocation{}, map[string][]string{}, map[string]int{}
	for k, v := range w.calls {
		calls[k] = append([]agent.Invocation(nil), v...)
		by[k] = append([]string(nil), w.by[k]...)
		most[k] = w.most[k]
	}
	return calls, by, most
}

// workConfig is one engine of a test.
type workConfig struct {
	name  string
	tool  agent.Tool
	ttl   time.Duration
	beat  time.Duration
	drain time.Duration
	// replies is the clerk's script; by default one call of the tool, then
	// an answer.
	replies []agent.Response
}

// worker is an engine running Work over its own view of the store.
type worker struct {
	engine *agent.Engine
	faults *agenttest.FaultStore
	stop   context.CancelFunc
	done   chan error
}

func startWorker(t *testing.T, store *agentpg.Store, cfg workConfig) *worker {
	t.Helper()
	replies := cfg.replies
	if replies == nil {
		replies = []agent.Response{
			agenttest.Use(agenttest.Call("call-1", cfg.tool.Name, `{}`)),
			agenttest.Say("finished"),
		}
	}
	w := &worker{faults: agenttest.NewFaultStore(store), done: make(chan error, 1)}
	engine, err := agent.New(agent.Options{
		Model:             agenttest.NewModel(agenttest.Replies(replies...)),
		Store:             w.faults,
		WorkerID:          cfg.name,
		LeaseTTL:          cfg.ttl,
		HeartbeatInterval: cfg.beat,
		PollInterval:      20 * time.Millisecond,
		DrainTimeout:      cfg.drain,
		RetryBase:         20 * time.Millisecond,
		RetryMax:          100 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, engine.Register(agent.Definition{Name: "clerk", Tools: []agent.Tool{cfg.tool}}))
	w.engine = engine

	ctx, cancel := context.WithCancel(context.Background())
	w.stop = cancel
	go func() { w.done <- engine.Work(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-w.done:
		case <-time.After(waitLimit):
		}
	})
	return w
}

// stopped cancels the worker and waits for Work to return.
func (w *worker) stopped(t *testing.T) error {
	t.Helper()
	w.stop()
	select {
	case err := <-w.done:
		w.done <- err
		return err
	case <-time.After(waitLimit):
		require.FailNow(t, "Work did not return")
		return nil
	}
}

// ended waits until the run is in one of the statuses that end it, and
// returns it.
func ended(t *testing.T, store *agentpg.Store, id string) agent.Run {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		run, err := store.GetRun(context.Background(), id)
		require.NoError(t, err)
		if run.Status == agent.StatusCompleted || run.Status == agent.StatusFailed || run.Status == agent.StatusCancelled {
			return run
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "the run did not end", "run %s is %s", id, run.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func receive(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitLimit):
		require.FailNow(t, "timed out waiting", what)
	}
}

func startClerk(t *testing.T, engine *agent.Engine, input string) agent.Run {
	t.Helper()
	run, err := engine.Start(context.Background(), agent.StartRequest{Agent: "clerk", Input: input})
	require.NoError(t, err)
	return run
}

func TestWorkOverPostgres_PicksUpARunStartedAfterItBegan(t *testing.T) {
	store := agentpg.New(newDatabase(t).pool(t))
	tools := newWorkTool()
	w := startWorker(t, store, workConfig{
		name: workerA, tool: tools.tool("lookup", workerA, nil), ttl: 5 * time.Second, beat: time.Second, drain: time.Second,
	})
	// Work has looked and found nothing at least once.
	time.Sleep(100 * time.Millisecond)

	started := startClerk(t, w.engine, "look it up")
	got := ended(t, store, started.ID)

	assert.Equal(t, agent.StatusCompleted, got.Status)
	assert.Equal(t, "finished", got.Output)
	assert.Empty(t, got.LeaseOwner)
	calls, _, _ := tools.snapshot()
	assert.Len(t, calls, 1)
	require.ErrorIs(t, w.stopped(t), context.Canceled)
}

func TestWorkOverPostgres_TwoEnginesRunEachToolCallOnceAtATime(t *testing.T) {
	store := agentpg.New(newDatabase(t).pool(t))
	tools := newWorkTool()
	slow := func(ctx context.Context, _ agent.Invocation) error {
		select {
		case <-time.After(30 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	replies := []agent.Response{
		agenttest.Use(agenttest.Call("call-1", "lookup", `{"n":1}`), agenttest.Call("call-2", "lookup", `{"n":2}`)),
		agenttest.Use(agenttest.Call("call-3", "lookup", `{"n":3}`)),
		agenttest.Say("finished"),
	}
	var workers []*worker
	for _, name := range []string{workerA, workerB} {
		workers = append(workers, startWorker(t, store, workConfig{
			name: name, tool: tools.tool("lookup", name, slow), replies: replies,
			ttl: 10 * time.Second, beat: time.Second, drain: 5 * time.Second,
		}))
	}

	const runs = 12
	var ids []string
	for i := range runs {
		ids = append(ids, startClerk(t, workers[i%2].engine, fmt.Sprintf("batch %d", i)).ID)
	}
	for _, id := range ids {
		got := ended(t, store, id)
		assert.Equal(t, agent.StatusCompleted, got.Status, "run %s", id)
		assert.Equal(t, "finished", got.Output, "run %s", id)
		assert.Zero(t, got.Failures, "run %s", id)
	}

	calls, by, most := tools.snapshot()
	assert.Len(t, calls, runs*3, "one key for each call of each run")
	workedBy := map[string]bool{}
	for key, ins := range calls {
		assert.Len(t, ins, 1, "key %s is invoked once", key)
		assert.Equal(t, 1, most[key], "key %s is never in flight on two engines", key)
		workedBy[by[key][0]] = true
	}
	// Which engine took which run is up to the scheduler; that both stores
	// were used is not asserted, since one engine may take every run.
	assert.NotEmpty(t, workedBy)
	for _, w := range workers {
		require.ErrorIs(t, w.stopped(t), context.Canceled)
	}
}

func TestWorkOverPostgres_HeartbeatsKeepALeaseAcrossSeveralTTLs(t *testing.T) {
	store := agentpg.New(newDatabase(t).pool(t))
	tools := newWorkTool()
	const ttl = 2 * time.Second
	began := make(chan struct{})
	holding := func(ctx context.Context, _ agent.Invocation) error {
		close(began)
		select {
		case <-time.After(4 * ttl):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	first := startWorker(t, store, workConfig{
		name: workerA, tool: tools.tool("slow", workerA, holding), ttl: ttl, beat: ttl / 10, drain: time.Second,
	})
	started := startClerk(t, first.engine, "take your time")
	receive(t, began, "the tool to begin")

	// The second engine is looking for work all the while the tool runs.
	second := startWorker(t, store, workConfig{
		name: workerB, tool: tools.tool("slow", workerB, nil), ttl: ttl, beat: ttl / 10, drain: time.Second,
	})
	got := ended(t, store, started.ID)

	assert.Equal(t, agent.StatusCompleted, got.Status)
	assert.Zero(t, got.Failures)
	calls, by, _ := tools.snapshot()
	require.Len(t, calls, 1)
	for key := range calls {
		assert.Equal(t, []string{workerA}, by[key], "the run is never taken from its holder")
	}
	steps, err := store.Steps(context.Background(), started.ID)
	require.NoError(t, err)
	require.Len(t, steps, 3)
	assert.Equal(t, "slow done by "+workerA, steps[1].Result)
	assert.Equal(t, 1, steps[1].Attempts)
	for _, w := range []*worker{first, second} {
		require.ErrorIs(t, w.stopped(t), context.Canceled)
	}
}

func TestWorkOverPostgres_AnEngineWhoseHeartbeatsStopLosesTheRun(t *testing.T) {
	store := agentpg.New(newDatabase(t).pool(t))
	tools := newWorkTool()
	const ttl = time.Second
	began, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	stuck := func(ctx context.Context, in agent.Invocation) error {
		close(began)
		<-ctx.Done()
		// The tool does not stop when told to.
		<-release
		close(returned)
		return nil
	}
	first := startWorker(t, store, workConfig{
		name: workerA, tool: tools.tool("slow", workerA, stuck), ttl: ttl, beat: ttl / 5,
		drain: 100 * time.Millisecond,
	})
	started := startClerk(t, first.engine, "take your time")
	receive(t, began, "the tool to begin")

	// Cancelled mid-tool, the engine stops heartbeating once the drain has
	// run out, and Work returns with the tool still running.
	require.ErrorIs(t, first.stopped(t), context.Canceled)
	before := first.faults.Calls()

	second := startWorker(t, store, workConfig{
		name: workerB, tool: tools.tool("slow", workerB, nil), ttl: ttl, beat: ttl / 5, drain: time.Second,
	})
	got := ended(t, store, started.ID)
	assert.Equal(t, agent.StatusCompleted, got.Status)

	// The first engine's tool returns at last, to nobody.
	close(release)
	receive(t, returned, "the stuck tool to return")
	time.Sleep(time.Second)
	after, err := store.GetRun(context.Background(), started.ID)
	require.NoError(t, err)
	assert.Equal(t, got.Rev, after.Rev, "the first engine writes nothing more")
	assert.Equal(t, before, first.faults.Calls(), "the first engine does not call the store again")

	calls, by, _ := tools.snapshot()
	require.Len(t, calls, 1)
	for key, ins := range calls {
		assert.Equal(t, []string{workerA, workerB}, by[key], "key %s", key)
		require.Len(t, ins, 2)
		assert.Equal(t, 2, ins[1].Attempt)
	}
	steps, err := store.Steps(context.Background(), started.ID)
	require.NoError(t, err)
	require.Len(t, steps, 3)
	assert.Equal(t, "slow done by "+workerB, steps[1].Result)
	require.ErrorIs(t, second.stopped(t), context.Canceled)
}

func TestWorkOverPostgres_OnShutdownAStepInFlightFinishesAndIsRecorded(t *testing.T) {
	store := agentpg.New(newDatabase(t).pool(t))
	tools := newWorkTool()
	began, release := make(chan struct{}), make(chan struct{})
	stepErr := make(chan error, 1)
	slow := func(ctx context.Context, _ agent.Invocation) error {
		close(began)
		<-release
		stepErr <- ctx.Err()
		return nil
	}
	w := startWorker(t, store, workConfig{
		name: workerA, tool: tools.tool("slow", workerA, slow), ttl: 2 * time.Second, beat: 200 * time.Millisecond,
		drain: waitLimit,
	})
	started := startClerk(t, w.engine, "take your time")
	receive(t, began, "the tool to begin")

	w.stop()
	// Let the drain run several TTLs: the keeper holds the lease meanwhile.
	time.Sleep(3 * time.Second)
	select {
	case err := <-w.done:
		require.FailNow(t, "Work returned with a step in flight", "%v", err)
	default:
	}
	close(release)
	require.ErrorIs(t, w.stopped(t), context.Canceled)

	require.NoError(t, <-stepErr, "the step in flight is not cancelled with the worker")
	got, err := store.GetRun(context.Background(), started.ID)
	require.NoError(t, err)
	assert.Equal(t, agent.StatusRunnable, got.Status)
	assert.Empty(t, got.LeaseOwner, "the run is given back")
	assert.Zero(t, got.Failures)
	steps, err := store.Steps(context.Background(), started.ID)
	require.NoError(t, err)
	require.Len(t, steps, 2, "no other step is begun")
	assert.Equal(t, agent.StepCompleted, steps[1].Status)
	assert.Equal(t, "slow done by "+workerA, steps[1].Result)
}

func TestWorkOverPostgres_OnShutdownAStepThatOutlivesTheDrainIsLeft(t *testing.T) {
	store := agentpg.New(newDatabase(t).pool(t))
	tools := newWorkTool()
	began, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cause := make(chan error, 1)
	stuck := func(ctx context.Context, _ agent.Invocation) error {
		close(began)
		<-ctx.Done()
		cause <- context.Cause(ctx)
		<-release
		close(returned)
		return nil
	}
	w := startWorker(t, store, workConfig{
		name: workerA, tool: tools.tool("slow", workerA, stuck), ttl: 10 * time.Second, beat: time.Second,
		drain: 200 * time.Millisecond,
	})
	started := startClerk(t, w.engine, "take your time")
	receive(t, began, "the tool to begin")
	before, err := store.GetRun(context.Background(), started.ID)
	require.NoError(t, err)

	require.ErrorIs(t, w.stopped(t), context.Canceled)
	require.Error(t, <-cause, "the step is cancelled once the drain has run out")
	got, err := store.GetRun(context.Background(), started.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Rev, got.Rev, "nothing is written for the step")
	assert.Equal(t, workerA, got.LeaseOwner, "the lease is left to lapse")

	close(release)
	receive(t, returned, "the stuck tool to return")
	time.Sleep(time.Second)
	got, err = store.GetRun(context.Background(), started.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Rev, got.Rev, "nor when the tool returns at last")
	steps, err := store.Steps(context.Background(), started.ID)
	require.NoError(t, err)
	require.Len(t, steps, 2)
	assert.Equal(t, agent.StepStarted, steps[1].Status)
}
