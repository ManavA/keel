package pg_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// The encoders write JSON by hand, so that raw values go in as the bytes
// given. These tests hold them to encoding/json for everything else: what
// they write reads back, through json.Unmarshal, as what json.Marshal would
// have written.

// fullMessage, fullCall and fullSnapshot have every field set, at every
// depth, which requireEveryFieldSet checks. A field added to one of these
// types and not to its encoder leaves the fixture with a field that is not
// set, and the test says so before the encoder silently drops it.
var (
	fullCall = agent.Call{ID: "call-1", Name: "send", Input: raw(`{"to":"a"}`), Malformed: true}

	fullMessage = agent.Message{
		Role:    agent.RoleAssistant,
		Text:    "reading it now",
		Calls:   []agent.Call{fullCall, {ID: "call-2", Name: "lookup", Input: raw(`[1,2]`), Malformed: true}},
		Results: []agent.Result{{CallID: "call-0", Content: "found", IsError: true}},
		Opaque:  &agent.Opaque{Provider: "provider-a", Data: raw(`{"z":1,"a":2}`)},
	}

	fullSnapshot = agent.Snapshot{
		System:    "Review one document.",
		Model:     "model-a",
		Tools:     []agent.ToolSpec{{Name: "lookup", Description: "Looks a thing up.", Schema: raw(`{"type":"object"}`)}},
		Output:    raw(`{"type":"string"}`),
		MaxTokens: 1024,
		Limits:    agent.Limits{MaxDuration: 15 * time.Minute, MaxCostMicros: 5_000_000, MaxTokens: -1, MaxModelCalls: 50},
	}
)

// requireEveryFieldSet fails for a zero field anywhere in v.
func requireEveryFieldSet(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			requireEveryFieldSet(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Pointer:
		require.False(t, v.IsNil(), "%s is not set: the encoder may not write it", path)
		requireEveryFieldSet(t, v.Elem(), path)
	case reflect.Slice:
		require.NotZero(t, v.Len(), "%s is not set: the encoder may not write it", path)
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		for i := range v.Len() {
			requireEveryFieldSet(t, v.Index(i), path)
		}
	default:
		require.False(t, v.IsZero(), "%s is not set: the encoder may not write it", path)
	}
}

// encodesAsJSON asserts that text, which an encoder wrote for v, is JSON
// that reads back as v and says what json.Marshal says of v.
func encodesAsJSON[T any](t *testing.T, name string, v T, text string, err error) {
	t.Helper()
	require.NoError(t, err, name)
	require.True(t, json.Valid([]byte(text)), "%s: %s", name, text)

	var back T
	require.NoError(t, json.Unmarshal([]byte(text), &back), name)
	standard, err := json.Marshal(v)
	require.NoError(t, err, name)
	var want T
	require.NoError(t, json.Unmarshal(standard, &want), name)
	assert.Equal(t, want, back, "%s: read back", name)
	assert.JSONEq(t, string(standard), text, name)
}

func TestEncodeMessage(t *testing.T) {
	requireEveryFieldSet(t, reflect.ValueOf(fullMessage), "Message")

	messages := []struct {
		name    string
		message agent.Message
	}{
		{"every field set", fullMessage},
		{"nothing set", agent.Message{}},
		{"a final answer", agent.Message{Role: agent.RoleAssistant, Text: "all done"}},
		{"calls and no text", agent.Message{Role: agent.RoleAssistant, Calls: []agent.Call{{ID: "c", Name: "n", Input: raw(`{}`)}}}},
		{"empty lists", agent.Message{Role: agent.RoleAssistant, Calls: []agent.Call{}, Results: []agent.Result{}}},
		{"results that are not errors", agent.Message{Role: agent.RoleTool, Results: []agent.Result{{CallID: "c"}, {CallID: "d", Content: "x"}}}},
		{"text JSON has to escape", agent.Message{
			Role: agent.RoleUser,
			Text: "quote \" slash \\ tab \t line \n nul \x00 markup <a&b> sep   and café \U0001F600",
		}},
		{"an opaque turn with no data", agent.Message{Role: agent.RoleAssistant, Opaque: &agent.Opaque{Provider: "p", Data: raw(`null`)}}},
	}
	for _, m := range messages {
		text, err := agentpg.EncodeMessage(m.message)
		encodesAsJSON(t, m.name, m.message, text, err)
	}

	t.Run("a raw value goes in as the bytes given", func(t *testing.T) {
		text, err := agentpg.EncodeMessage(agent.Message{
			Role:   agent.RoleAssistant,
			Calls:  []agent.Call{{ID: "c", Name: "n", Input: raw(`{"b": "<&>",  "a":1}`)}},
			Opaque: &agent.Opaque{Provider: "p", Data: raw(`{ "z":1, "a":2 }`)},
		})
		require.NoError(t, err)
		assert.Equal(t,
			`{"role":"assistant","calls":[{"id":"c","name":"n","input":{"b": "<&>",  "a":1}}],"opaque":{"provider":"p","data":{ "z":1, "a":2 }}}`,
			text)
	})

	t.Run("no raw value is written as null", func(t *testing.T) {
		text, err := agentpg.EncodeMessage(agent.Message{
			Calls:  []agent.Call{{ID: "c", Name: "n"}, {ID: "d", Name: "n", Input: json.RawMessage{}}},
			Opaque: &agent.Opaque{Provider: "p"},
		})
		require.NoError(t, err)
		assert.JSONEq(t,
			`{"role":"","calls":[{"id":"c","name":"n","input":null},{"id":"d","name":"n","input":null}],"opaque":{"provider":"p","data":null}}`,
			text)
	})

	t.Run("a byte that is not UTF-8 in a string is the replacement character", func(t *testing.T) {
		text, err := agentpg.EncodeMessage(agent.Message{Role: agent.RoleAssistant, Text: "caf\xff"})
		require.NoError(t, err)
		var back agent.Message
		require.NoError(t, json.Unmarshal([]byte(text), &back))
		assert.Equal(t, "caf�", back.Text)
	})

	t.Run("a raw value that is not JSON is an error that says where", func(t *testing.T) {
		_, err := agentpg.EncodeMessage(agent.Message{Calls: []agent.Call{{ID: "c"}, {ID: "d", Input: raw(`{"a":`)}}})
		require.ErrorContains(t, err, "call 2")
		_, err = agentpg.EncodeMessage(agent.Message{Opaque: &agent.Opaque{Data: raw("{\"a\":\"\xff\"}")}})
		require.ErrorContains(t, err, "opaque")
		require.ErrorContains(t, err, "UTF-8")
	})
}

