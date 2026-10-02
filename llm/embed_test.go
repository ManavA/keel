package llm_test

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// embedText embeds one text with e and returns its vector.
func embedText(t *testing.T, e *llm.HashEmbedder, text string) []float32 {
	t.Helper()
	resp, err := e.Embed(t.Context(), llm.EmbedRequest{Input: []string{text}})
	require.NoError(t, err)
	require.Len(t, resp.Vectors, 1)
	return resp.Vectors[0]
}

func dotProduct(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func vectorLength(v []float32) float64 { return math.Sqrt(dotProduct(v, v)) }

// sparseVector is a vector of dims zeros but for the entries in at.
func sparseVector(dims int, at map[int]float32) []float32 {
	v := make([]float32, dims)
	for i, x := range at {
		v[i] = x
	}
	return v
}

func TestHashEmbedder_SameTextSameVector(t *testing.T) {
	e := llm.NewHashEmbedder(0)

	t.Run("on one embedder", func(t *testing.T) {
		assert.Equal(t, embedText(t, e, "reset my password"), embedText(t, e, "reset my password"))
	})

	t.Run("on two embedders", func(t *testing.T) {
		other := llm.NewHashEmbedder(0)
		assert.Equal(t, embedText(t, e, "reset my password"), embedText(t, other, "reset my password"))
	})

	t.Run("whatever the case or the punctuation between the words", func(t *testing.T) {
		want := embedText(t, e, "hello world")
		for _, text := range []string{"Hello, World!", "HELLO   world", "hello-world", "\thello\nworld\n", "(hello) \"world\""} {
			assert.Equal(t, want, embedText(t, e, text), text)
		}
	})

	t.Run("different words give a different vector", func(t *testing.T) {
		assert.NotEqual(t, embedText(t, e, "reset my password"), embedText(t, e, "forecast the quarter"))
	})
}

// These vectors were worked out by hand from FNV-1a over each word's bytes,
// not by running the embedder: a word's dimension is its 64-bit hash modulo
// the dimensions and its sign is the hash's top bit. They pin the hash, so a
// vector stored today still means the same thing in every later process.
func TestHashEmbedder_GoldenVectors(t *testing.T) {
	root2 := float32(math.Sqrt2 / 2)
	tests := []struct {
		name string
		dims int
		text string
		want []float32
	}{
		{name: "one word, negative", dims: 16, text: "hello", want: sparseVector(16, map[int]float32{11: -1})},
		{name: "one word, positive", dims: 16, text: "world", want: sparseVector(16, map[int]float32{3: 1})},
		{
			name: "two words in two dimensions", dims: 16, text: "hello world",
			want: sparseVector(16, map[int]float32{11: -root2, 3: root2}),
		},
		{name: "a repeated word is the same direction", dims: 16, text: "hello hello", want: sparseVector(16, map[int]float32{11: -1})},
		{name: "256 dimensions", dims: 256, text: "hello", want: sparseVector(256, map[int]float32{11: -1})},
		{name: "256 dimensions, another word", dims: 256, text: "world", want: sparseVector(256, map[int]float32{243: 1})},
		{name: "one dimension, negative", dims: 1, text: "hello", want: []float32{-1}},
		{name: "one dimension, positive", dims: 1, text: "world", want: []float32{1}},
		{
			name: "two words of one sign in one dimension add", dims: 1, text: "world keel",
			want: []float32{1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := embedText(t, llm.NewHashEmbedder(tt.dims), tt.text)
			assert.InDeltaSlice(t, tt.want, got, 1e-6)
		})
	}
}

func TestHashEmbedder_SharedWordsAreCloser(t *testing.T) {
	tests := []struct {
		name  string
		query string
		near  string
		far   string
	}{
		{
			name:  "most words shared against none",
			query: "the quick brown fox jumps over the lazy dog",
			near:  "the quick brown fox leaps over the lazy dog",
			far:   "pack my box with five dozen liquor jugs",
		},
		{
			name:  "a reworded request",
			query: "Reset my password please",
			near:  "please reset the password",
			far:   "quarterly revenue forecast",
		},
	}
	e := llm.NewHashEmbedder(0)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := embedText(t, e, tt.query)
			near := dotProduct(q, embedText(t, e, tt.near))
			far := dotProduct(q, embedText(t, e, tt.far))
			assert.Greater(t, near, far)
			assert.Greater(t, near, 0.5, "texts that share most words are close")
			assert.Less(t, far, 0.2, "texts that share none are nearly orthogonal")
			assert.InDelta(t, 1, dotProduct(q, q), 1e-5, "a text is closest to itself")
		})
	}
}

