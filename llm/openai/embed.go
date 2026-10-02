package openai

import (
	"context"
	"errors"
	"fmt"

	"github.com/ManavA/keel/llm"
)

// embedRequest is the body of POST /embeddings. The vectors are asked for as
// floats, which the reference makes the default and a server may not know to
// default.
type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	Dimensions     int      `json:"dimensions,omitempty"`
}

// wireEmbeddings is a list of embeddings.
type wireEmbeddings struct {
	Model string          `json:"model"`
	Data  []wireEmbedding `json:"data"`
	Usage *struct {
		PromptTokens int64 `json:"prompt_tokens"`
	} `json:"usage"`
}

type wireEmbedding struct {
	Index     *int      `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// Embed implements llm.Embedder.
func (c *Client) Embed(ctx context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	model := req.Model
	if model == "" {
		model = c.embeddingModel
	}
	if model == "" {
		return nil, errors.New("openai: no embedding model: set EmbedRequest.Model or Options.EmbeddingModel")
	}
	if len(req.Input) == 0 {
		return &llm.EmbedResponse{Model: model, Vectors: [][]float32{}}, nil
	}

	body := embedRequest{Model: model, Input: req.Input, EncodingFormat: "float"}
	if req.Dimensions > 0 {
		body.Dimensions = req.Dimensions
	}
	raw, err := encode(body)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, "/embeddings", raw)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var reply wireEmbeddings
	if err := c.readJSON(ctx, resp.Body, &reply); err != nil {
		return nil, err
	}
	vectors, err := orderVectors(reply.Data, len(req.Input))
	if err != nil {
		return nil, err
	}

	out := &llm.EmbedResponse{Model: reply.Model, Vectors: vectors}
	if out.Model == "" {
		out.Model = model
	}
	if reply.Usage != nil {
		out.Usage.InputTokens = reply.Usage.PromptTokens
	}
	return out, nil
}

// orderVectors puts the vectors in input order by their index. A server that
// sends no index is read in the order it sent. It is an error for the reply
// to hold other than one vector per input, or an index twice or out of range.
func orderVectors(data []wireEmbedding, inputs int) ([][]float32, error) {
	if len(data) != inputs {
		return nil, fmt.Errorf("openai: embeddings reply has %d vector(s) for %d input(s)", len(data), inputs)
	}
	positional := false
	for _, d := range data {
		positional = positional || d.Index == nil
	}

	vectors := make([][]float32, inputs)
	taken := make([]bool, inputs)
	for pos, d := range data {
		idx := pos
		if !positional {
			idx = *d.Index
		}
		switch {
		case idx < 0 || idx >= inputs:
			return nil, fmt.Errorf("openai: embeddings reply has index %d for %d input(s)", idx, inputs)
		case taken[idx]:
			return nil, fmt.Errorf("openai: embeddings reply has index %d twice", idx)
		}
		taken[idx] = true
		vectors[idx] = d.Embedding
	}
	return vectors, nil
}
