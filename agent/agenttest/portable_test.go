package agenttest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

// The suite must hold a store to the contract and to nothing else. textStore
// answers as a store that keeps its rows in a database would: every value it
// returns has been through JSON, every time comes back in the server's zone,
// a map or list it was given none of comes back empty rather than nil, an id
// it is asked to keep must be a UUID, and a child's parent must already be
// there. If a case relied on what only an in-process store does, it would
// fail here, without a database to say so.
func TestRunStoreSuite_AssumesNothingOfHowAStoreKeepsItsRows(t *testing.T) {
	agenttest.RunStoreSuite(t, func(*testing.T) agent.Store {
		return textStore{inner: agent.NewMemoryStore()}
	})
}

type textStore struct{ inner agent.Store }

// serverZone is not UTC and not the zone the suite's clock is in.
var serverZone = time.FixedZone("server", -7*60*60)

// asText returns v as it reads back after being stored as JSON. A value that
// cannot be stored is a bug in the suite, which must pass only what a
// database could keep.
func asText[T any](v T) T {
	text, err := json.Marshal(v)
	if err != nil {
		panic("the suite passed a value that does not marshal: " + err.Error())
	}
	var out T
	if err := json.Unmarshal(text, &out); err != nil {
		panic("the suite passed a value that does not read back: " + err.Error())
	}
	return out
}

func inServerZone(t *time.Time) {
	if t != nil {
		*t = t.Truncate(time.Microsecond).In(serverZone)
	}
}

func textRun(r agent.Run) agent.Run {
	r = asText(r)
	if r.Metadata == nil {
		r.Metadata = map[string]string{}
	}
	if r.Definition.Tools == nil {
		r.Definition.Tools = []agent.ToolSpec{}
	}
	inServerZone(&r.CreatedAt)
	inServerZone(&r.UpdatedAt)
	inServerZone(r.FinishedAt)
	inServerZone(r.LeaseExpiresAt)
	inServerZone(r.NextAttemptAt)
	return r
}

func textRuns(runs []agent.Run) []agent.Run {
	out := make([]agent.Run, len(runs))
	for i, r := range runs {
		out[i] = textRun(r)
	}
	return out
}

func textSteps(steps []agent.Step) []agent.Step {
	out := make([]agent.Step, len(steps))
	for i, s := range steps {
		s = asText(s)
		if s.Message != nil {
			if s.Message.Calls == nil {
				s.Message.Calls = []agent.Call{}
			}
			if s.Message.Results == nil {
				s.Message.Results = []agent.Result{}
			}
		}
		inServerZone(&s.CreatedAt)
		inServerZone(s.StartedAt)
		inServerZone(s.FinishedAt)
		out[i] = s
	}
	return out
}

func textApproval(a agent.Approval) agent.Approval {
	a = asText(a)
	if a.Action.Attrs == nil {
		a.Action.Attrs = map[string]any{}
	}
	inServerZone(&a.RequestedAt)
	inServerZone(a.DecidedAt)
	inServerZone(a.ExpiresAt)
	return a
}

func textApprovals(approvals []agent.Approval) []agent.Approval {
	out := make([]agent.Approval, len(approvals))
	for i, a := range approvals {
		out[i] = textApproval(a)
	}
	return out
}

// uuidColumn refuses an id that a UUID column would.
func uuidColumn(name, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("textStore: %s %q is not a UUID", name, id)
	}
	return nil
}

func (s textStore) CreateRun(ctx context.Context, run agent.Run) (agent.Run, bool, error) {
	if err := uuidColumn("run id", run.ID); err != nil {
		return agent.Run{}, false, err
	}
	if run.ParentID != "" {
		if err := uuidColumn("parent id", run.ParentID); err != nil {
			return agent.Run{}, false, err
		}
		if _, err := s.inner.GetRun(ctx, run.ParentID); err != nil {
			return agent.Run{}, false, fmt.Errorf("textStore: parent %s: %w", run.ParentID, err)
		}
	}
	stored, created, err := s.inner.CreateRun(ctx, run)
	if err != nil {
		return agent.Run{}, false, err
	}
	return textRun(stored), created, nil
}

func (s textStore) GetRun(ctx context.Context, id string) (agent.Run, error) {
	run, err := s.inner.GetRun(ctx, id)
	if err != nil {
		return agent.Run{}, err
	}
	return textRun(run), nil
}