func TestEncodeCall(t *testing.T) {
	requireEveryFieldSet(t, reflect.ValueOf(fullCall), "Call")

	calls := []struct {
		name string
		call agent.Call
	}{
		{"every field set", fullCall},
		{"arguments that parsed", agent.Call{ID: "call-1", Name: "send", Input: raw(`{"to":"a","n":2}`)}},
		{"malformed arguments, held as one string", agent.Call{ID: "call-1", Name: "send", Input: raw(`"{\"id\":"`), Malformed: true}},
		{"a name JSON has to escape", agent.Call{ID: "a\"b", Name: "x\\y\x00", Input: raw(`1`)}},
	}
	for _, c := range calls {
		text, err := agentpg.EncodeCall(c.call)
		encodesAsJSON(t, c.name, c.call, text, err)
	}
}

func TestEncodeSnapshot(t *testing.T) {
	requireEveryFieldSet(t, reflect.ValueOf(fullSnapshot), "Snapshot")

	snapshots := []struct {
		name     string
		snapshot agent.Snapshot
	}{
		{"every field set", fullSnapshot},
		{"nothing set", agent.Snapshot{}},
		{"a prompt and nothing else", agent.Snapshot{System: "system"}},
		{"tools with no schema and no description", agent.Snapshot{Tools: []agent.ToolSpec{{Name: "a"}, {Name: "b", Description: "d"}}}},
		{"an empty list of tools", agent.Snapshot{Tools: []agent.ToolSpec{}}},
		{"limits at their extremes", agent.Snapshot{Limits: agent.Limits{
			MaxDuration: -1, MaxCostMicros: 1<<63 - 1, MaxTokens: -1 << 63, MaxModelCalls: -1,
		}, MaxTokens: -5}},
	}
	for _, s := range snapshots {
		text, err := agentpg.EncodeSnapshot(s.snapshot)
		encodesAsJSON(t, s.name, s.snapshot, text, err)
	}

	t.Run("a schema goes in as the bytes given", func(t *testing.T) {
		text, err := agentpg.EncodeSnapshot(agent.Snapshot{
			System: "s",
			Tools:  []agent.ToolSpec{{Name: "a", Schema: raw(`{"type": "object"}`)}},
			Output: raw(`{"enum": ["<", ">"]}`),
		})
		require.NoError(t, err)
		assert.Equal(t,
			`{"system":"s","tools":[{"name":"a","schema":{"type": "object"}}],"output":{"enum": ["<", ">"]},`+
				`"limits":{"max_duration_ns":0,"max_cost_micros":0,"max_tokens":0,"max_model_calls":0}}`,
			text)
	})

	t.Run("a schema that is not JSON is an error that names the tool", func(t *testing.T) {
		_, err := agentpg.EncodeSnapshot(agent.Snapshot{Tools: []agent.ToolSpec{{Name: "lookup", Schema: raw(`{`)}}})
		require.ErrorContains(t, err, `"lookup"`)
		_, err = agentpg.EncodeSnapshot(agent.Snapshot{Output: raw(`nope`)})
		require.ErrorContains(t, err, "output")
	})
}
