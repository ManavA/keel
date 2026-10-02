package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ManavA/keel/agent/internal/storerule"
)

// maxListLimit is the most a listing returns, whatever it is asked for.
const maxListLimit = storerule.MaxListLimit

// isUUID reports whether id is one a store keeps: a UUID in its canonical
// form.
func isUUID(id string) bool { return storerule.IsUUID(id) }

// MemoryStore is the in-process Store. Its runs last as long as the process;
// use agent/pg for runs that must outlive it.
//
// The whole contract sits behind one mutex, so each method is one atomic
// step, as each is one transaction in Postgres. Everything given to the
// store and everything it returns is copied: nothing a caller holds reaches
// into it. Two Engines over one MemoryStore therefore behave as two
// processes over one database, which is how takeover is tested without one.
//
// Like every Store it never reads a clock. What it refuses and how values
// read back are as agent/pg has them, so that a test passing on one passes
// on the other: an id must be a UUID in its canonical form, a child's parent
// must exist and be one level above it, and metadata and an action's
// attributes come back as a JSON round trip gives them.
//
// That holds for the strings a database cannot keep too: one with a NUL
// character in it, or a byte that is not UTF-8. A string the store only
// records is kept with each such character as the replacement character,
// U+FFFD: a tool's result, a run's input, output and error, a reason, a
// rule, the name of a tool or a model, who decided or cancelled and why, and
// the strings inside metadata and an action. So nothing a model or a tool
// wrote can make a journal write fail. Inside a message and a definition,
// which are kept as JSON, a NUL is kept and only the byte that is not UTF-8
// is replaced. A string the store compares names something and is refused
// before the store is looked at: an agent's name, a start key and an owner,
// as an id is. Raw JSON that is not JSON, or not UTF-8, is refused the same
// way.
//
// Every method makes its checks in one order. First what the request alone
// shows to be wrong, before the store is looked at; then whether the run
// exists; then, for a method that takes a Lease, whether the lease is the
// run's; and only then the state of the run, its steps and its approvals.
type MemoryStore struct {
	mu   sync.Mutex
	runs map[string]*memoryRun
	// order holds the runs as they arrived.
	order []*memoryRun
	// keys maps an agent and a start key to the run that has them.
	keys      map[startKey]*memoryRun
	approvals map[string]*memoryApproval
}

type startKey struct{ agent, key string }

// memoryRun is a run with its journal, its approvals and its children.
type memoryRun struct {
	run Run
	// steps is the journal: steps[i] has Seq i+1.
	steps     []*Step
	approvals []*memoryApproval
	children  []*memoryRun
}

type memoryApproval struct {
	approval Approval
	run      *memoryRun
}

// NewMemoryStore builds an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		runs:      map[string]*memoryRun{},
		keys:      map[startKey]*memoryRun{},
		approvals: map[string]*memoryApproval{},
	}
}

var _ Store = (*MemoryStore)(nil)

