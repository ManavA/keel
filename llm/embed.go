package llm

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

const (
	// defaultEmbedDims is the vector length of a HashEmbedder built with no
	// positive length.
	defaultEmbedDims = 256
	// hashEmbedderModel is what a HashEmbedder reports as EmbedResponse.Model
	// when the request names no model.
	hashEmbedderModel = "hash"
)

// HashEmbedder is the in-process Embedder. Each word of a text is hashed to a
// dimension and a sign, so texts that share words land near each other. It
// needs no network, key or file, gives the same vector for the same text in
// every process, and is safe for concurrent use. It is for tests and for code
// that needs some vector, not for retrieval quality.
type HashEmbedder struct {
	dims int
}

var _ Embedder = (*HashEmbedder)(nil)

// NewHashEmbedder builds a HashEmbedder producing vectors of dims
// dimensions, 256 when dims is not positive.
func NewHashEmbedder(dims int) *HashEmbedder {
	if dims <= 0 {
		dims = defaultEmbedDims
	}
	return &HashEmbedder{dims: dims}
}

// Embed implements Embedder. A positive EmbedRequest.Dimensions replaces the
// length the embedder was built with. Nothing is billed, so Usage is zero.
func (e *HashEmbedder) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dims := e.dims
	if req.Dimensions > 0 {
		dims = req.Dimensions
	}
	model := req.Model
	if model == "" {
		model = hashEmbedderModel
	}
	vectors := make([][]float32, len(req.Input))
	for i, text := range req.Input {
		vectors[i] = hashVector(text, dims)
	}
	return &EmbedResponse{Model: model, Vectors: vectors}, nil
}

// hashVector is the unit vector of text's words: each is added to the
// dimension its FNV-1a hash picks, with the sign of the hash's top bit. A text
// with no words, or whose words cancel exactly, has no direction, and is the
// zero vector.
func hashVector(text string, dims int) []float32 {
	sums := make([]float64, dims)
	tokens := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, token := range tokens {
		h := fnv.New64a()
		_, _ = h.Write([]byte(token)) // a hash.Hash never fails to write
		sum := h.Sum64()
		sign := 1.0
		if sum>>63 == 1 {
			sign = -1
		}
		sums[sum%uint64(dims)] += sign //nolint:gosec // G115: dims is positive, see NewHashEmbedder and Embed
	}

	var squares float64
	for _, v := range sums {
		squares += v * v
	}
	vec := make([]float32, dims)
	if squares == 0 {
		return vec
	}
	length := math.Sqrt(squares)
	for i, v := range sums {
		vec[i] = float32(v / length)
	}
	return vec
}
