package agenttest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ManavA/keel/agent"
)

// ErrKilled is what a FaultStore returns once it has been killed.
var ErrKilled = errors.New("agenttest: store killed")

// ErrFault is what a FaultStore returns for a call that FailBefore or
// FailAfter chose. The store goes on working after it.
var ErrFault = errors.New("agenttest: store fault")

// The Store methods, as FailBefore and FailAfter name them.
const (
	opCreateRun       = "CreateRun"
	opGetRun          = "GetRun"
	opListRuns        = "ListRuns"
	opClaim           = "Claim"
	opHeartbeat       = "Heartbeat"
	opYield           = "Yield"
	opPark            = "Park"
	opFinish          = "Finish"
	opSteps           = "Steps"
	opBeginModel      = "BeginModel"
	opCompleteModel   = "CompleteModel"
	opUpdateStep      = "UpdateStep"
	opRequestApproval = "RequestApproval"
	opGetApproval     = "GetApproval"
	opListApprovals   = "ListApprovals"
	opDecideApproval  = "DecideApproval"
	opExpireApprovals = "ExpireApprovals"
	opRequestCancel   = "RequestCancel"
	opChanges         = "Changes"
)

var storeOps = []string{
	opCreateRun, opGetRun, opListRuns, opClaim, opHeartbeat, opYield, opPark, opFinish,
	opSteps, opBeginModel, opCompleteModel, opUpdateStep, opRequestApproval,
	opGetApproval, opListApprovals, opDecideApproval, opExpireApprovals, opRequestCancel, opChanges,
}

// FaultStore wraps a Store so a test can stop a process at a chosen store
// call, the way a crash would. Until a fault is asked for it is the store it
// wraps. It is safe for concurrent use.
//
// A call that fails returns no part of the store's answer, whether or not it
// reached the store.
type FaultStore struct {
	inner agent.Store

	mu    sync.Mutex
	calls int
	// killed is set once a call has failed as killed, or by Kill.
	killed bool
	// before is the number of the first call that fails without reaching the
	// store, and after the number of the call that reaches it and then
	// fails. Zero is not set.
	before, after int
	// failBefore and failAfter count the faults left for each method.
	failBefore, failAfter map[string]int
}

// NewFaultStore wraps inner.
func NewFaultStore(inner agent.Store) *FaultStore {
	return &FaultStore{
		inner:      inner,
		failBefore: map[string]int{},
		failAfter:  map[string]int{},
	}
}

// Kill makes every later call fail with ErrKilled without reaching the
// store.
func (f *FaultStore) Kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = true
}

// KillBefore kills the store as its nth call arrives, so that call and
// every later one fail and none of them reaches the store. n counts from 1,
// over every call since the store was built; an n already passed kills the
// store at its next call.
func (f *FaultStore) KillBefore(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n <= f.calls {
		f.killed = true
		return
	}
	f.before = n
}

// KillAfter lets the nth call reach the store, then fails it and every
// later one: the write landed and the caller never learned of it. n counts
// as it does for KillBefore.
func (f *FaultStore) KillAfter(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n <= f.calls {
		f.killed = true
		return
	}
	f.after = n
}

// FailBefore makes the next times calls of op fail with ErrFault without
// reaching the store, and leaves the store working: other methods, and op
// itself once the count is spent, go through. op is the name of a Store
// method, as "CompleteModel"; any other name panics, so that a misspelt one
// is not a fault that silently never happens.
func (f *FaultStore) FailBefore(op string, times int) {
	f.addFaults(f.failBefore, op, times)
}

// FailAfter lets the next times calls of op reach the store and then fails
// each with ErrFault: the write landed, the caller was told it did not, and
// the store goes on working. op is as for FailBefore, and a fault asked for
// with FailBefore is spent first.
func (f *FaultStore) FailAfter(op string, times int) {
	f.addFaults(f.failAfter, op, times)
}

func (f *FaultStore) addFaults(faults map[string]int, op string, times int) {
	if !slices.Contains(storeOps, op) {
		panic(fmt.Sprintf("agenttest: %q is not a Store method", op))
	}
	if times < 1 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	faults[op] += times
}

// Calls reports how many calls have arrived, those that failed included.
func (f *FaultStore) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// arrive counts a call of op and decides its fate. A non-nil err fails the
// call without reaching the store. Otherwise the call reaches the store, and
// a non-nil after is what it then fails with.
func (f *FaultStore) arrive(op string) (after, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	switch {
	case f.killed:
		return nil, ErrKilled
	case f.before != 0 && f.calls >= f.before:
		f.killed = true
		return nil, ErrKilled
	case f.calls == f.after:
		// Killed from here on, though this call is still on its way to the
		// store: a call that arrives while it is there fails too.
		f.killed = true
		return ErrKilled, nil
	case f.failBefore[op] > 0:
		f.failBefore[op]--
		return nil, ErrFault
	case f.failAfter[op] > 0:
		f.failAfter[op]--
		return ErrFault, nil
	}
	return nil, nil
}

// do runs call as the store call op, with whatever fault is due.
func do[T any](f *FaultStore, op string, call func() (T, error)) (T, error) {
	var zero T
	after, err := f.arrive(op)
	if err != nil {
		return zero, err
	}
	got, err := call()
	if after != nil {
		return zero, after
	}
	return got, err
}