func (s textStore) ListRuns(ctx context.Context, f agent.RunFilter) ([]agent.Run, error) {
	runs, err := s.inner.ListRuns(ctx, f)
	if err != nil {
		return nil, err
	}
	return textRuns(runs), nil
}

func (s textStore) Claim(ctx context.Context, req agent.ClaimRequest) (*agent.Run, error) {
	run, err := s.inner.Claim(ctx, req)
	if err != nil || run == nil {
		return nil, err
	}
	out := textRun(*run)
	return &out, nil
}

func (s textStore) Heartbeat(ctx context.Context, lease agent.Lease, now time.Time, ttl time.Duration) (bool, error) {
	return s.inner.Heartbeat(ctx, lease, now, ttl)
}

func (s textStore) Yield(ctx context.Context, lease agent.Lease, req agent.YieldRequest) error {
	return s.inner.Yield(ctx, lease, req)
}

func (s textStore) Park(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error) {
	return s.inner.Park(ctx, lease, req)
}

func (s textStore) Finish(ctx context.Context, lease agent.Lease, req agent.FinishRequest) error {
	return s.inner.Finish(ctx, lease, req)
}

func (s textStore) Steps(ctx context.Context, runID string) ([]agent.Step, error) {
	steps, err := s.inner.Steps(ctx, runID)
	if err != nil {
		return nil, err
	}
	return textSteps(steps), nil
}

func (s textStore) BeginModel(ctx context.Context, lease agent.Lease, seq int, now time.Time) error {
	return s.inner.BeginModel(ctx, lease, seq, now)
}

func (s textStore) CompleteModel(ctx context.Context, lease agent.Lease, req agent.CompleteModelRequest) error {
	// What a database is sent is the message as JSON.
	req.Message = asText(req.Message)
	return s.inner.CompleteModel(ctx, lease, req)
}

func (s textStore) UpdateStep(ctx context.Context, lease agent.Lease, req agent.StepUpdate) error {
	if req.ChildRunID != "" {
		if err := uuidColumn("child run id", req.ChildRunID); err != nil {
			return err
		}
	}
	return s.inner.UpdateStep(ctx, lease, req)
}

func (s textStore) RequestApproval(ctx context.Context, lease agent.Lease, req agent.ApprovalRequest) (agent.Approval, error) {
	if err := uuidColumn("approval id", req.ID); err != nil {
		return agent.Approval{}, err
	}
	req.Action = asText(req.Action)
	approval, err := s.inner.RequestApproval(ctx, lease, req)
	if err != nil {
		return agent.Approval{}, err
	}
	return textApproval(approval), nil
}

func (s textStore) GetApproval(ctx context.Context, id string) (agent.Approval, error) {
	approval, err := s.inner.GetApproval(ctx, id)
	if err != nil {
		return agent.Approval{}, err
	}
	return textApproval(approval), nil
}

func (s textStore) ListApprovals(ctx context.Context, f agent.ApprovalFilter) ([]agent.Approval, error) {
	approvals, err := s.inner.ListApprovals(ctx, f)
	if err != nil {
		return nil, err
	}
	return textApprovals(approvals), nil
}

func (s textStore) DecideApproval(ctx context.Context, req agent.DecideRequest) (agent.Approval, error) {
	approval, err := s.inner.DecideApproval(ctx, req)
	if approval.ID == "" {
		return agent.Approval{}, err
	}
	// With ErrAlreadyDecided too: the approval comes back as it stands.
	return textApproval(approval), err
}

func (s textStore) ExpireApprovals(ctx context.Context, now time.Time) (int, error) {
	return s.inner.ExpireApprovals(ctx, now)
}

func (s textStore) RequestCancel(ctx context.Context, req agent.CancelRequest) error {
	return s.inner.RequestCancel(ctx, req)
}

func (s textStore) Changes(ctx context.Context, runID string, since int64) (agent.Changes, error) {
	changes, err := s.inner.Changes(ctx, runID, since)
	if err != nil {
		return agent.Changes{}, err
	}
	return agent.Changes{
		Run:       textRun(changes.Run),
		Steps:     textSteps(changes.Steps),
		Approvals: textApprovals(changes.Approvals),
	}, nil
}

var _ agent.Store = textStore{}
