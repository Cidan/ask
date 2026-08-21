package tools

import (
	"context"
	"strings"

	"github.com/Cidan/ask/pkg/memory"
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/preloadmemorytool"
)

type loadMemoryParams struct {
	Query string `json:"query" jsonschema:"The query to search memory for."`
}

// LoadMemoryTool returns the native tool for querying long-term memory with JSON Schema compatibility.
func LoadMemoryTool() Tool {
	return loadmemorytool.New()
}

// PreloadMemoryTool returns the native ADK preload_memory tool for automatic turn recall.
func PreloadMemoryTool() Tool {
	return preloadmemorytool.New()
}

type memoryIndexParams struct {
	Text        string `json:"text" jsonschema:"the text to embed and store in long term memory"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// MemoryIndexTool returns the Tool for storing text in vector memory.
func MemoryIndexTool(cwd string, requestApproval func(ctx context.Context, name string, params map[string]any) *ToolResponse) Tool {
	return NewTool(
		"memory_index",
		"Store text in the project's long-term vector memory. Use this to record architectural decisions, solved bugs, learned facts, and important project conventions so they automatically surface in future sessions when relevant. Do NOT use this for code snippets or entire files (those are searched via grep/glob); use it for conceptual knowledge.",
		func(ctx context.Context, p memoryIndexParams) (ToolResponse, error) {
			text := strings.TrimSpace(p.Text)
			if text == "" {
				return NewTextErrorResponse("text cannot be empty"), nil
			}

			if requestApproval != nil {
				if denied := requestApproval(ctx, "memory_index", map[string]any{
					"text":        p.Text,
					"description": p.Description,
				}); denied != nil {
					return *denied, nil
				}
			}

			if err := memory.Index(ctx, cwd, text); err != nil {
				return NewTextErrorResponse("error: " + err.Error()), nil
			}

			return NewTextResponse("successfully indexed memory"), nil
		},
	)
}

// MemoryAwareTool decorates a file tool (read / edit / write) so the tool output carries
// relevant memory recall context for the touched file path.

// WrapFileToolsWithMemory decorates read, edit, and write tools with memory recall.

// Run executes the underlying tool and appends file-specific memory recall when available.
