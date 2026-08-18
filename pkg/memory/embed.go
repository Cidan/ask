package memory

/*
#cgo CXXFLAGS: -std=c++11
#cgo CFLAGS: -I${SRCDIR}/../.. -I${SRCDIR}/../../build/llama.cpp/include -I${SRCDIR}/../../build/llama.cpp/ggml/include
#cgo LDFLAGS: -L${SRCDIR}/../../build/llama.cpp/build/src -L${SRCDIR}/../../build/llama.cpp/build/ggml/src -lllama -lggml -lggml-base -lggml-cpu -lstdc++ -fopenmp -lm
#include <stdlib.h>
#include <string.h>
#include "llama.h"
#include "ggml-backend.h"

// Helper function to tokenize
static void empty_log_callback(enum ggml_log_level level, const char * text, void * user_data) {
    // empty
}

static void silence_llama_logs() {
    llama_log_set(empty_log_callback, NULL);
}

static int tokenize(struct llama_model * model, const char * text, int text_len, llama_token * tokens, int n_max_tokens, bool add_bos, bool special) {
    const struct llama_vocab * vocab = llama_model_get_vocab(model);
    return llama_tokenize(vocab, text, text_len, tokens, n_max_tokens, add_bos, special);
}
*/
import "C"
import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// Embedder represents a model capable of generating vector embeddings for text.
type Embedder interface {
	Embed(text string) ([]float32, error)
	EmbdSize() int
	Close()
}

// EmbeddingModel manages an in-memory llama.cpp embedding model instance.
type EmbeddingModel struct {
	model *C.struct_llama_model
	ctx   *C.struct_llama_context
	mu    sync.Mutex
}

// LoadEmbeddingModel initializes the llama.cpp backend and loads the GGUF model from path.
func LoadEmbeddingModel(path string) (*EmbeddingModel, error) {
	C.silence_llama_logs()
	C.ggml_backend_load_all()
	C.llama_backend_init()

	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	mparams := C.llama_model_default_params()
	model := C.llama_model_load_from_file(cPath, mparams)
	if model == nil {
		return nil, errors.New("failed to load llama model")
	}

	cparams := C.llama_context_default_params()
	cparams.n_ctx = 2048
	cparams.n_ubatch = 2048
	cparams.embeddings = C.bool(true)

	ctx := C.llama_init_from_model(model, cparams)
	if ctx == nil {
		C.llama_model_free(model)
		return nil, errors.New("failed to create llama context")
	}

	return &EmbeddingModel{
		model: model,
		ctx:   ctx,
	}, nil
}

// Embed generates an embedding vector for the provided text.
func (m *EmbeddingModel) Embed(text string) ([]float32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.model == nil || m.ctx == nil {
		return nil, errors.New("embedding model is closed")
	}

	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))

	// allocate token buffer
	n_max_tokens := len(text) + 16
	tokens := make([]C.llama_token, n_max_tokens)

	n_tokens := C.tokenize(m.model, cText, C.int(len(text)), &tokens[0], C.int(n_max_tokens), C.bool(true), C.bool(false))
	if n_tokens < 0 {
		return nil, fmt.Errorf("failed to tokenize: %d", int(n_tokens))
	}

	// silently truncate if we exceed our context window (2048)
	if n_tokens > 2048 {
		n_tokens = 2048
	}

	// allocate batch
	batch := C.llama_batch_get_one(&tokens[0], n_tokens)

	// evaluate
	if C.llama_encode(m.ctx, batch) != 0 {
		return nil, errors.New("llama_encode failed")
	}

	// get embeddings for the sequence
	embd := C.llama_get_embeddings_seq(m.ctx, 0)
	if embd == nil {
		return nil, errors.New("failed to get sequence embeddings")
	}

	// The embedding size
	n_embd := int(C.llama_model_n_embd(m.model))

	// Copy to Go slice
	result := make([]float32, n_embd)
	slice := unsafe.Slice((*float32)(unsafe.Pointer(embd)), n_embd)
	copy(result, slice)

	return result, nil
}

// EmbdSize returns the embedding dimension of the model.
func (m *EmbeddingModel) EmbdSize() int {
	if m.model == nil {
		return 0
	}
	return int(C.llama_model_n_embd(m.model))
}

// FakeEmbedder provides a deterministic in-memory embedder for tests.
type FakeEmbedder struct {
	dim int
}

func NewFakeEmbedder(dim int) *FakeEmbedder {
	return &FakeEmbedder{dim: dim}
}

func (f *FakeEmbedder) Embed(text string) ([]float32, error) {
	vec := make([]float32, f.dim)
	var h uint32 = 2166136261
	for i := 0; i < len(text); i++ {
		h = (h ^ uint32(text[i])) * 16777619
	}
	for i := 0; i < f.dim; i++ {
		vec[i] = float32((h+uint32(i*31))%1000) / 1000.0
	}
	return vec, nil
}

func (f *FakeEmbedder) EmbdSize() int { return f.dim }
func (f *FakeEmbedder) Close()        {}

// Close releases the llama context and model resources.
func (m *EmbeddingModel) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx != nil {
		C.llama_free(m.ctx)
		m.ctx = nil
	}
	if m.model != nil {
		C.llama_model_free(m.model)
		m.model = nil
	}
	C.llama_backend_free()
}
