package memory

import (
	"context"
	"encoding/json"
	"strings"

	"charm.land/fantasy"
)

type memoryIndexParams struct {
	Text        string `json:"text" description:"the text to embed and store in long term memory"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// MemoryIndexTool returns the fantasy AgentTool for storing text in vector memory.
func MemoryIndexTool(cwd string, requestApproval func(ctx context.Context, name string, params map[string]any) *fantasy.ToolResponse) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"memory_index",
		"Store text in the project's long-term vector memory. Use this to record architectural decisions, solved bugs, learned facts, and important project conventions so they automatically surface in future sessions when relevant. Do NOT use this for code snippets or entire files (those are searched via grep/glob); use it for conceptual knowledge.",
		func(ctx context.Context, p memoryIndexParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			text := strings.TrimSpace(p.Text)
			if text == "" {
				return fantasy.NewTextErrorResponse("text cannot be empty"), nil
			}

			if requestApproval != nil {
				if denied := requestApproval(ctx, "memory_index", map[string]any{
					"text":        p.Text,
					"description": p.Description,
				}); denied != nil {
					return *denied, nil
				}
			}

			if err := Index(ctx, cwd, text); err != nil {
				return fantasy.NewTextErrorResponse("error: " + err.Error()), nil
			}

			return fantasy.NewTextResponse("successfully indexed memory"), nil
		},
	)
}

// MemoryAwareTool decorates a file tool (read / edit / write) so the tool output carries
// relevant memory recall context for the touched file path.
type MemoryAwareTool struct {
	fantasy.AgentTool
	Cwd string
}

// WrapFileTools decorates read, edit, and write tools with memory recall.
func WrapFileTools(tools []fantasy.AgentTool, cwd string) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(tools))
	for i, t := range tools {
		switch t.Info().Name {
		case "read", "edit", "write":
			out[i] = &MemoryAwareTool{AgentTool: t, Cwd: cwd}
		default:
			out[i] = t
		}
	}
	return out
}

// Run executes the underlying tool and appends file-specific memory recall when available.
func (m *MemoryAwareTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := m.AgentTool.Run(ctx, call)
	if err != nil || resp.IsError || resp.Type != "text" || !IsOpen() {
		return resp, err
	}
	path := fileToolPath(call.Input)
	if path == "" {
		return resp, err
	}
	recallCtx, cancel := context.WithTimeout(ctx, DefaultHookTimeout)
	defer cancel()
	hits, rerr := Recall(recallCtx, m.Cwd, path, DefaultRecallK)
	if rerr != nil {
		return resp, err
	}
	if block := FormatRecallContext(hits, "Memory for "+path); block != "" {
		resp.Content = resp.Content + "\n\n" + block
	}
	return resp, err
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
