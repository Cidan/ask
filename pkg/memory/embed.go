package memory

import (
	"math"
	"strings"
)

// Embedder represents a model capable of generating vector embeddings for text.
//
// The store never loads a model itself: callers hand one in through
// Options.Embedder. cmd/ask uses the llama.cpp model in pkg/memory/llamacpp,
// the one place in the module that links llama.cpp; other consumers bring
// their own, and tests use NewFakeEmbedder.
type Embedder interface {
	Embed(text string) ([]float32, error)
	EmbdSize() int
	Close()
}

// FakeEmbedder is the deterministic test embedder: a hashed bag of
// words, L2-normalised, so texts sharing words land near each other and
// unrelated texts do not.
type FakeEmbedder struct {
	dim int
}

func NewFakeEmbedder(dim int) *FakeEmbedder {
	return &FakeEmbedder{dim: dim}
}

func (f *FakeEmbedder) Embed(text string) ([]float32, error) {
	vec := make([]float32, f.dim)
	for _, word := range strings.Fields(strings.ToLower(text)) {
		word = strings.Trim(word, ".,;:!?\"'()[]{}#-")
		if word == "" {
			continue
		}
		var h uint32 = 2166136261
		for i := 0; i < len(word); i++ {
			h = (h ^ uint32(word[i])) * 16777619
		}
		vec[int(h%uint32(f.dim))] += 1
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v * v)
	}
	if norm == 0 {
		vec[0] = 1
		return vec, nil
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range vec {
		vec[i] *= scale
	}
	return vec, nil
}

func (f *FakeEmbedder) EmbdSize() int { return f.dim }
func (f *FakeEmbedder) Close()        {}
