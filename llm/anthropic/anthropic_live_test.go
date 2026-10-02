//go:build live

package anthropic_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/anthropic"
)

// liveModel is the cheapest current model, which is all these two calls need.
const liveModel = "claude-haiku-4-5-20251001"

// liveClient builds a Client for the real Messages API. It requires:
//
//	ANTHROPIC_API_KEY  a valid API key
//
// Run with: go test -tags=live ./llm/anthropic/ -run Live
//
// This never runs in CI: there is no key CI could safely hold. It exists so
// a change to the request or response mapping has one command that proves it
// against the real service before it ships.
func liveClient(t *testing.T) *anthropic.Client {
	t.Helper()
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set: skipping live Anthropic test")
	}
	c, err := anthropic.New(anthropic.Options{APIKey: key, Model: liveModel})
	require.NoError(t, err)
	return c
}

// TestClient_Live_GenerateWithATool asks a question only the tool can answer
// and sends the tool's result back, which also proves the assistant turn is
// accepted when it is replayed from Opaque.
func TestClient_Live_GenerateWithATool(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	req := llm.Request{
		System:    "Use the tool to answer. Do not guess.",
		MaxTokens: 1024,
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: "What is the weather in San Francisco, CA right now?"}},
		Tools: []llm.Tool{{
			Name:        "get_weather",
			Description: "Get the current weather in a given location",
			Schema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string",` +
				`"description":"The city and state, e.g. San Francisco, CA"}},"required":["location"]}`),
		}},
	}

	first, err := c.Generate(ctx, req)
	require.NoError(t, err)
	require.Equal(t, llm.StopToolUse, first.Stop)
	require.Len(t, first.Message.ToolCalls, 1)
	call := first.Message.ToolCalls[0]
	assert.Equal(t, "get_weather", call.Name)
	assert.True(t, json.Valid(call.Input))
	require.NotNil(t, first.Message.Opaque)
	assert.Positive(t, first.Usage.InputTokens)
	assert.Positive(t, first.Usage.OutputTokens)

	req.Messages = append(req.Messages, first.Message, llm.Message{
		Role:        llm.RoleTool,
		ToolResults: []llm.ToolResult{{CallID: call.ID, Content: "15 degrees Celsius, clear"}},
	})
	second, err := c.Generate(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, llm.StopEnd, second.Stop)
	assert.Contains(t, second.Message.Text, "15")
}

// TestClient_Live_Stream checks that the deltas add up to the reply.
func TestClient_Live_Stream(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var text string
	resp, err := c.Stream(ctx, llm.Request{
		MaxTokens: 256,
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: "Reply with the single word: pong"}},
	}, func(d llm.Delta) error {
		text += d.Text
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, llm.StopEnd, resp.Stop)
	assert.NotEmpty(t, resp.Message.Text)
	assert.Equal(t, resp.Message.Text, text)
	assert.NotEmpty(t, resp.ID)
	assert.Positive(t, resp.Usage.OutputTokens)
	require.NotNil(t, resp.Message.Opaque)
}
