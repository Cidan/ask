.PHONY: all build clean setup-llama download-model

# Variables
LLAMA_DIR := build/llama.cpp
LLAMA_URL := https://github.com/ggerganov/llama.cpp.git
# Pin to a known good commit for stability if needed, using master for now
LLAMA_BRANCH := master

# Model details
MODEL_URL := https://huggingface.co/ggml-org/embeddinggemma-300M-GGUF/resolve/main/embeddinggemma-300M-Q8_0.gguf
MODEL_DIR := build/models
MODEL_FILE := $(MODEL_DIR)/embeddinggemma-300M-Q8_0.gguf

all: setup-llama download-model build

setup-llama:
	@mkdir -p build
	@if [ ! -d "$(LLAMA_DIR)" ]; then \
		echo "Cloning llama.cpp..."; \
		git clone --branch $(LLAMA_BRANCH) $(LLAMA_URL) $(LLAMA_DIR); \
	fi
	@echo "Building llama.cpp static libraries..."
	@cd $(LLAMA_DIR) && cmake -B build -DBUILD_SHARED_LIBS=OFF -DCMAKE_POSITION_INDEPENDENT_CODE=ON && cmake --build build --config Release -j 4

download-model:
	@mkdir -p $(MODEL_DIR)
	@if [ ! -f "$(MODEL_FILE)" ]; then \
		echo "Downloading Gemma embedding model..."; \
		curl -L -o $(MODEL_FILE) $(MODEL_URL); \
	fi

build: setup-llama download-model
	@echo "Building ask..."
	CGO_LDFLAGS="-L$(PWD)/$(LLAMA_DIR)/build/src -L$(PWD)/$(LLAMA_DIR)/build/ggml/src -lllama -lggml -lstdc++ -lm" \
	CGO_CXXFLAGS="-I$(PWD)/$(LLAMA_DIR)/include -I$(PWD)/$(LLAMA_DIR)/ggml/include" \
	CGO_CFLAGS="-I$(PWD)" \
	go build -o bin/ask .

test: setup-llama download-model
	@echo "Testing ask..."
	CGO_LDFLAGS="-L$(PWD)/$(LLAMA_DIR)/build/src -L$(PWD)/$(LLAMA_DIR)/build/ggml/src -lllama -lggml -lstdc++ -lm" \
	CGO_CXXFLAGS="-I$(PWD)/$(LLAMA_DIR)/include -I$(PWD)/$(LLAMA_DIR)/ggml/include" \
	CGO_CFLAGS="-I$(PWD)" \
	go test ./...

clean:
	rm -rf build/llama.cpp bin/ask
