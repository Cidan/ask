.PHONY: all build clean setup-llama download-model install

# Variables
LLAMA_DIR := build/llama.cpp
LLAMA_URL := https://github.com/ggerganov/llama.cpp.git
# Pin to a known good commit for stability if needed, using master for now
LLAMA_BRANCH := master

# Model details
MODEL_URL := https://huggingface.co/ggml-org/embeddinggemma-300M-GGUF/resolve/main/embeddinggemma-300M-Q8_0.gguf
MODEL_DIR := $(HOME)/.local/share/ask/models
MODEL_FILE := $(MODEL_DIR)/embeddinggemma-300M-Q8_0.gguf

export CGO_LDFLAGS=-L$(PWD)/$(LLAMA_DIR)/build/src -L$(PWD)/$(LLAMA_DIR)/build/ggml/src -lllama -lggml -lstdc++ -lm
export CGO_CXXFLAGS=-I$(PWD)/$(LLAMA_DIR)/include -I$(PWD)/$(LLAMA_DIR)/ggml/include
export CGO_CFLAGS=-I$(PWD)/third_party/sqlite

all: setup-llama download-model build

# A worktree reaches build/ through a symlink to the primary checkout's, and
# CMake records the path it was configured from, so llama.cpp is configured
# through the physical path every checkout shares, and a cache recorded under
# any other path is discarded rather than failing the build.
setup-llama:
	@mkdir -p build
	@if [ ! -d "$(LLAMA_DIR)" ]; then \
		echo "Cloning llama.cpp..."; \
		git clone --branch $(LLAMA_BRANCH) $(LLAMA_URL) $(LLAMA_DIR); \
	fi
	@echo "Building llama.cpp static libraries..."
	@src=$$(cd $(LLAMA_DIR) && pwd -P); \
	cache="$$src/build/CMakeCache.txt"; \
	if [ -f "$$cache" ] && ! grep -qxF "CMAKE_CACHEFILE_DIR:INTERNAL=$$src/build" "$$cache"; then \
		echo "Discarding a llama.cpp CMake cache configured from another path..."; \
		rm -rf "$$cache" "$$src/build/CMakeFiles"; \
	fi; \
	cd "$$src" && cmake -S "$$src" -B "$$src/build" -DBUILD_SHARED_LIBS=OFF -DCMAKE_POSITION_INDEPENDENT_CODE=ON && cmake --build "$$src/build" --config Release -j 4

download-model:
	@mkdir -p $(MODEL_DIR)
	@if [ ! -f "$(MODEL_FILE)" ]; then \
		echo "Downloading Gemma embedding model..."; \
		curl -L -o $(MODEL_FILE) $(MODEL_URL); \
	fi

build: setup-llama download-model
	@echo "Building ask..."
	go build -o bin/ask ./cmd/ask

install: setup-llama download-model
	@echo "Installing ask..."
	go install ./cmd/ask

test: setup-llama download-model
	@echo "Testing ask..."
	go test ./...

clean:
	rm -rf build/llama.cpp bin/ask