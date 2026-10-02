package httpapi_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/agent"
)

// servedFields is every field of every type the API serves, as a step, a run
// or an approval is: the timeline and the stream send them as the engine
// returns them, with one field blanked, Message.Opaque (see strippedFields).
// Each is decided: it may be shown to anyone who may read a journal.
var servedFields = []string{
	"Action.Attrs", "Action.Kind", "Action.Target",
	"Approval.Action", "Approval.Attempt", "Approval.Cause", "Approval.DecidedAt", "Approval.DecidedBy",
	"Approval.ExpiresAt", "Approval.ID", "Approval.Input", "Approval.Reason", "Approval.RequestedAt",
	"Approval.Rev", "Approval.Rule", "Approval.RunID", "Approval.Seq", "Approval.Status", "Approval.Tool",
	"Call.ID", "Call.Input", "Call.Malformed", "Call.Name",
	"Changes.Approvals", "Changes.Run", "Changes.Steps",
	"Limits.MaxCostMicros", "Limits.MaxDuration", "Limits.MaxModelCalls", "Limits.MaxTokens",
	"Message.Calls", "Message.Results", "Message.Role", "Message.Text",
	"Result.CallID", "Result.Content", "Result.IsError",
	"Run.ActiveMillis", "Run.Agent", "Run.CancelBy", "Run.CancelReason", "Run.CancelRequested",
	"Run.CreatedAt", "Run.Definition", "Run.Depth", "Run.Error", "Run.FinishedAt", "Run.Failures",
	"Run.ID", "Run.Input", "Run.Key", "Run.LeaseEpoch", "Run.LeaseExpiresAt", "Run.LeaseOwner",
	"Run.Metadata", "Run.ModelCalls", "Run.NextAttemptAt", "Run.Output", "Run.ParentID", "Run.ParentSeq",
	"Run.Reason", "Run.Rev", "Run.Status", "Run.UpdatedAt", "Run.Usage",
	"Snapshot.Limits", "Snapshot.MaxTokens", "Snapshot.Model", "Snapshot.Output", "Snapshot.System", "Snapshot.Tools",
	"Step.Attempts", "Step.Call", "Step.ChildRunID", "Step.CreatedAt", "Step.Decision", "Step.FinishedAt",
	"Step.IsError", "Step.Key", "Step.Kind", "Step.Message", "Step.Name", "Step.Result", "Step.Rev",
	"Step.Rule", "Step.RunID", "Step.Seq", "Step.StartedAt", "Step.Status", "Step.Stop", "Step.Turn", "Step.Usage",
	"ToolSpec.Description", "ToolSpec.Name", "ToolSpec.Schema",
	"Usage.CostMicros", "Usage.InputTokens", "Usage.OutputTokens",
}

// strippedFields are the fields the API blanks before it serves a value, and
// the test that they are is next to each route's tests.
var strippedFields = []string{
	"Message.Opaque",
}

// reachable lists the fields of t and of every struct of the agent package it
// holds, as "Type.Field". Types of other packages (time.Time, json.RawMessage)
// are leaves: they are what a field holds, not a place for more of them. What a
// stripped field holds is not served, so it is not looked into.
func reachable(t reflect.Type, seen map[reflect.Type]bool, out *[]string) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t.PkgPath() != reflect.TypeFor[agent.Run]().PkgPath() || seen[t] {
		return
	}
	seen[t] = true
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		name := t.Name() + "." + f.Name
		*out = append(*out, name)
		if !slices.Contains(strippedFields, name) {
			reachable(f.Type, seen, out)
		}
	}
}

// A field that is added to a type the API serves is served to every client of
// the timeline and the stream from that moment, unless something is done. Opaque
// is removed by name, so nothing would notice a field that carries the same
// sort of thing: this fails until someone has decided.
func TestServedTypes_HaveOnlyFieldsSomeoneDecidedOn(t *testing.T) {
	var found []string
	seen := map[reflect.Type]bool{}
	for _, root := range []reflect.Type{
		reflect.TypeFor[agent.Run](), reflect.TypeFor[agent.Step](), reflect.TypeFor[agent.Approval](),
		reflect.TypeFor[agent.Changes](), // the timeline, which is served whole
	} {
		reachable(root, seen, &found)
	}
	slices.Sort(found)

	decided := slices.Concat(servedFields, strippedFields)
	slices.Sort(decided)

	var undecided, gone []string
	for _, f := range found {
		if !slices.Contains(decided, f) {
			undecided = append(undecided, f)
		}
	}
	for _, f := range decided {
		if !slices.Contains(found, f) {
			gone = append(gone, f)
		}
	}
	assert.Empty(t, undecided, strings.Join([]string{
		"these fields are on a type the API serves, and nobody has decided whether they may be served.",
		"They would reach every client of the timeline and the stream, as the engine returns them.",
		"Decide first. If a field may be shown to anyone who may read a journal, add it to servedFields.",
		"If it may not (it holds a model's own reasoning, a credential, anything private), blank it on the",
		"copy in withoutOpaque, add a test that it is not served on the timeline and on the stream, and",
		"list it in strippedFields.",
	}, "\n"))
	assert.Empty(t, gone, "these fields are listed here and are not on any served type: remove them from the lists")

	// The walk must have found what it is looking at.
	assert.Contains(t, found, "Message.Opaque")
	assert.Contains(t, found, "Call.Input")
	assert.NotContains(t, found, "Time.wall", "the walk stays in the agent package")
}
