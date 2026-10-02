package agenttest_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

// The suite must hold a store to the contract and to nothing else. textStore
// answers as a store that keeps its rows in a database would: every value it
// returns has been through JSON, every time comes back in the server's zone,
// and a list it was given none of comes back empty rather than nil. If a
// case relied on what only an in-process store does, it would fail here,
// without a database to say so.
func TestRunStoreSuite_AssumesNothingOfHowAStoreKeepsItsRows(t *testing.T) {
	agenttest.RunStoreSuite(t, func(*testing.T) agent.Store {
		return textStore{inner: agent.NewMemoryStore()}
	})
}

// anySpellingEnv, when set, makes the test below run the suite against a
// store that must fail it.
const anySpellingEnv = "KEEL_AGENTTEST_ANY_SPELLING"

// The case that must fail. A database reads a UUID in any spelling and keeps
// the canonical one, so a store that hands ids to it without checking their
// form finds a run by the upper-case spelling of its id, and keeps a run
// under an id other than the one it was given. The suite says a store knows
// an id by the one string. This runs the suite, in a process of its own,
// against a store with that check left out, and requires it to fail, in the
// cases about an id's spelling and in no others: if those cases stopped
// telling the two stores apart, this test would say so.
func TestRunStoreSuite_FailsAStoreThatReadsAnIDInAnySpelling(t *testing.T) {
	if os.Getenv(anySpellingEnv) != "" {
		agenttest.RunStoreSuite(t, func(*testing.T) agent.Store {
			return textStore{inner: agent.NewMemoryStore(), anySpelling: true}
		})
		return
	}

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), anySpellingEnv+"=1")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "the suite passed a store that reads an id in any spelling")

	// A case is a subtest two levels down: the suite, its group, the case.
	failed := map[string][]string{}
	for _, m := range regexp.MustCompile(`--- FAIL: `+t.Name()+`/([A-Za-z]+)/(\S+)`).FindAllStringSubmatch(string(out), -1) {
		group, name := m[1], m[2]
		failed[group] = append(failed[group], name)
		assert.Contains(t, name, "spelling", "%s/%s failed, and is not about an id's spelling", group, name)
	}
	for _, group := range []string{"CreateRun", "UpdateStep", "RequestApproval", "ListRuns", "ListApprovals"} {
		assert.NotEmpty(t, failed[group], "no case of %s caught the store", group)
	}
	// Every method that looks a run or an approval up, in both spellings.
	assert.Len(t, failed["NotFound"], 30, "the lookups that caught the store:\n%s", strings.Join(failed["NotFound"], "\n"))
}

// textStore is a Store over another that answers as a database would.
type textStore struct {
	inner agent.Store
	// anySpelling leaves out the check of an id's form that a store makes
	// before it asks its database, and reads an id as a UUID column does: in
	// any spelling, as the canonical one.
	anySpelling bool
}

// serverZone is not UTC and not the zone the suite's clock is in.
var serverZone = time.FixedZone("server", -7*60*60)

// column is id as a UUID column reads it when the store has not checked its
// form: any spelling of a UUID is that UUID. What is no UUID at all is left
// for the store beneath to refuse.
func (s textStore) column(id string) string {
	if !s.anySpelling {
		return id
	}
	if parsed, err := uuid.Parse(id); err == nil {
		return parsed.String()
	}
	return id
}

func (s textStore) lease(lease agent.Lease) agent.Lease {
	lease.RunID = s.column(lease.RunID)
	return lease
}

// asText returns v as it reads back after being stored as JSON, and false
// for a value JSON cannot hold, which is the store's to refuse.
func asText[T any](v T) (T, bool) {
	var out T
	text, err := json.Marshal(v)
	if err != nil {
		return v, false
	}
	if err := json.Unmarshal(text, &out); err != nil {
		return v, false
	}
	return out, true
}

// mustText is asText for what a store returned, which was stored and so
// reads back.
func mustText[T any](v T) T {
	out, ok := asText(v)
	if !ok {
		panic("a store returned a value that does not read back as JSON")
	}
	return out
}

func inServerZone(t *time.Time) {
	if t != nil {
		*t = t.Truncate(time.Microsecond).In(serverZone)
	}
}