// doErr is do for a call that returns only an error.
func doErr(f *FaultStore, op string, call func() error) error {
	_, err := do(f, op, func() (struct{}, error) { return struct{}{}, call() })
	return err
}

// CreateRun implements agent.Store.
func (f *FaultStore) CreateRun(ctx context.Context, run agent.Run) (agent.Run, bool, error) {
	type result struct {
		run     agent.Run
		created bool
	}
	got, err := do(f, opCreateRun, func() (result, error) {
		stored, created, err := f.inner.CreateRun(ctx, run)
		return result{stored, created}, err
	})
	return got.run, got.created, err
}

// GetRun implements agent.Store.
func (f *FaultStore) GetRun(ctx context.Context, id string) (agent.Run, error) {
	return do(f, opGetRun, func() (agent.Run, error) { return f.inner.GetRun(ctx, id) })
}

// ListRuns implements agent.Store.
func (f *FaultStore) ListRuns(ctx context.Context, filter agent.RunFilter) ([]agent.Run, error) {
	return do(f, opListRuns, func() ([]agent.Run, error) { return f.inner.ListRuns(ctx, filter) })
}

// Claim implements agent.Store.
func (f *FaultStore) Claim(ctx context.Context, req agent.ClaimRequest) (*agent.Run, error) {
	return do(f, opClaim, func() (*agent.Run, error) { return f.inner.Claim(ctx, req) })
}

// Heartbeat implements agent.Store.
func (f *FaultStore) Heartbeat(ctx context.Context, lease agent.Lease, now time.Time, ttl time.Duration) (bool, error) {
	return do(f, opHeartbeat, func() (bool, error) { return f.inner.Heartbeat(ctx, lease, now, ttl) })
}

// Yield implements agent.Store.
func (f *FaultStore) Yield(ctx context.Context, lease agent.Lease, req agent.YieldRequest) error {
	return doErr(f, opYield, func() error { return f.inner.Yield(ctx, lease, req) })
}

// Park implements agent.Store.
func (f *FaultStore) Park(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error) {
	return do(f, opPark, func() (bool, error) { return f.inner.Park(ctx, lease, req) })
}

// Finish implements agent.Store.
func (f *FaultStore) Finish(ctx context.Context, lease agent.Lease, req agent.FinishRequest) error {
	return doErr(f, opFinish, func() error { return f.inner.Finish(ctx, lease, req) })
}

// Steps implements agent.Store.
func (f *FaultStore) Steps(ctx context.Context, runID string) ([]agent.Step, error) {
	return do(f, opSteps, func() ([]agent.Step, error) { return f.inner.Steps(ctx, runID) })
}

// BeginModel implements agent.Store.
func (f *FaultStore) BeginModel(ctx context.Context, lease agent.Lease, seq int, now time.Time) error {
	return doErr(f, opBeginModel, func() error { return f.inner.BeginModel(ctx, lease, seq, now) })
}

// CompleteModel implements agent.Store.
func (f *FaultStore) CompleteModel(ctx context.Context, lease agent.Lease, req agent.CompleteModelRequest) error {
	return doErr(f, opCompleteModel, func() error { return f.inner.CompleteModel(ctx, lease, req) })
}

// UpdateStep implements agent.Store.
func (f *FaultStore) UpdateStep(ctx context.Context, lease agent.Lease, req agent.StepUpdate) error {
	return doErr(f, opUpdateStep, func() error { return f.inner.UpdateStep(ctx, lease, req) })
}

// RequestApproval implements agent.Store.
func (f *FaultStore) RequestApproval(ctx context.Context, lease agent.Lease, req agent.ApprovalRequest) (agent.Approval, error) {
	return do(f, opRequestApproval, func() (agent.Approval, error) { return f.inner.RequestApproval(ctx, lease, req) })
}

// GetApproval implements agent.Store.
func (f *FaultStore) GetApproval(ctx context.Context, id string) (agent.Approval, error) {
	return do(f, opGetApproval, func() (agent.Approval, error) { return f.inner.GetApproval(ctx, id) })
}

// ListApprovals implements agent.Store.
func (f *FaultStore) ListApprovals(ctx context.Context, filter agent.ApprovalFilter) ([]agent.Approval, error) {
	return do(f, opListApprovals, func() ([]agent.Approval, error) { return f.inner.ListApprovals(ctx, filter) })
}

// DecideApproval implements agent.Store.
func (f *FaultStore) DecideApproval(ctx context.Context, req agent.DecideRequest) (agent.Approval, error) {
	return do(f, opDecideApproval, func() (agent.Approval, error) { return f.inner.DecideApproval(ctx, req) })
}

// ExpireApprovals implements agent.Store.
func (f *FaultStore) ExpireApprovals(ctx context.Context, now time.Time) (int, error) {
	return do(f, opExpireApprovals, func() (int, error) { return f.inner.ExpireApprovals(ctx, now) })
}

// RequestCancel implements agent.Store.
func (f *FaultStore) RequestCancel(ctx context.Context, req agent.CancelRequest) error {
	return doErr(f, opRequestCancel, func() error { return f.inner.RequestCancel(ctx, req) })
}

// Changes implements agent.Store.
func (f *FaultStore) Changes(ctx context.Context, runID string, since int64) (agent.Changes, error) {
	return do(f, opChanges, func() (agent.Changes, error) { return f.inner.Changes(ctx, runID, since) })
}

var _ agent.Store = (*FaultStore)(nil)