// CreateRun implements Store.
func (s *MemoryStore) CreateRun(_ context.Context, run Run) (Run, bool, error) {
	if !isUUID(run.ID) {
		return Run{}, false, fmt.Errorf("agent: create run: id %q is not a UUID", run.ID)
	}
	if run.ParentID != "" && !isUUID(run.ParentID) {
		return Run{}, false, fmt.Errorf("agent: create run: parent id %q is not a UUID", run.ParentID)
	}
	if run.Status != StatusRunnable {
		return Run{}, false, fmt.Errorf("agent: create run: status is %q, not %q", run.Status, StatusRunnable)
	}
	if !storerule.Comparable(run.Agent) || !storerule.Comparable(run.Key) || !storerule.Comparable(run.LeaseOwner) {
		return Run{}, false, fmt.Errorf("agent: create run: the agent %q, the key %q or the owner %q holds a character no store keeps",
			run.Agent, run.Key, run.LeaseOwner)
	}
	definition, err := storedSnapshot(run.Definition)
	if err != nil {
		return Run{}, false, fmt.Errorf("agent: create run: definition: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := startKey{agent: run.Agent, key: run.Key}
	if run.Key != "" {
		if first, ok := s.keys[key]; ok {
			return cloneRun(first.run), false, nil
		}
	}
	if _, ok := s.runs[run.ID]; ok {
		return Run{}, false, fmt.Errorf("agent: create run: id %q is already in use", run.ID)
	}

	var parent *memoryRun
	if run.ParentID != "" {
		named, ok := s.runs[run.ParentID]
		if !ok {
			return Run{}, false, fmt.Errorf("agent: create run: parent %s does not exist", run.ParentID)
		}
		parent = named
	}
	// A child is one level below its parent, and a run nobody started is at
	// none. The order rows are locked in, children before parents, is taken
	// from this.
	if parent == nil && run.Depth != 0 {
		return Run{}, false, fmt.Errorf("agent: create run: depth is %d for a run no other started", run.Depth)
	}
	if parent != nil && run.Depth != parent.run.Depth+1 {
		return Run{}, false, fmt.Errorf("agent: create run: depth is %d under a parent at depth %d", run.Depth, parent.run.Depth)
	}

	r := &memoryRun{run: cloneRun(run)}
	r.run.Rev = 1
	r.run.Definition = definition
	r.run.Metadata = storerule.Metadata(run.Metadata)
	r.run.CreatedAt, r.run.UpdatedAt = storerule.Instant(run.CreatedAt), storerule.Instant(run.UpdatedAt)
	r.run.LeaseExpiresAt, r.run.NextAttemptAt = storerule.InstantPtr(run.LeaseExpiresAt), storerule.InstantPtr(run.NextAttemptAt)
	r.run.FinishedAt = storerule.InstantPtr(run.FinishedAt)
	r.run.Reason, r.run.Input, r.run.Output, r.run.Error = storerule.Kept(run.Reason), storerule.Kept(run.Input), storerule.Kept(run.Output), storerule.Kept(run.Error)
	r.run.CancelBy, r.run.CancelReason = storerule.Kept(run.CancelBy), storerule.Kept(run.CancelReason)
	s.runs[run.ID] = r
	s.order = append(s.order, r)
	if run.Key != "" {
		s.keys[key] = r
	}
	if parent != nil {
		parent.children = append(parent.children, r)
	}
	return cloneRun(r.run), true, nil
}

// GetRun implements Store.
func (s *MemoryStore) GetRun(_ context.Context, id string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.runs[id]
	if !ok {
		return Run{}, ErrNotFound
	}
	return cloneRun(r.run), nil
}

// ListRuns implements Store.
func (s *MemoryStore) ListRuns(_ context.Context, f RunFilter) ([]Run, error) {
	before, err := listCursor(f.Before)
	if err != nil {
		return nil, fmt.Errorf("agent: list runs: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var found []*memoryRun
	for _, r := range s.order {
		switch {
		case f.Status != "" && r.run.Status != f.Status:
		case f.Agent != "" && r.run.Agent != f.Agent:
		case f.ParentID != "" && r.run.ParentID != f.ParentID:
		case before != nil && !olderThan(r.run, *before):
		default:
			found = append(found, r)
		}
	}
	slices.SortFunc(found, func(a, b *memoryRun) int {
		return cmp.Or(b.run.CreatedAt.Compare(a.run.CreatedAt), cmp.Compare(b.run.ID, a.run.ID))
	})

	found = found[:min(len(found), storerule.ListLimit(f.Limit))]
	out := make([]Run, len(found))
	for i, r := range found {
		out[i] = cloneRun(r.run)
	}
	return out, nil
}

// listCursor is the position a listing starts after, or nil for the start.
// storerule.Cursor says which cursors are refused.
func listCursor(c *Cursor) (*Cursor, error) {
	if c == nil {
		return nil, nil
	}
	start, err := storerule.Cursor(c.ID, c.CreatedAt)
	if err != nil || start {
		return nil, err
	}
	return c, nil
}

// olderThan reports whether run comes after the position c in a listing,
// which is newest first and then by id descending.
func olderThan(run Run, c Cursor) bool {
	if order := run.CreatedAt.Compare(c.CreatedAt); order != 0 {
		return order < 0
	}
	return run.ID < c.ID
}

// Claim implements Store. The conditions and the update are those of the
// claim statement in agent/pg. Behind the one mutex every claim waits for a
// write in progress, which is what the contract asks of a claim by RunID.
func (s *MemoryStore) Claim(_ context.Context, req ClaimRequest) (*Run, error) {
	// A hold with no owner is no hold: the run would read as released, and
	// every write under it would be refused.
	if req.Owner == "" {
		return nil, errors.New("agent: claim: owner is empty")
	}
	if req.TTL <= 0 {
		return nil, fmt.Errorf("agent: claim: ttl is %s, not more than zero", req.TTL)
	}
	if !storerule.Comparable(req.Owner) {
		return nil, fmt.Errorf("agent: claim: owner %q holds a character no store keeps", req.Owner)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var r *memoryRun
	if req.RunID != "" {
		named, ok := s.runs[req.RunID]
		if !ok {
			return nil, ErrNotFound
		}
		if !claimable(named.run, req) {
			return nil, ErrNotClaimable
		}
		r = named
	} else {
		// The oldest, and of runs created at one instant the one whose id
		// sorts first: a table keeps no order of arrival to break the tie by.
		for _, candidate := range s.order {
			if claimable(candidate.run, req) && (r == nil || claimedBefore(candidate.run, r.run)) {
				r = candidate
			}
		}
		if r == nil {
			return nil, nil
		}
	}

	// An owner still on the run means its lease lapsed and was never given
	// back: the execution that held it died.
	if r.run.LeaseOwner != "" {
		r.run.Failures++
	}
	// To the microsecond above, so that the store never counts the lease
	// lapsed before its holder does.
	expires := storerule.Expiry(req.Now.Add(req.TTL))
	r.run.LeaseOwner = req.Owner
	r.run.LeaseEpoch++
	r.run.LeaseExpiresAt = &expires
	touch(r, req.Now)

	out := cloneRun(r.run)
	return &out, nil
}

// claimedBefore reports whether a claim takes a ahead of b.
func claimedBefore(a, b Run) bool {
	if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
		return order < 0
	}
	return a.ID < b.ID
}

func claimable(run Run, req ClaimRequest) bool {
	return run.Status == StatusRunnable &&
		slices.Contains(req.Agents, run.Agent) &&
		(run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(req.Now)) &&
		(run.NextAttemptAt == nil || !run.NextAttemptAt.After(req.Now))
}

// held returns the run lease names, or ErrLeaseLost unless lease is the hold
// the run records. Every method that takes a Lease starts here.
func (s *MemoryStore) held(lease Lease) (*memoryRun, error) {
	r, ok := s.runs[lease.RunID]
	if !ok {
		return nil, ErrNotFound
	}
	// A run nobody holds records an empty owner, which a lease with no owner
	// would otherwise match.
	if lease.Owner == "" || r.run.LeaseOwner != lease.Owner || r.run.LeaseEpoch != lease.Epoch {
		return nil, ErrLeaseLost
	}
	return r, nil
}

// touch records a change to r at now and returns the run's new Rev, which
// the caller stamps on each step and approval it changed.
func touch(r *memoryRun, now time.Time) int64 {
	r.run.Rev++
	r.run.UpdatedAt = storerule.Instant(now)
	return r.run.Rev
}

func release(r *memoryRun) {
	r.run.LeaseOwner = ""
	r.run.LeaseExpiresAt = nil
}

// wake makes a waiting run runnable. It no longer waits, so the reason it
// waited for goes too.
func wake(r *memoryRun) {
	if r.run.Status == StatusWaiting {
		r.run.Status = StatusRunnable
		r.run.Reason = ""
	}
}

// Heartbeat implements Store.
func (s *MemoryStore) Heartbeat(_ context.Context, lease Lease, now time.Time, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("agent: heartbeat: ttl is %s, not more than zero", ttl)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.held(lease)
	if err != nil {
		return false, err
	}
	expires := storerule.Expiry(now.Add(ttl))
	r.run.LeaseExpiresAt = &expires
	return r.run.CancelRequested, nil
}

// Yield implements Store.
func (s *MemoryStore) Yield(_ context.Context, lease Lease, req YieldRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.held(lease)
	if err != nil {
		return err
	}
	release(r)
	r.run.NextAttemptAt = storerule.InstantPtr(req.NextAttemptAt)
	if req.Failed {
		r.run.Failures++
		r.run.Error = storerule.Kept(req.Error)
	}
	touch(r, req.Now)
	return nil
}

// Park implements Store.
func (s *MemoryStore) Park(_ context.Context, lease Lease, req ParkRequest) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.held(lease)
	if err != nil {
		return false, err
	}
	if r.run.CancelRequested || !hasPending(r) || s.hasWork(r) {
		return false, nil
	}
	r.run.Status = StatusWaiting
	r.run.Reason = storerule.Kept(req.Reason)
	release(r)
	touch(r, req.Now)
	return true, nil
}

// hasPending reports whether r has something to wait for: an approval
// nobody has answered, or a child run that has not ended.
func hasPending(r *memoryRun) bool {
	return slices.ContainsFunc(r.approvals, func(a *memoryApproval) bool {
		return a.approval.Status == ApprovalPending
	}) || slices.ContainsFunc(r.children, func(c *memoryRun) bool {
		return !c.run.Terminal()
	})
}

// hasWork reports whether a waiting step of r has what it waited for: its
// child has ended, or the approval for its current attempt has its answer.
// The execution that holds r has something to do about it, so r is not
// parked, whatever else it still waits for.
func (s *MemoryStore) hasWork(r *memoryRun) bool {
	for _, st := range r.steps {
		if st.Kind != StepTool || st.Status != StepWaiting {
			continue
		}
		if st.ChildRunID != "" {
			if child, ok := s.runs[st.ChildRunID]; ok && child.run.Terminal() {
				return true
			}
			continue
		}
		for _, a := range r.approvals {
			if a.approval.Seq == st.Seq && a.approval.Attempt == st.Attempts && a.approval.Status != ApprovalPending {
				return true
			}
		}
	}
	return false
}

// Finish implements Store.
func (s *MemoryStore) Finish(_ context.Context, lease Lease, req FinishRequest) error {
	if !(Run{Status: req.Status}).Terminal() {
		return fmt.Errorf("agent: finish: status %q does not end a run", req.Status)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.held(lease)
	if err != nil {
		return err
	}

	finished := storerule.Instant(req.Now)
	r.run.Status = req.Status
	r.run.Reason = storerule.Kept(req.Reason)
	r.run.Output = storerule.Kept(req.Output)
	r.run.Error = storerule.Kept(req.Error)
	r.run.FinishedAt = &finished
	release(r)
	rev := touch(r, req.Now)
	for _, a := range r.approvals {
		if a.approval.Status == ApprovalPending {
			cancelled := finished
			a.approval.Status = ApprovalCancelled
			a.approval.DecidedAt = &cancelled
			a.approval.Rev = rev
		}
	}

	if parent, ok := s.runs[r.run.ParentID]; ok && parent.run.Status == StatusWaiting {
		wake(parent)
		touch(parent, req.Now)
	}
	return nil
}

// Steps implements Store.
func (s *MemoryStore) Steps(_ context.Context, runID string) ([]Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.runs[runID]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]Step, len(r.steps))
	for i, st := range r.steps {
		out[i] = cloneStep(*st)
	}
	return out, nil
}

// step returns the step of r at seq, or nil.
func (r *memoryRun) step(seq int) *Step {
	if seq < 1 || seq > len(r.steps) {
		return nil
	}
	return r.steps[seq-1]
}

// BeginModel implements Store.
func (s *MemoryStore) BeginModel(_ context.Context, lease Lease, seq int, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.held(lease)
	if err != nil {
		return err
	}
	started := storerule.Instant(now)
	if seq == len(r.steps)+1 {
		r.steps = append(r.steps, &Step{
			RunID:     r.run.ID,
			Seq:       seq,
			Kind:      StepModel,
			Status:    StepStarted,
			Attempts:  1,
			Rev:       touch(r, now),
			CreatedAt: started,
			StartedAt: &started,
		})
		return nil
	}
	st := r.step(seq)
	if st == nil || st.Kind != StepModel || st.Status != StepStarted {
		return ErrConflict
	}
	st.Attempts++
	st.StartedAt = &started
	st.Rev = touch(r, now)
	return nil
}

// CompleteModel implements Store.
func (s *MemoryStore) CompleteModel(_ context.Context, lease Lease, req CompleteModelRequest) error {
	message, err := storedMessage(req.Message)
	if err != nil {
		return fmt.Errorf("agent: complete model: message: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.held(lease)
	if err != nil {
		return err
	}
	st := r.step(req.Seq)
	if st == nil || st.Kind != StepModel || st.Status != StepStarted {
		return ErrConflict
	}
	// The calls go in at the sequence numbers after the reply, which only a
	// reply at the end of the journal has free.
	if len(req.Message.Calls) > 0 && req.Seq != len(r.steps) {
		return ErrConflict
	}

	rev := touch(r, req.Now)
	finished := storerule.Instant(req.Now)
	st.Status = StepCompleted
	st.Name = storerule.Kept(req.Model)
	st.Message = &message
	st.Stop = Stop(storerule.Kept(string(req.Stop)))
	st.Usage = req.Usage
	st.FinishedAt = &finished
	st.Rev = rev

	r.run.Usage = r.run.Usage.Add(req.Usage)
	r.run.ModelCalls++
	r.run.ActiveMillis += storerule.ActiveMillis(st.StartedAt, req.Now)
	r.run.Failures = 0

	// Each call is the message's own, copied, so the step shares nothing
	// with the reply. The step's name is a column of its own, where the
	// call's is inside JSON.
	for i, call := range message.Calls {
		seq := req.Seq + 1 + i
		call.Input = slices.Clone(call.Input)
		r.steps = append(r.steps, &Step{
			RunID:     r.run.ID,
			Seq:       seq,
			Kind:      StepTool,
			Status:    StepProposed,
			Name:      storerule.Kept(req.Message.Calls[i].Name),
			Turn:      req.Seq,
			Call:      &call,
			Key:       StepKey(r.run.ID, seq),
			Rev:       rev,
			CreatedAt: finished,
		})
	}
	return nil
}

// UpdateStep implements Store.
func (s *MemoryStore) UpdateStep(_ context.Context, lease Lease, req StepUpdate) error {
	if !isStepStatus(req.To) {
		return fmt.Errorf("agent: update step: %q is not a step status", req.To)
	}
	if req.ChildRunID != "" && !isUUID(req.ChildRunID) {
		return fmt.Errorf("agent: update step: child run id %q is not a UUID", req.ChildRunID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.held(lease)
	if err != nil {
		return err
	}
	st := r.step(req.Seq)
	if st == nil || st.Kind != StepTool || st.Status != req.From {
		return ErrConflict
	}

	st.Rev = touch(r, req.Now)
	st.Status = req.To
	if req.Decision != "" {
		st.Decision = Effect(storerule.Kept(string(req.Decision)))
		st.Rule = storerule.Kept(req.Rule)
	}
	if req.Result != nil {
		st.Result = storerule.Kept(*req.Result)
		st.IsError = req.IsError
	}
	if req.ChildRunID != "" {
		st.ChildRunID = req.ChildRunID
	}
	st.Usage = st.Usage.Add(req.Usage)
	r.run.Usage = r.run.Usage.Add(req.Usage)

	at := storerule.Instant(req.Now)
	switch {
	case req.To == StepStarted:
		st.Attempts++
		st.StartedAt = &at
	case req.To.Done():
		if req.From == StepStarted {
			r.run.ActiveMillis += storerule.ActiveMillis(st.StartedAt, req.Now)
		}
		st.FinishedAt = &at
		r.run.Failures = 0
	}
	return nil
}

func isStepStatus(status StepStatus) bool {
	return storerule.IsStepStatus(string(status))
}

// RequestApproval implements Store.
func (s *MemoryStore) RequestApproval(_ context.Context, lease Lease, req ApprovalRequest) (Approval, error) {
	if !isUUID(req.ID) {
		return Approval{}, fmt.Errorf("agent: request approval: id %q is not a UUID", req.ID)
	}
	if !storerule.IsCause(string(req.Cause)) {
		return Approval{}, fmt.Errorf("agent: request approval: %q is not a cause", req.Cause)
	}
	action, err := storedAction(req.Action)
	if err != nil {
		return Approval{}, fmt.Errorf("agent: request approval: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.held(lease)
	if err != nil {
		return Approval{}, err
	}
	st := r.step(req.Seq)
	if st == nil || st.Kind != StepTool {
		return Approval{}, ErrConflict
	}
	for _, a := range r.approvals {
		if a.approval.Seq == req.Seq && a.approval.Attempt == st.Attempts {
			return cloneApproval(a.approval), nil
		}
	}
	if st.Status != req.From {
		return Approval{}, ErrConflict
	}
	if _, ok := s.approvals[req.ID]; ok {
		return Approval{}, fmt.Errorf("agent: request approval: id %q is already in use", req.ID)
	}

	rev := touch(r, req.Now)
	st.Status = StepWaiting
	if req.Decision != "" {
		st.Decision = Effect(storerule.Kept(string(req.Decision)))
		st.Rule = storerule.Kept(req.Rule)
	}
	st.Rev = rev

	a := &memoryApproval{run: r, approval: Approval{
		ID:          req.ID,
		RunID:       r.run.ID,
		Seq:         req.Seq,
		Attempt:     st.Attempts,
		Cause:       req.Cause,
		Tool:        st.Name,
		Action:      action,
		Rule:        storerule.Kept(req.Rule),
		Status:      ApprovalPending,
		Rev:         rev,
		RequestedAt: storerule.Instant(req.Now),
		ExpiresAt:   storerule.InstantPtr(req.ExpiresAt),
	}}
	if st.Call != nil {
		a.approval.Input = slices.Clone(st.Call.Input)
	}
	r.approvals = append(r.approvals, a)
	s.approvals[req.ID] = a
	return cloneApproval(a.approval), nil
}

// GetApproval implements Store.
func (s *MemoryStore) GetApproval(_ context.Context, id string) (Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.approvals[id]
	if !ok {
		return Approval{}, ErrNotFound
	}
	return cloneApproval(a.approval), nil
}

// ListApprovals implements Store.
func (s *MemoryStore) ListApprovals(_ context.Context, f ApprovalFilter) ([]Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var found []Approval
	for _, a := range s.approvals {
		switch {
		case f.Status != "" && a.approval.Status != f.Status:
		case f.RunID != "" && a.approval.RunID != f.RunID:
		default:
			found = append(found, a.approval)
		}
	}
	slices.SortFunc(found, oldestFirst)

	found = found[:min(len(found), storerule.ListLimit(f.Limit))]
	for i := range found {
		found[i] = cloneApproval(found[i])
	}
	return found, nil
}

// oldestFirst orders approvals as ListApprovals and Changes return them.
func oldestFirst(a, b Approval) int {
	return cmp.Or(a.RequestedAt.Compare(b.RequestedAt), cmp.Compare(a.ID, b.ID))
}

// DecideApproval implements Store.
func (s *MemoryStore) DecideApproval(_ context.Context, req DecideRequest) (Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.approvals[req.ID]
	if !ok {
		return Approval{}, ErrNotFound
	}
	if a.approval.Status != ApprovalPending {
		return cloneApproval(a.approval), ErrAlreadyDecided
	}

	decided := storerule.Instant(req.Now)
	a.approval.Status = ApprovalDeclined
	if req.Approved {
		a.approval.Status = ApprovalApproved
	}
	a.approval.DecidedBy = storerule.Kept(req.By)
	a.approval.Reason = storerule.Kept(req.Reason)
	a.approval.DecidedAt = &decided
	a.approval.Rev = touch(a.run, req.Now)
	wake(a.run)
	return cloneApproval(a.approval), nil
}

// ExpireApprovals implements Store. Approvals of one run that lapse together
// change the run once, and carry the one new Rev.
func (s *MemoryStore) ExpireApprovals(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	lapsed := 0
	for _, r := range s.order {
		var rev int64
		for _, a := range r.approvals {
			if a.approval.Status != ApprovalPending || a.approval.ExpiresAt == nil || a.approval.ExpiresAt.After(now) {
				continue
			}
			if rev == 0 {
				rev = touch(r, now)
				wake(r)
			}
			decided := storerule.Instant(now)
			a.approval.Status = ApprovalExpired
			a.approval.DecidedAt = &decided
			a.approval.Rev = rev
			lapsed++
		}
	}
	return lapsed, nil
}

// RequestCancel implements Store.
func (s *MemoryStore) RequestCancel(_ context.Context, req CancelRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.runs[req.RunID]
	if !ok {
		return ErrNotFound
	}
	if r.run.Terminal() {
		return ErrFinished
	}
	// The first request stands: who asked, why, and the one revision.
	if r.run.CancelRequested {
		return nil
	}
	r.run.CancelRequested = true
	r.run.CancelBy = storerule.Kept(req.By)
	r.run.CancelReason = storerule.Kept(req.Reason)
	wake(r)
	touch(r, req.Now)
	return nil
}

// Changes implements Store. Steps come in journal order and approvals
// oldest first, whatever order they changed in.
func (s *MemoryStore) Changes(_ context.Context, runID string, since int64) (Changes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.runs[runID]
	if !ok {
		return Changes{}, ErrNotFound
	}
	out := Changes{Run: cloneRun(r.run)}
	for _, st := range r.steps {
		if st.Rev > since {
			out.Steps = append(out.Steps, cloneStep(*st))
		}
	}
	for _, a := range r.approvals {
		if a.approval.Rev > since {
			out.Approvals = append(out.Approvals, cloneApproval(a.approval))
		}
	}
	slices.SortFunc(out.Approvals, oldestFirst)
	return out, nil
}

// The copies. Each follows every pointer, slice and map its value holds, so
// the copy and the original share nothing.

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneRun(r Run) Run {
	if r.Definition.Tools != nil {
		tools := make([]ToolSpec, len(r.Definition.Tools))
		for i, tool := range r.Definition.Tools {
			tool.Schema = slices.Clone(tool.Schema)
			tools[i] = tool
		}
		r.Definition.Tools = tools
	}
	r.Definition.Output = slices.Clone(r.Definition.Output)
	r.Metadata = maps.Clone(r.Metadata)
	r.LeaseExpiresAt = cloneTime(r.LeaseExpiresAt)
	r.NextAttemptAt = cloneTime(r.NextAttemptAt)
	r.FinishedAt = cloneTime(r.FinishedAt)
	return r
}

func cloneMessage(m Message) Message {
	if m.Calls != nil {
		calls := make([]Call, len(m.Calls))
		for i, call := range m.Calls {
			call.Input = slices.Clone(call.Input)
			calls[i] = call
		}
		m.Calls = calls
	}
	m.Results = slices.Clone(m.Results)
	if m.Opaque != nil {
		opaque := *m.Opaque
		opaque.Data = slices.Clone(opaque.Data)
		m.Opaque = &opaque
	}
	return m
}

func cloneStep(st Step) Step {
	if st.Message != nil {
		message := cloneMessage(*st.Message)
		st.Message = &message
	}
	if st.Call != nil {
		call := *st.Call
		call.Input = slices.Clone(call.Input)
		st.Call = &call
	}
	st.StartedAt = cloneTime(st.StartedAt)
	st.FinishedAt = cloneTime(st.FinishedAt)
	return st
}

func cloneApproval(a Approval) Approval {
	a.Input = slices.Clone(a.Input)
	a.Action = cloneAction(a.Action)
	a.DecidedAt = cloneTime(a.DecidedAt)
	a.ExpiresAt = cloneTime(a.ExpiresAt)
	return a
}

// storedSnapshot is a definition as it reads back from a store, which keeps
// it as JSON: each schema as the bytes it came as, and each string with a
// byte that is not UTF-8 as the replacement character. It fails for a schema
// that is not JSON.
func storedSnapshot(def Snapshot) (Snapshot, error) {
	if err := storerule.ValidRaw(def.Output); err != nil {
		return Snapshot{}, fmt.Errorf("output: %w", err)
	}
	def.System, def.Model = storerule.KeptInJSON(def.System), storerule.KeptInJSON(def.Model)
	def.Output = rawOrNone(def.Output)
	if def.Tools != nil {
		tools := make([]ToolSpec, len(def.Tools))
		for i, tool := range def.Tools {
			if err := storerule.ValidRaw(tool.Schema); err != nil {
				return Snapshot{}, fmt.Errorf("tool %q: schema: %w", tool.Name, err)
			}
			tool.Name, tool.Description = storerule.KeptInJSON(tool.Name), storerule.KeptInJSON(tool.Description)
			tool.Schema = rawOrNone(tool.Schema)
			tools[i] = tool
		}
		def.Tools = tools
	}
	return def, nil
}

// rawOrNone is a schema as it reads back from a store: a copy, and nil for
// one that was empty, which JSON leaves out.
func rawOrNone(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	return slices.Clone(value)
}

// storedMessage is a reply as it reads back from a store, which keeps it as
// JSON: the arguments of each call and the provider's form as the bytes they
// came as, or JSON's null for none, and each string with a byte that is not
// UTF-8 as the replacement character. It fails for arguments or a provider's
// form that are not JSON.
func storedMessage(m Message) (Message, error) {
	m = cloneMessage(m)
	m.Role, m.Text = Role(storerule.KeptInJSON(string(m.Role))), storerule.KeptInJSON(m.Text)
	for i := range m.Calls {
		call := &m.Calls[i]
		if err := storerule.ValidRaw(call.Input); err != nil {
			return Message{}, fmt.Errorf("call %d: input: %w", i+1, err)
		}
		call.ID, call.Name, call.Input = storerule.KeptInJSON(call.ID), storerule.KeptInJSON(call.Name), storerule.OrNull(call.Input)
	}
	for i := range m.Results {
		result := &m.Results[i]
		result.CallID, result.Content = storerule.KeptInJSON(result.CallID), storerule.KeptInJSON(result.Content)
	}
	if m.Opaque != nil {
		if err := storerule.ValidRaw(m.Opaque.Data); err != nil {
			return Message{}, fmt.Errorf("opaque: data: %w", err)
		}
		m.Opaque.Provider, m.Opaque.Data = storerule.KeptInJSON(m.Opaque.Provider), storerule.OrNull(m.Opaque.Data)
	}
	return m, nil
}

// storedAction is a as it reads back from a store: its attributes as
// storerule.Attrs gives them, and its kind and target as strings that are
// only recorded. It fails for attributes JSON cannot hold.
func storedAction(a Action) (Action, error) {
	attrs, err := storerule.Attrs(a.Attrs)
	if err != nil {
		return Action{}, err
	}
	a.Kind, a.Target, a.Attrs = storerule.Kept(a.Kind), storerule.Kept(a.Target), attrs
	return a, nil
}

func cloneAction(a Action) Action {
	if a.Attrs != nil {
		attrs := make(map[string]any, len(a.Attrs))
		for k, v := range a.Attrs {
			attrs[k] = cloneValue(v)
		}
		a.Attrs = attrs
	}
	return a
}

// cloneValue copies an attribute's value. A stored action's attributes have
// been through JSON, so a value is a map, a list or a scalar.
func cloneValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = cloneValue(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = cloneValue(e)
		}
		return out
	}
	return v
}
