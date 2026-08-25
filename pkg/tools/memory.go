package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/memory"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type loadMemoryParams struct {
	Query       string `json:"query,omitempty" jsonschema:"text to search long-term memory for; omit when fetching one concept by id"`
	ID          int64  `json:"id,omitempty" jsonschema:"a concept id (the #number in a memory list) to fetch in full"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// LoadMemoryResult is the load_memory tool's response.
type LoadMemoryResult struct {
	Memories string `json:"memories,omitempty" jsonschema:"matching concepts, #id [kind · topic] title with bodies"`
	Count    int    `json:"count,omitempty"`
	Topic    string `json:"topic,omitempty" jsonschema:"the topic the matches share"`
}

// LoadMemoryTool returns the on-demand recall tool: a query searches the
// project and global scopes, an id fetches one concept's full body.
func LoadMemoryTool(cwd string) Tool {
	return NewTypedTool(
		"load_memory",
		"Search long-term memory by text, or fetch one concept's full body by id. Results are #id [kind · topic] title lines; ids can be passed to memory_reinforce, memory_demote, or memory_forget.",
		func(ctx agent.Context, p loadMemoryParams) (LoadMemoryResult, error) {
			svc := memory.Default()
			if !svc.IsOpen() {
				return LoadMemoryResult{}, errors.New("memory service closed")
			}
			if p.ID > 0 {
				c, err := svc.Get(ctx, p.ID)
				if err != nil {
					return LoadMemoryResult{}, err
				}
				return LoadMemoryResult{Memories: memory.FormatConcepts([]memory.Concept{c}, "Memory", "", 1), Count: 1, Topic: c.Topic}, nil
			}
			query := strings.TrimSpace(p.Query)
			if query == "" {
				return LoadMemoryResult{}, errors.New("query or id is required")
			}
			res, err := svc.Recall(ctx, memory.RecallQuery{Cwd: cwd, Query: query, K: 10})
			if err != nil {
				return LoadMemoryResult{}, errors.New("failed to search memory: " + err.Error())
			}
			if len(res.Concepts) == 0 {
				return LoadMemoryResult{Count: 0, Topic: res.Topic}, nil
			}
			return LoadMemoryResult{
				Memories: memory.FormatConcepts(res.Concepts, "Memory", "", 5),
				Count:    len(res.Concepts),
				Topic:    res.Topic,
			}, nil
		},
	)
}

// MemoryRecallHook is the per-request recall: it embeds the turn's user
// text, recalls concepts in the project and global scopes, and appends
// the block to the turn's user message. The block is computed once per
// invocation so every request of the turn carries the same text.
type MemoryRecallHook struct {
	cwd     string
	topic   func() string
	onTopic func(string)

	mu         sync.Mutex
	exclude    map[int64]bool
	excludeSet bool
	invocation string
	block      string
}

// PreloadMemoryTool builds the recall hook. topic supplies the tab's
// current topic (nil means none); onTopic receives the topic each turn's
// hits imply (nil to ignore).
func PreloadMemoryTool(cwd string, topic func() string, onTopic func(string)) *MemoryRecallHook {
	return &MemoryRecallHook{cwd: cwd, topic: topic, onTopic: onTopic}
}

func (h *MemoryRecallHook) Name() string { return "preload_memory" }
func (h *MemoryRecallHook) Description() string {
	return "Recalls relevant long-term memory for each turn."
}
func (h *MemoryRecallHook) IsLongRunning() bool { return false }

// ProcessRequest implements ADK's request processor: it runs before every
// model call and never becomes a function declaration.
func (h *MemoryRecallHook) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if !memory.IsOpen() || req == nil {
		return nil
	}
	text := userText(ctx.UserContent())
	if text == "" {
		return nil
	}
	block := h.blockFor(ctx, text)
	if block == "" {
		return nil
	}
	target := lastUserTextContent(req.Contents)
	if target < 0 {
		return nil
	}
	copied := *req.Contents[target]
	copied.Parts = append(append([]*genai.Part(nil), req.Contents[target].Parts...), genai.NewPartFromText("<memory>\n"+block+"\n</memory>"))
	req.Contents[target] = &copied
	return nil
}

func (h *MemoryRecallHook) blockFor(ctx agent.Context, text string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	inv := ctx.InvocationID()
	if inv != "" && inv == h.invocation {
		return h.block
	}
	if !h.excludeSet {
		h.exclude = memory.TopIDs(ctx, h.cwd)
		h.excludeSet = true
	}
	current := ""
	if h.topic != nil {
		current = h.topic()
	}
	block, topic := memory.RecallBlock(ctx, h.cwd, text, current, h.exclude)
	h.invocation = inv
	h.block = block
	if topic != "" && h.onTopic != nil {
		h.onTopic(topic)
	}
	return block
}

// Block returns the block computed for the current invocation.
func (h *MemoryRecallHook) Block() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.block
}

func userText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var parts []string
	for _, p := range c.Parts {
		if p != nil && p.Text != "" && !p.Thought {
			parts = append(parts, p.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// lastUserTextContent finds the turn's user message: the last user-role
// content that carries text and no function responses.
func lastUserTextContent(contents []*genai.Content) int {
	for i := len(contents) - 1; i >= 0; i-- {
		c := contents[i]
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		hasText, hasResponse := false, false
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.FunctionResponse != nil {
				hasResponse = true
			}
			if p.Text != "" {
				hasText = true
			}
		}
		if hasText && !hasResponse {
			return i
		}
	}
	return -1
}

type memoryIndexParams struct {
	Kind        string `json:"kind" jsonschema:"user | feedback | project | reference"`
	Scope       string `json:"scope,omitempty" jsonschema:"project (default) or global for facts about the user that hold everywhere"`
	Topic       string `json:"topic,omitempty" jsonschema:"one or two words grouping this concept; reuse an existing topic when one fits"`
	Title       string `json:"title" jsonschema:"one line, the fact itself"`
	Body        string `json:"body,omitempty" jsonschema:"self-contained detail, including the why"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// MemoryIndexResult is the memory_index tool's response.
