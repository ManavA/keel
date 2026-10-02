package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The bounds on a listing's length, for RunFilter.Limit and
// ApprovalFilter.Limit.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

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
	if !storable(run.Agent) || !storable(run.Key) || !storable(run.LeaseOwner) {
		return Run{}, false, fmt.Errorf("agent: create run: the agent %q, the key %q or the owner %q holds a character no store keeps",
			run.Agent, run.Key, run.LeaseOwner)
	}
	definition, err := storedSnapshot(run.Definition)
	if err != nil {
		return Run{}, false, fmt.Errorf("agent: create run: definition: %w", err)
	}
	metadata, err := storedMetadata(run.Metadata)
	if err != nil {
		return Run{}, false, fmt.Errorf("agent: create run: %w", err)
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
	r.run.Metadata = metadata
	r.run.Reason, r.run.Input, r.run.Output, r.run.Error = kept(run.Reason), kept(run.Input), kept(run.Output), kept(run.Error)
	r.run.CancelBy, r.run.CancelReason = kept(run.CancelBy), kept(run.CancelReason)
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

// isUUID reports whether id is a UUID as uuid.NewString writes one. Other
// spellings of the same UUID are refused: a database would take them and
// hand back this one, and the id would no longer be the one given.
func isUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

// replacement is what a character no database column holds is kept as.
const replacement = "\uFFFD"

// storable reports whether a text column holds s as it is: with no NUL
// character in it, and in UTF-8.
func storable(s string) bool {
	return strings.IndexByte(s, 0) < 0 && utf8.ValidString(s)
}

// kept is s as a store keeps a string it only records: with each NUL and
// each byte that is not UTF-8 as the replacement character.
func kept(s string) string {
	if storable(s) {
		return s
	}
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", replacement), replacement)
}

// keptInJSON is s as a store keeps a string inside a JSON value, where a NUL
// has an escape and is kept: only a byte that is not UTF-8 is replaced.
func keptInJSON(s string) string {
	return strings.ToValidUTF8(s, replacement)
}

// validRaw reports why a store could not keep value, which is JSON somebody
// else wrote. None is kept as none.
func validRaw(value json.RawMessage) error {
	switch {
	case len(value) == 0:
		return nil
	case !json.Valid(value):
		return errors.New("not valid JSON")
	case !utf8.Valid(value):
		return errors.New("not UTF-8")
	}
	return nil
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

	found = found[:min(len(found), listLimit(f.Limit))]
	out := make([]Run, len(found))
	for i, r := range found {
		out[i] = cloneRun(r.run)
	}
	return out, nil
}

// listCursor is the position a listing starts after, or nil for the start.
// A cursor is an argument, and may have come from a client: its id must be a
// UUID in the one form, though it need not be one a run has. The zero Cursor
// is no position, and lists from the start.
func listCursor(c *Cursor) (*Cursor, error) {
	if c == nil || c.ID == "" && c.CreatedAt.IsZero() {
		return nil, nil
	}
	if !isUUID(c.ID) {
		return nil, fmt.Errorf("cursor id %q is not a UUID", c.ID)
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

func listLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultListLimit
	case limit > maxListLimit:
		return maxListLimit
	}
	return limit
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
	if !storable(req.Owner) {
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
	expires := req.Now.Add(req.TTL)
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
	r.run.UpdatedAt = now
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
	expires := now.Add(ttl)
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
	r.run.NextAttemptAt = cloneTime(req.NextAttemptAt)
	if req.Failed {
		r.run.Failures++
		r.run.Error = kept(req.Error)
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
	if r.run.CancelRequested || !hasPending(r) {
		return false, nil
	}
	r.run.Status = StatusWaiting
	r.run.Reason = kept(req.Reason)
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

	finished := req.Now
	r.run.Status = req.Status
	r.run.Reason = kept(req.Reason)
	r.run.Output = kept(req.Output)
	r.run.Error = kept(req.Error)
	r.run.FinishedAt = &finished
	release(r)
	rev := touch(r, req.Now)
	for _, a := range r.approvals {
		if a.approval.Status == ApprovalPending {
			cancelled := req.Now
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
	started := now
	if seq == len(r.steps)+1 {
		r.steps = append(r.steps, &Step{
			RunID:     r.run.ID,
			Seq:       seq,
			Kind:      StepModel,
			Status:    StepStarted,
			Attempts:  1,
			Rev:       touch(r, now),
			CreatedAt: now,
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
	finished := req.Now
	st.Status = StepCompleted
	st.Name = kept(req.Model)
	st.Message = &message
	st.Stop = Stop(kept(string(req.Stop)))
	st.Usage = req.Usage
	st.FinishedAt = &finished
	st.Rev = rev

	r.run.Usage = r.run.Usage.Add(req.Usage)
	r.run.ModelCalls++
	r.run.ActiveMillis += activeMillis(st.StartedAt, req.Now)
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
			Name:      kept(req.Message.Calls[i].Name),
			Turn:      req.Seq,
			Call:      &call,
			Key:       StepKey(r.run.ID, seq),
			Rev:       rev,
			CreatedAt: req.Now,
		})
	}
	return nil
}

// activeMillis is the time a step that started at start and finished at end
// spent working.
func activeMillis(start *time.Time, end time.Time) int64 {
	if start == nil {
		return 0
	}
	return end.Sub(*start).Milliseconds()
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
		st.Decision = Effect(kept(string(req.Decision)))
		st.Rule = kept(req.Rule)
	}
	if req.Result != nil {
		st.Result = kept(*req.Result)
		st.IsError = req.IsError
	}
	if req.ChildRunID != "" {
		st.ChildRunID = req.ChildRunID
	}
	st.Usage = st.Usage.Add(req.Usage)
	r.run.Usage = r.run.Usage.Add(req.Usage)

	at := req.Now
	switch {
	case req.To == StepStarted:
		st.Attempts++
		st.StartedAt = &at
	case req.To.Done():
		if req.From == StepStarted {
			r.run.ActiveMillis += activeMillis(st.StartedAt, req.Now)
		}
		st.FinishedAt = &at
		r.run.Failures = 0
	}
	return nil
}

func isStepStatus(status StepStatus) bool {
	switch status {
	case StepProposed, StepWaiting, StepStarted, StepCompleted, StepBlocked, StepDeclined:
		return true
	}
	return false
}

// RequestApproval implements Store.
func (s *MemoryStore) RequestApproval(_ context.Context, lease Lease, req ApprovalRequest) (Approval, error) {
	if !isUUID(req.ID) {
		return Approval{}, fmt.Errorf("agent: request approval: id %q is not a UUID", req.ID)
	}
	if !isCause(req.Cause) {
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
		st.Decision = Effect(kept(string(req.Decision)))
		st.Rule = kept(req.Rule)
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
		Rule:        kept(req.Rule),
		Status:      ApprovalPending,
		Rev:         rev,
		RequestedAt: req.Now,
		ExpiresAt:   cloneTime(req.ExpiresAt),
	}}
	if st.Call != nil {
		a.approval.Input = slices.Clone(st.Call.Input)
	}
	r.approvals = append(r.approvals, a)
	s.approvals[req.ID] = a
	return cloneApproval(a.approval), nil
}

func isCause(cause ApprovalCause) bool {
	switch cause {
	case CauseGuard, CauseTool, CauseInterrupted:
		return true
	}
	return false
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

	found = found[:min(len(found), listLimit(f.Limit))]
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

	decided := req.Now
	a.approval.Status = ApprovalDeclined
	if req.Approved {
		a.approval.Status = ApprovalApproved
	}
	a.approval.DecidedBy = kept(req.By)
	a.approval.Reason = kept(req.Reason)
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
			decided := now
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
	r.run.CancelBy = kept(req.By)
	r.run.CancelReason = kept(req.Reason)
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

// storedMetadata is metadata as it reads back from a store: through JSON,
// so a byte that is no UTF-8 is the replacement character, and none is an
// empty map. A NUL is the replacement character too, in a key or a value.
func storedMetadata(metadata map[string]string) (map[string]string, error) {
	text, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	var read map[string]string
	if err := json.Unmarshal(text, &read); err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	stored := make(map[string]string, len(read))
	for k, v := range read {
		stored[kept(k)] = kept(v)
	}
	return stored, nil
}

// storedSnapshot is a definition as it reads back from a store, which keeps
// it as JSON: each schema as the bytes it came as, and each string with a
// byte that is not UTF-8 as the replacement character. It fails for a schema
// that is not JSON.
func storedSnapshot(def Snapshot) (Snapshot, error) {
	if err := validRaw(def.Output); err != nil {
		return Snapshot{}, fmt.Errorf("output: %w", err)
	}
	def.System, def.Model = keptInJSON(def.System), keptInJSON(def.Model)
	def.Output = slices.Clone(def.Output)
	if def.Tools != nil {
		tools := make([]ToolSpec, len(def.Tools))
		for i, tool := range def.Tools {
			if err := validRaw(tool.Schema); err != nil {
				return Snapshot{}, fmt.Errorf("tool %q: schema: %w", tool.Name, err)
			}
			tool.Name, tool.Description = keptInJSON(tool.Name), keptInJSON(tool.Description)
			tool.Schema = slices.Clone(tool.Schema)
			tools[i] = tool
		}
		def.Tools = tools
	}
	return def, nil
}

// storedMessage is a reply as it reads back from a store, which keeps it as
// JSON: the arguments of each call and the provider's form as the bytes they
// came as, and each string with a byte that is not UTF-8 as the replacement
// character. It fails for arguments or a provider's form that are not JSON.
func storedMessage(m Message) (Message, error) {
	m = cloneMessage(m)
	m.Role, m.Text = Role(keptInJSON(string(m.Role))), keptInJSON(m.Text)
	for i := range m.Calls {
		call := &m.Calls[i]
		if err := validRaw(call.Input); err != nil {
			return Message{}, fmt.Errorf("call %d: input: %w", i+1, err)
		}
		call.ID, call.Name = keptInJSON(call.ID), keptInJSON(call.Name)
	}
	for i := range m.Results {
		result := &m.Results[i]
		result.CallID, result.Content = keptInJSON(result.CallID), keptInJSON(result.Content)
	}
	if m.Opaque != nil {
		if err := validRaw(m.Opaque.Data); err != nil {
			return Message{}, fmt.Errorf("opaque: data: %w", err)
		}
		m.Opaque.Provider = keptInJSON(m.Opaque.Provider)
	}
	return m, nil
}

// storedAction is a as it reads back from a store: its attributes through
// JSON, so a number is a float64 (and an integer past 2^53 is no longer
// exact), an object a map[string]any and a list a []any, and no attributes
// are an empty map. It fails for attributes JSON cannot hold.
func storedAction(a Action) (Action, error) {
	text, err := json.Marshal(a.Attrs)
	if err != nil {
		return Action{}, fmt.Errorf("action attributes: %w", err)
	}
	var attrs map[string]any
	if err := json.Unmarshal(text, &attrs); err != nil {
		return Action{}, fmt.Errorf("action attributes: %w", err)
	}
	// A NUL has no place in a value a database keeps, in a key or in a
	// string at any depth.
	a.Attrs, _ = withoutNUL(attrs).(map[string]any)
	if a.Attrs == nil {
		a.Attrs = map[string]any{}
	}
	a.Kind, a.Target = kept(a.Kind), kept(a.Target)
	return a, nil
}

// withoutNUL is a value that has been through JSON with each NUL in its
// strings and keys as the replacement character.
func withoutNUL(value any) any {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, "\x00", replacement)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = withoutNUL(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, e := range v {
			out[strings.ReplaceAll(key, "\x00", replacement)] = withoutNUL(e)
		}
		return out
	}
	return value
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
