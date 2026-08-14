package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultRecallK is the default limit of memory nodes injected into prompts or tool responses.
	DefaultRecallK = 5

	// DefaultHookTimeout caps how long recall operations can take.
	DefaultHookTimeout = 8 * time.Second
)

// FormatRecallContext formats vector search hits into a numbered markdown list under heading.
func FormatRecallContext(hits []RecallHit, heading string) string {
	lines := make([]string, 0, len(hits))
	for _, h := range hits {
		text := strings.TrimSpace(h.Text)
		if text == "" {
			continue
		}
		lines = append(lines, text)
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", heading)
	for i, text := range lines {
		fmt.Fprintf(&b, "%d. %s\n", i+1, text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SystemBlock returns the session-start project context memory block.
func SystemBlock(ctx context.Context, cwd string) string {
	if !IsOpen() {
		return ""
	}
	recallCtx, cancel := context.WithTimeout(ctx, DefaultHookTimeout)
	defer cancel()

	hits, err := Recall(recallCtx, cwd, "current project context", DefaultRecallK)
	if err != nil {
		return ""
	}
	return FormatRecallContext(hits, "Project memory")
}

// PromptContext returns the per-turn memory recall block matching the user prompt.
func PromptContext(ctx context.Context, cwd, prompt string) string {
	if !IsOpen() || strings.TrimSpace(prompt) == "" {
		return ""
	}
	recallCtx, cancel := context.WithTimeout(ctx, DefaultHookTimeout)
	defer cancel()

	hits, err := Recall(recallCtx, cwd, prompt, DefaultRecallK)
	if err != nil {
		return ""
	}
	return FormatRecallContext(hits, "Relevant memory")
}
