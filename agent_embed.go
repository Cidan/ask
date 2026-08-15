package main

import (
	"github.com/Cidan/ask/pkg/memory"
)

// EmbeddingModel is an alias to memory.EmbeddingModel for backward compatibility.
type EmbeddingModel = memory.EmbeddingModel

// LoadEmbeddingModel delegates to memory.LoadEmbeddingModel.
func LoadEmbeddingModel(path string) (*EmbeddingModel, error) {
	return memory.LoadEmbeddingModel(path)
}
