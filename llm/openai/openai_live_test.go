//go:build live

package openai_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/openai"
)

// liveClient builds a client for the server OPENAI_BASE_URL names, which may
// be OpenAI or a local runtime that speaks its protocol. It requires:
//
//	OPENAI_BASE_URL         the endpoint, such as http://localhost:11434/v1
//	OPENAI_MODEL            a chat model the server has
//
// and reads, when set:
//
//	OPENAI_API_KEY          sent as a bearer token; a local runtime wants none
//	OPENAI_EMBEDDING_MODEL  an embedding model; the embeddings test skips without one
func liveClient(t *testing.T) *openai.Client {
	t.Helper()
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		t.Skip("OPENAI_BASE_URL not set: skipping live OpenAI-compatible test")
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		t.Skip("OPENAI_MODEL not set: skipping live OpenAI-compatible test")
	}
	c, err := openai.New(openai.Options{
		BaseURL:        baseURL,
		APIKey:         os.Getenv("OPENAI_API_KEY"),
		Model:          model,
		EmbeddingModel: os.Getenv("OPENAI_EMBEDDING_MODEL"),
		MaxTokens:      512,
	})
	require.NoError(t, err)
	return c
}

var liveWeatherTool = llm.Tool{
	Name:        "get_current_weather",
	Description: "Get the current weather in a given location",
	Schema: json.RawMessage(`{"type":"object","properties":{` +
		`"location":{"type":"string","description":"The city and state, e.g. San Francisco, CA"}},` +
		`"required":["location"]}`),
}

// TestClient_Live asks one question that wants a tool, sends the tool's
// answer back, and asks again as a stream. It needs a server that can call a
// tool; a model that answers in words instead still passes the first call.
//
// Run with: go test -tags=live ./llm/openai/ -run Live
//
// This never runs in CI, which has no server to ask.
func TestClient_Live(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	t.Run("generate with a tool", func(t *testing.T) {
		req := llm.Request{
			System:   "You are a terse assistant. Use a tool when it helps.",
			Messages: []llm.Message{{Role: llm.RoleUser, Text: "What is the weather like in Boston today?"}},
			Tools:    []llm.Tool{liveWeatherTool},
		}
		resp, err := c.Generate(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, llm.RoleAssistant, resp.Message.Role)
		assert.NotEmpty(t, resp.Model)
		assert.Positive(t, resp.Usage.Total(), "the server reported usage")

		if len(resp.Message.ToolCalls) == 0 {
			assert.NotEmpty(t, resp.Message.Text, "neither a call nor words")
			t.Skip("the model answered in words and made no call; the round trip is not exercised")
		}
		call := resp.Message.ToolCalls[0]
		assert.Equal(t, liveWeatherTool.Name, call.Name)
		assert.True(t, json.Valid(call.Input))

		req.Messages = append(req.Messages, resp.Message, llm.Message{
			Role:        llm.RoleTool,
			ToolResults: []llm.ToolResult{{CallID: call.ID, Content: "Sunny, 21 degrees Celsius"}},
		})
		next, err := c.Generate(ctx, req)
		require.NoError(t, err)
		assert.NotEmpty(t, next.Message.Text)
	})

	t.Run("stream", func(t *testing.T) {
		var text strings.Builder
		resp, err := c.Stream(ctx, llm.Request{
			Messages: []llm.Message{{Role: llm.RoleUser, Text: "Count from one to five in words."}},
		}, func(d llm.Delta) error {
			text.WriteString(d.Text)
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, text.String(), resp.Message.Text, "the deltas are the reply")
		assert.NotEmpty(t, resp.Message.Text)
	})

	t.Run("embed", func(t *testing.T) {
		if os.Getenv("OPENAI_EMBEDDING_MODEL") == "" {
			t.Skip("OPENAI_EMBEDDING_MODEL not set: skipping the embeddings call")
		}
		resp, err := c.Embed(ctx, llm.EmbedRequest{Input: []string{"the first text", "a second, longer text"}})
		require.NoError(t, err)
		require.Len(t, resp.Vectors, 2)
		assert.NotEmpty(t, resp.Vectors[0])
		assert.Len(t, resp.Vectors[1], len(resp.Vectors[0]))
	})
}
