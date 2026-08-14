package main

import (
	"context"
	"encoding/json"
	"strings"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/memory"
)

type memoryRecallHit = memory.RecallHit

func openMemoryService(cfg askConfig) error {
	return memory.Open(memory.Options{})
}

func closeMemoryService() error {
	return memory.Close()
}

func memoryServiceOpen() bool {
	return memory.IsOpen()
}

func sweepOldMemories() {
	_ = memory.Sweep(context.Background())
}

func memoryIndex(ctx context.Context, cwd, text string) error {
	return memory.Index(ctx, cwd, text)
}

func memoryRecall(ctx context.Context, cwd, prompt string, k int) ([]memoryRecallHit, error) {
	return memory.Recall(ctx, cwd, prompt, k)
}

func wrapFileToolsWithMemory(tools []fantasy.AgentTool, cwd string) []fantasy.AgentTool {
	return memory.WrapFileTools(tools, cwd)
}

func agentMemorySystemBlock(cwd string) string {
	return memory.SystemBlock(context.Background(), cwd)
}

func agentMemoryPromptContext(cwd, prompt string) string {
	return memory.PromptContext(context.Background(), cwd, prompt)
}

func fileToolPath(input string) string {
	var in struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return ""
	}
	return strings.TrimSpace(in.FilePath)
}
