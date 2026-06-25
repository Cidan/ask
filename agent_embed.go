package main

/*
#cgo CXXFLAGS: -std=c++11
#cgo CFLAGS: -I${SRCDIR}/build/llama.cpp/include -I${SRCDIR}/build/llama.cpp/ggml/include
#cgo LDFLAGS: -L${SRCDIR}/build/llama.cpp/build/src -L${SRCDIR}/build/llama.cpp/build/ggml/src -lllama -lggml -lggml-base -lggml-cpu -lstdc++ -fopenmp -lm
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

type EmbeddingModel struct {
	model *C.struct_llama_model
	ctx   *C.struct_llama_context
	mu    sync.Mutex
}

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

func (m *EmbeddingModel) Embed(text string) ([]float32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

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

func (m *EmbeddingModel) EmbdSize() int {
	return int(C.llama_model_n_embd(m.model))
}

func (m *EmbeddingModel) Close() {
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
