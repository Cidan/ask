package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Cidan/ask/pkg/memory"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/preloadmemorytool"
	"google.golang.org/genai"
)

// LoadMemoryTool returns the native ADK load_memory tool for querying long-term memory.
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
type MemoryAwareTool struct {
	Inner Tool
	Cwd   string
}

func (m *MemoryAwareTool) Name() string        { return m.Inner.Name() }
func (m *MemoryAwareTool) Description() string { return m.Inner.Description() }
func (m *MemoryAwareTool) IsLongRunning() bool { return m.Inner.IsLongRunning() }
func (m *MemoryAwareTool) Info() ToolInfo      { return ExtractToolInfo(m.Inner) }
func (m *MemoryAwareTool) Declaration() *genai.FunctionDeclaration {
	if dp, ok := m.Inner.(interface{ Declaration() *genai.FunctionDeclaration }); ok {
		return dp.Declaration()
	}
	return nil
}

// WrapFileToolsWithMemory decorates read, edit, and write tools with memory recall.
func WrapFileToolsWithMemory(tools []Tool, cwd string) []Tool {
	out := make([]Tool, len(tools))
	for i, t := range tools {
		switch t.Name() {
		case "read", "edit", "write":
			out[i] = &MemoryAwareTool{Inner: t, Cwd: cwd}
		default:
			out[i] = t
		}
	}
	return out
}

// Run executes the underlying tool and appends file-specific memory recall when available.
func (m *MemoryAwareTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	resp, err := RunADKTool(ctx, m.Inner, args)
	if err != nil || !memory.IsOpen() {
		return resp, err
	}
	if isErr, _ := resp["is_error"].(bool); isErr {
		return resp, err
	}

	argsMap, _ := args.(map[string]any)
	if argsMap == nil {
		if raw, err := json.Marshal(args); err == nil {
			_ = json.Unmarshal(raw, &argsMap)
		}
	}

	path, _ := argsMap["file_path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return resp, err
	}
	recallCtx, cancel := context.WithTimeout(ctx, memory.DefaultHookTimeout)
	defer cancel()
	hits, rerr := memory.Recall(recallCtx, m.Cwd, path, memory.DefaultRecallK)
	if rerr != nil {
		return resp, err
	}
	block := memory.FormatRecallContext(hits, "Memory for "+path)
	if block == "" {
		return resp, err
	}

	if resp == nil {
		resp = make(map[string]any)
	}
	if s, ok := resp["result"].(string); ok && s != "" {
		resp["result"] = s + "\n\n" + block
	} else {
		resp["result"] = block
	}
	return resp, err
}