type MemoryIndexResult struct {
	Indexed bool  `json:"indexed,omitempty"`
	ID      int64 `json:"id,omitempty"`
}

// MemoryIndexTool returns the tool for storing a concept explicitly.
func MemoryIndexTool(cwd string, approvalDenied func(ctx context.Context, name string, params map[string]any) string) Tool {
	return NewTypedTool(
		"memory_index",
		"Store a concept in long-term memory: the user's role or preferences (user), how they want work done and why (feedback), project goals and constraints not derivable from the code (project), or where things live externally (reference). Concepts surface automatically in future sessions when relevant. Do NOT store code, task state, or anything the repository already records.",
		func(ctx agent.Context, p memoryIndexParams) (MemoryIndexResult, error) {
			title := strings.TrimSpace(p.Title)
			body := strings.TrimSpace(p.Body)
			if title == "" && body == "" {
				return MemoryIndexResult{}, errors.New("title cannot be empty")
			}
			kind := strings.ToLower(strings.TrimSpace(p.Kind))
			if !memory.ValidKind(kind) {
				return MemoryIndexResult{}, fmt.Errorf("kind must be one of %s", strings.Join(memory.Kinds, ", "))
			}
			if approvalDenied != nil {
				if denied := approvalDenied(ctx, "memory_index", map[string]any{
					"kind":        kind,
					"scope":       p.Scope,
					"topic":       p.Topic,
					"title":       title,
					"body":        body,
					"description": p.Description,
				}); denied != "" {
					return MemoryIndexResult{}, errors.New(denied)
				}
			}
			scope := memory.ScopeFor(cwd)
			if strings.EqualFold(strings.TrimSpace(p.Scope), memory.ScopeGlobal) || scope == "" {
				scope = memory.ScopeGlobal
			}
			svc := memory.Default()
			if !svc.IsOpen() {
				return MemoryIndexResult{}, errors.New("memory service closed")
			}
			id, err := svc.Upsert(ctx, memory.Concept{Scope: scope, Kind: kind, Topic: p.Topic, Title: title, Body: body})
			if err != nil {
				return MemoryIndexResult{}, errors.New("error: " + err.Error())
			}
			return MemoryIndexResult{Indexed: true, ID: id}, nil
		},
	)
}