func TestHashEmbedder_NonEmptyVectorsHaveLengthOne(t *testing.T) {
	texts := []string{
		"hello",
		"hello hello hello",
		"the quick brown fox jumps over the lazy dog",
		"Größe naïve café 日本語 данные",
		"42 x 7 = 294",
		strings.Repeat("lorem ipsum dolor sit amet ", 400),
	}
	for _, dims := range []int{3, 16, 256, 1000} {
		e := llm.NewHashEmbedder(dims)
		for _, text := range texts {
			v := embedText(t, e, text)
			require.Len(t, v, dims)
			assert.InDelta(t, 1, vectorLength(v), 1e-5, "dims %d: %.30q", dims, text)
		}
	}
}

func TestHashEmbedder_EmptyTextIsTheZeroVector(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "no text", text: ""},
		{name: "one space", text: " "},
		{name: "white space of every kind", text: "\n\t  \r\n"},
		{name: "punctuation only", text: "!!! --- ???"},
		{name: "an ellipsis", text: "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := embedText(t, llm.NewHashEmbedder(0), tt.text)
			assert.Equal(t, make([]float32, 256), v, "all zero, and not a NaN")
		})
	}
}

// Two words that land in one dimension with opposite signs sum to nothing, so
// the text has no direction. That is the zero vector and not a division by it.
func TestHashEmbedder_WordsThatCancelGiveTheZeroVector(t *testing.T) {
	tests := []struct {
		name string
		dims int
		text string
	}{
		{name: "two words, one dimension", dims: 1, text: "hello world"},
		{name: "two words that share a dimension of sixteen", dims: 16, text: "go hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, make([]float32, tt.dims), embedText(t, llm.NewHashEmbedder(tt.dims), tt.text))
		})
	}
}

func TestHashEmbedder_Dimensions(t *testing.T) {
	tests := []struct {
		name    string
		dims    int
		request int
		want    int
	}{
		{name: "zero is 256", dims: 0, request: 0, want: 256},
		{name: "negative is 256", dims: -5, request: 0, want: 256},
		{name: "the dims given", dims: 64, request: 0, want: 64},
		{name: "the request overrides the dims given", dims: 64, request: 32, want: 32},
		{name: "the request overrides the default", dims: 0, request: 512, want: 512},
		{name: "a negative request changes nothing", dims: 64, request: -1, want: 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := llm.NewHashEmbedder(tt.dims)
			resp, err := e.Embed(t.Context(), llm.EmbedRequest{Input: []string{"hello world", ""}, Dimensions: tt.request})
			require.NoError(t, err)
			require.Len(t, resp.Vectors, 2)
			assert.Len(t, resp.Vectors[0], tt.want)
			assert.Len(t, resp.Vectors[1], tt.want)
		})
	}
}

func TestHashEmbedder_Embed(t *testing.T) {
	e := llm.NewHashEmbedder(32)

	t.Run("one vector per input, in input order", func(t *testing.T) {
		inputs := []string{"alpha beta", "", "gamma", "alpha beta", "delta epsilon zeta"}
		resp, err := e.Embed(t.Context(), llm.EmbedRequest{Input: inputs})
		require.NoError(t, err)
		require.Len(t, resp.Vectors, len(inputs))
		for i, text := range inputs {
			assert.Equal(t, embedText(t, e, text), resp.Vectors[i], "input %d", i)
		}
	})

	t.Run("no input is no vectors", func(t *testing.T) {
		resp, err := e.Embed(t.Context(), llm.EmbedRequest{})
		require.NoError(t, err)
		assert.NotNil(t, resp.Vectors)
		assert.Empty(t, resp.Vectors)
	})

	t.Run("the model named, or a name of its own", func(t *testing.T) {
		resp, err := e.Embed(t.Context(), llm.EmbedRequest{Input: []string{"a"}})
		require.NoError(t, err)
		assert.Equal(t, "hash", resp.Model)

		resp, err = e.Embed(t.Context(), llm.EmbedRequest{Model: "text-embedding-3-small", Input: []string{"a"}})
		require.NoError(t, err)
		assert.Equal(t, "text-embedding-3-small", resp.Model)
	})

	t.Run("nothing is billed", func(t *testing.T) {
		resp, err := e.Embed(t.Context(), llm.EmbedRequest{Input: []string{"some text"}})
		require.NoError(t, err)
		assert.Equal(t, llm.Usage{}, resp.Usage)
	})

	t.Run("a cancelled context is an error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		resp, err := e.Embed(ctx, llm.EmbedRequest{Input: []string{"a"}})
		assert.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, resp)
	})

	t.Run("vectors are the caller's to change", func(t *testing.T) {
		first := embedText(t, e, "alpha beta")
		for i := range first {
			first[i] = 7
		}
		assert.NotEqual(t, first, embedText(t, e, "alpha beta"))
	})
}

func TestHashEmbedder_IsSafeForConcurrentUse(t *testing.T) {
	e := llm.NewHashEmbedder(0)
	want := embedText(t, e, "the same text from every goroutine")

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			resp, err := e.Embed(t.Context(), llm.EmbedRequest{Input: []string{"the same text from every goroutine"}})
			assert.NoError(t, err)
			assert.Equal(t, [][]float32{want}, resp.Vectors)
		})
	}
	wg.Wait()
}
