package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/memory"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool/preloadmemorytool"
	"google.golang.org/genai"
)

type loadMemoryParams struct {
	Query string `json:"query" jsonschema:"The query to search memory for."`
}

// LoadMemoryTool returns the native tool for querying long-term memory with JSON Schema compatibility.
// LoadMemoryResult is the load_memory tool's response.
type LoadMemoryResult struct {
	Memories string `json:"memories,omitempty" jsonschema:"matching memories, one per line"`
	Count    int    `json:"count,omitempty"`
}

func LoadMemoryTool() Tool {
	return NewTypedTool(
		"load_memory",
		"Loads the memory for the current user.",
		func(ctx agent.Context, p loadMemoryParams) (LoadMemoryResult, error) {
			query := strings.TrimSpace(p.Query)
			if query == "" {
				return LoadMemoryResult{}, errors.New("query cannot be empty")
			}
			actx := engine.NewStandaloneAgentContext(ctx)
			res, err := actx.SearchMemory(ctx, query)
			if err != nil {
				return LoadMemoryResult{}, errors.New("failed to search memory: " + err.Error())
			}
			if res == nil || len(res.Memories) == 0 {
				return LoadMemoryResult{Count: 0}, nil
			}
			var lines []string
			for _, m := range res.Memories {
				if m.Content != nil {
					for _, part := range m.Content.Parts {
						if part.Text != "" {
							lines = append(lines, part.Text)
						}
					}
				}
			}
			if len(lines) == 0 {
				return LoadMemoryResult{Count: 0}, nil
			}
			return LoadMemoryResult{Memories: strings.Join(lines, "\n"), Count: len(lines)}, nil
		},
	)
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
// MemoryIndexResult is the memory_index tool's response.
type MemoryIndexResult struct {
	Indexed bool `json:"indexed,omitempty"`
}

func MemoryIndexTool(cwd string, approvalDenied func(ctx context.Context, name string, params map[string]any) string) Tool {
	return NewTypedTool(
		"memory_index",
		"Store text in the project's long-term vector memory. Use this to record architectural decisions, solved bugs, learned facts, and important project conventions so they automatically surface in future sessions when relevant. Do NOT use this for code snippets or entire files (those are searched via grep/glob); use it for conceptual knowledge.",
		func(ctx agent.Context, p memoryIndexParams) (MemoryIndexResult, error) {
			text := strings.TrimSpace(p.Text)
			if text == "" {
				return MemoryIndexResult{}, errors.New("text cannot be empty")
			}

			if approvalDenied != nil {
				if denied := approvalDenied(ctx, "memory_index", map[string]any{
					"text":        p.Text,
					"description": p.Description,
				}); denied != "" {
					return MemoryIndexResult{}, errors.New(denied)
				}
			}

			if err := memory.Index(ctx, cwd, text); err != nil {
				return MemoryIndexResult{}, errors.New("error: " + err.Error())
			}

			return MemoryIndexResult{Indexed: true}, nil
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
	if dp, ok := m.Inner.(interface {
		Declaration() *genai.FunctionDeclaration
	}); ok {
		return dp.Declaration()
	}
	return nil
}

func (m *MemoryAwareTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if rp, ok := m.Inner.(interface {
		ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
	}); ok {
		return rp.ProcessRequest(ctx, req)
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

	return engine.AppendToolResultText(resp, block), err
}