type memoryIDParams struct {
	ID          int64  `json:"id" jsonschema:"the concept id (#number) from a memory list"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// MemoryAdjustResult is the memory_reinforce / memory_demote response.
type MemoryAdjustResult struct {
	Applied bool    `json:"applied"`
	Weight  float64 `json:"weight,omitempty"`
	Note    string  `json:"note,omitempty"`
}

// MemoryReinforceTool bumps a concept the model found useful.
func MemoryReinforceTool() Tool {
	return NewTypedTool(
		"memory_reinforce",
		"Mark a recalled concept as genuinely useful for this turn, raising its weight so it surfaces more readily. Rate-limited per concept.",
		func(ctx agent.Context, p memoryIDParams) (MemoryAdjustResult, error) {
			return adjustMemory(ctx, p.ID, true)
		},
	)
}

// MemoryDemoteTool lowers a concept that misled the model.
func MemoryDemoteTool() Tool {
	return NewTypedTool(
		"memory_demote",
		"Mark a recalled concept as misleading or outdated, lowering its weight without deleting it. Rate-limited per concept.",
		func(ctx agent.Context, p memoryIDParams) (MemoryAdjustResult, error) {
			return adjustMemory(ctx, p.ID, false)
		},
	)
}

func adjustMemory(ctx agent.Context, id int64, up bool) (MemoryAdjustResult, error) {
	if id <= 0 {
		return MemoryAdjustResult{}, errors.New("id is required")
	}
	svc := memory.Default()
	if !svc.IsOpen() {
		return MemoryAdjustResult{}, errors.New("memory service closed")
	}
	var applied bool
	var err error
	if up {
		applied, err = svc.Reinforce(ctx, id)
	} else {
		applied, err = svc.Demote(ctx, id)
	}
	if err != nil {
		return MemoryAdjustResult{}, err
	}
	c, err := svc.Get(ctx, id)
	if err != nil {
		return MemoryAdjustResult{}, err
	}
	res := MemoryAdjustResult{Applied: applied, Weight: c.Weight}
	if !applied {
		res.Note = fmt.Sprintf("concept #%d was adjusted moments ago; no change", id)
	}
	return res, nil
}

// MemoryForgetResult is the memory_forget response.
type MemoryForgetResult struct {
	Forgotten bool `json:"forgotten,omitempty"`
}

// MemoryForgetTool hard-deletes a concept; approval-gated.
func MemoryForgetTool(approvalDenied func(ctx context.Context, name string, params map[string]any) string) Tool {
	return NewTypedTool(
		"memory_forget",
		"Delete a concept from long-term memory outright. Only for corrected misinformation, secrets stored by mistake, or an explicit user request; prefer memory_demote otherwise.",
		func(ctx agent.Context, p memoryIDParams) (MemoryForgetResult, error) {
			if p.ID <= 0 {
				return MemoryForgetResult{}, errors.New("id is required")
			}
			if approvalDenied != nil {
				if denied := approvalDenied(ctx, "memory_forget", map[string]any{
					"id":          p.ID,
					"description": p.Description,
				}); denied != "" {
					return MemoryForgetResult{}, errors.New(denied)
				}
			}
			svc := memory.Default()
			if !svc.IsOpen() {
				return MemoryForgetResult{}, errors.New("memory service closed")
			}
			if err := svc.Forget(ctx, p.ID); err != nil {
				return MemoryForgetResult{}, err
			}
			return MemoryForgetResult{Forgotten: true}, nil
		},
	)
}

// MemoryTools is the registry set: explicit store, reinforce, demote,
// forget.
func MemoryTools(cwd string, approvalDenied func(ctx context.Context, name string, params map[string]any) string) []Tool {
	return []Tool{
		MemoryIndexTool(cwd, approvalDenied),
		MemoryReinforceTool(),
		MemoryDemoteTool(),
		MemoryForgetTool(approvalDenied),
	}
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
	block := memory.FileBlock(ctx, m.Cwd, path)
	if block == "" {
		return resp, err
	}
	return engine.AppendToolResultText(resp, block), err
}