func textRun(r agent.Run) agent.Run {
	r = mustText(r)
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
		s = mustText(s)
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
	a = mustText(a)
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

func (s textStore) CreateRun(ctx context.Context, run agent.Run) (agent.Run, bool, error) {
	run.ID, run.ParentID = s.column(run.ID), s.column(run.ParentID)
	stored, created, err := s.inner.CreateRun(ctx, run)
	if err != nil {
		return agent.Run{}, false, err
	}
	return textRun(stored), created, nil
}

func (s textStore) GetRun(ctx context.Context, id string) (agent.Run, error) {
	run, err := s.inner.GetRun(ctx, s.column(id))
	if err != nil {
		return agent.Run{}, err
	}
	return textRun(run), nil
}

func (s textStore) ListRuns(ctx context.Context, f agent.RunFilter) ([]agent.Run, error) {
	f.ParentID = s.column(f.ParentID)
	runs, err := s.inner.ListRuns(ctx, f)
	if err != nil {
		return nil, err
	}
	return textRuns(runs), nil
}

func (s textStore) Claim(ctx context.Context, req agent.ClaimRequest) (*agent.Run, error) {
	req.RunID = s.column(req.RunID)
	run, err := s.inner.Claim(ctx, req)
	if err != nil || run == nil {
		return nil, err
	}
	out := textRun(*run)
	return &out, nil
}

func (s textStore) Heartbeat(ctx context.Context, lease agent.Lease, now time.Time, ttl time.Duration) (bool, error) {
	return s.inner.Heartbeat(ctx, s.lease(lease), now, ttl)
}

func (s textStore) Yield(ctx context.Context, lease agent.Lease, req agent.YieldRequest) error {
	return s.inner.Yield(ctx, s.lease(lease), req)
}

func (s textStore) Park(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error) {
	return s.inner.Park(ctx, s.lease(lease), req)
}

func (s textStore) Finish(ctx context.Context, lease agent.Lease, req agent.FinishRequest) error {
	return s.inner.Finish(ctx, s.lease(lease), req)
}

func (s textStore) Steps(ctx context.Context, runID string) ([]agent.Step, error) {
	steps, err := s.inner.Steps(ctx, s.column(runID))
	if err != nil {
		return nil, err
	}
	return textSteps(steps), nil
}

func (s textStore) BeginModel(ctx context.Context, lease agent.Lease, seq int, now time.Time) error {
	return s.inner.BeginModel(ctx, s.lease(lease), seq, now)
}

func (s textStore) CompleteModel(ctx context.Context, lease agent.Lease, req agent.CompleteModelRequest) error {
	// What a database is sent is the message as JSON.
	req.Message = mustText(req.Message)
	return s.inner.CompleteModel(ctx, s.lease(lease), req)
}

func (s textStore) UpdateStep(ctx context.Context, lease agent.Lease, req agent.StepUpdate) error {
	req.ChildRunID = s.column(req.ChildRunID)
	return s.inner.UpdateStep(ctx, s.lease(lease), req)
}

func (s textStore) RequestApproval(ctx context.Context, lease agent.Lease, req agent.ApprovalRequest) (agent.Approval, error) {
	req.ID = s.column(req.ID)
	// An action JSON cannot hold goes to the store as it is, to be refused.
	if action, ok := asText(req.Action); ok {
		req.Action = action
	}
	approval, err := s.inner.RequestApproval(ctx, s.lease(lease), req)
	if err != nil {
		return agent.Approval{}, err
	}
	return textApproval(approval), nil
}

func (s textStore) GetApproval(ctx context.Context, id string) (agent.Approval, error) {
	approval, err := s.inner.GetApproval(ctx, s.column(id))
	if err != nil {
		return agent.Approval{}, err
	}
	return textApproval(approval), nil
}

func (s textStore) ListApprovals(ctx context.Context, f agent.ApprovalFilter) ([]agent.Approval, error) {
	f.RunID = s.column(f.RunID)
	approvals, err := s.inner.ListApprovals(ctx, f)
	if err != nil {
		return nil, err
	}
	return textApprovals(approvals), nil
}

func (s textStore) DecideApproval(ctx context.Context, req agent.DecideRequest) (agent.Approval, error) {
	req.ID = s.column(req.ID)
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
	req.RunID = s.column(req.RunID)
	return s.inner.RequestCancel(ctx, req)
}

func (s textStore) Changes(ctx context.Context, runID string, since int64) (agent.Changes, error) {
	changes, err := s.inner.Changes(ctx, s.column(runID), since)
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
