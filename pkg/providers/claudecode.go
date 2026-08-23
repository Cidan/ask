package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// ClaudeCode is the provider that forks the `claude` CLI in headless
// stream-json mode and drives it with ask's own tools, prompt, and modals.
// Claude Code never runs a tool itself: `--tools ""` strips every built-in and
// ask registers as an in-process (sdk-type) MCP server over the child's
// stdio, so the ADK loop executes every tool exactly as it does for Vertex or
// OpenRouter. Auth is whatever the `claude` binary is logged in as (Claude
// subscription or ANTHROPIC_API_KEY in the environment); ask never stores a
// credential.
type ClaudeCode struct{}

const (
	ClaudeCodeProviderID   = "claude-code"
	ClaudeCodeDefaultModel = "default"

	ClaudeCodeFieldBinary = "binary"
	ClaudeCodeEnvBinary   = "ASK_CLAUDE_BIN"
	ClaudeCodeDefaultBin  = "claude"

	ClaudeCodeContextWindow  = 200_000
	ClaudeCodeMaxOutputToken = 64_000
)

// ClaudeCodeEffortOptions are the CLI's effort levels; ask's global picker
// offers the first three.
var ClaudeCodeEffortOptions = []string{"low", "medium", "high", "xhigh", "max"}

var claudeCodeSettings = []SettingField{
	{
		Key:     ClaudeCodeFieldBinary,
		Title:   "Binary",
		Hint:    "path to the claude CLI (default: claude on PATH); enter to save",
		EnvKey:  ClaudeCodeEnvBinary,
		Default: ClaudeCodeDefaultBin,
	},
}

func (ClaudeCode) ID() string              { return ClaudeCodeProviderID }
func (ClaudeCode) DisplayName() string     { return "Claude Code" }
func (ClaudeCode) DefaultModel() string    { return ClaudeCodeDefaultModel }
func (ClaudeCode) ModelOptions() []string  { return ClaudeCodeModelOptions }
func (ClaudeCode) EffortOptions() []string { return ClaudeCodeEffortOptions }

func (ClaudeCode) Settings() []SettingField {
	return append([]SettingField(nil), claudeCodeSettings...)
}

// Configured reports whether the claude binary resolves. Auth lives in the
// binary, so there is no credential to check.
func (ClaudeCode) Configured(pc config.ProviderConfig) bool {
	_, err := exec.LookPath(ClaudeCodeResolveBinary(pc))
	return err == nil
}

// BuildModel returns the lock-step adapter. It does not spawn the child — the
// process starts on the first GenerateContent — but it fails fast if the
// binary cannot be found, so a misconfigured session errors at session start.
func (ClaudeCode) BuildModel(ctx context.Context, pc config.ProviderConfig, modelID string) (model.LLM, error) {
	bin := ClaudeCodeResolveBinary(pc)
	if _, err := exec.LookPath(bin); err != nil {
		return nil, errors.New("claude-code: the claude CLI was not found — install it, set it in /config → Claude Code, or export " + ClaudeCodeEnvBinary)
	}
	cwd := ""
	if wd := ctxCwd(ctx); wd != "" {
		cwd = wd
	}
	return newClaudeCodeModel(bin, CanonicalClaudeCodeModelID(modelID, ClaudeCodeDefaultModel), cwd), nil
}

func (ClaudeCode) CanonicalModelID(modelID, fallback string) string {
	return CanonicalClaudeCodeModelID(modelID, fallback)
}

// CallOptions rides ask's effort onto a ThinkingConfig; the adapter reads the
// resulting ThinkingLevel back into a --effort flag when it spawns the child.
func (ClaudeCode) CallOptions(modelID, effort string) (*genai.GenerateContentConfig, *float64) {
	cfg := &genai.GenerateContentConfig{}
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" || e == "off" {
		return cfg, nil
	}
	cfg.ThinkingConfig = &genai.ThinkingConfig{
		IncludeThoughts: true,
		ThinkingLevel:   genai.ThinkingLevel(strings.ToUpper(e)),
	}
	return cfg, nil
}

func (ClaudeCode) SupportsImages(modelID string) bool {
	return CatalogSupportsImages(ClaudeCodeProviderID, modelID, true)
}

func (ClaudeCode) ContextWindow(modelID string) int64 {
	return CatalogContextWindow(ClaudeCodeProviderID, modelID, ClaudeCodeContextWindow)
}

func (ClaudeCode) MaxOutputTokens(modelID string) int64 {
	return CatalogDefaultMaxTokens(ClaudeCodeProviderID, modelID, ClaudeCodeMaxOutputToken)
}

// ListModels returns the account's live model list by spawning a short-lived
// child, reading the initialize control response's `models` array, and killing
// it — the same handshake a session does, without a turn. Falls back to the
// static catalog if the probe fails so the picker is never empty.
func (ClaudeCode) ListModels(ctx context.Context, pc config.ProviderConfig) ([]string, error) {
	ids, err := probeClaudeCodeModels(ctx, ClaudeCodeResolveBinary(pc))
	if err != nil || len(ids) == 0 {
		return ClaudeCodeModelOptions, err
	}
	return ids, nil
}

// ccListTimeout bounds the model-listing probe: it is only a handshake.
const ccListTimeout = 15 * time.Second

// probeClaudeCodeModels forks `claude`, sends initialize, and returns the
// `models` array's values. It also caches each model's live metadata
// (displayName, description, effort levels) for ModelMetaFor. The child never
// runs a turn; it is killed as soon as the response lands (or the timeout).
func probeClaudeCodeModels(ctx context.Context, binary string) ([]string, error) {
	pctx, cancel := context.WithTimeout(ctx, ccListTimeout)
	defer cancel()

	conn, err := ccDial(pctx, ccDialArgs{Binary: binary, Argv: ccProbeArgv(), Env: currentEnvMinusClaude()})
	if err != nil {
		return nil, err
	}
	defer conn.close()

	if err := conn.send(ccControlEnvelope{
		Type: "control_request", RequestID: "init",
		Request: map[string]any{"subtype": "initialize", "hooks": nil},
	}); err != nil {
		return nil, err
	}

	for {
		select {
		case <-pctx.Done():
			return nil, pctx.Err()
		case fr, ok := <-conn.frames():
			if !ok {
				return nil, errChildExited
			}
			if fr.Type != "control_response" {
				continue
			}
			var wrap ccInitResponseWrap
			if json.Unmarshal(fr.Response, &wrap) != nil || wrap.RequestID != "init" {
				continue
			}
			ids := make([]string, 0, len(wrap.Response.Models))
			metas := make([]ccModelMeta, 0, len(wrap.Response.Models))
			for _, m := range wrap.Response.Models {
				if m.Value == "" {
					continue
				}
				ids = append(ids, m.Value)
				metas = append(metas, m.toMeta())
			}
			cacheClaudeCodeMeta(metas)
			return ids, nil
		}
	}
}

// ccProbeArgv is the minimal argv for the listing probe: enough to complete the
// initialize handshake, with no tools and no MCP server.
func ccProbeArgv() []string {
	return []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--tools", "",
		"--setting-sources", "",
	}
}

func (m ccInitModelInfo) toMeta() ccModelMeta {
	return ccModelMeta{
		ID:              m.Value,
		Name:            m.DisplayName,
		Description:     m.Description,
		ResolvedModel:   m.ResolvedModel,
		Reasoning:       m.SupportsEffort || len(m.SupportedEffortLevels) > 0,
		ReasoningLevels: m.SupportedEffortLevels,
	}
}

// ccModelMeta is the slice of the initialize response's model info ask keeps.
type ccModelMeta struct {
	ID              string
	Name            string
	Description     string
	ResolvedModel   string
	Reasoning       bool
	ReasoningLevels []string
}

func (m ccModelMeta) modelMeta() ModelMeta {
	return ModelMeta{
		ID:              m.ID,
		Name:            m.Name,
		Description:     m.Description,
		Reasoning:       m.Reasoning,
		ReasoningLevels: m.ReasoningLevels,
	}
}

// claudeCodeMeta is the in-memory cache of the live model listing, keyed by the
// `value` a session passes as --model. In-memory only, like OpenRouter's — the
// picker must never block on a spawn.
var claudeCodeMeta = struct {
	mu   sync.RWMutex
	byID map[string]ccModelMeta
}{byID: map[string]ccModelMeta{}}

func cacheClaudeCodeMeta(metas []ccModelMeta) {
	claudeCodeMeta.mu.Lock()
	defer claudeCodeMeta.mu.Unlock()
	for _, m := range metas {
		claudeCodeMeta.byID[m.ID] = m
	}
}

// cachedClaudeCodeMeta looks up a model's live metadata; ok=false until a
// listing has landed.
func cachedClaudeCodeMeta(modelID string) (ccModelMeta, bool) {
	claudeCodeMeta.mu.RLock()
	defer claudeCodeMeta.mu.RUnlock()
	m, ok := claudeCodeMeta.byID[modelID]
	return m, ok
}

var (
	_ Provider    = ClaudeCode{}
	_ ModelLister = ClaudeCode{}
)

// ClaudeCodeResolveBinary: config value wins, then ASK_CLAUDE_BIN, then the
// default "claude".
func ClaudeCodeResolveBinary(pc config.ProviderConfig) string {
	return SettingValue(pc, claudeCodeSettings[0])
}

// CanonicalClaudeCodeModelID passes through a known alias or full model name;
// only an empty id falls back. The CLI accepts aliases ("opus") and full names
// ("claude-opus-5") alike, so ask does not second-guess an unrecognized id.
func CanonicalClaudeCodeModelID(modelID string, fallback ...string) string {
	fb := ClaudeCodeDefaultModel
	if len(fallback) > 0 && fallback[0] != "" {
		fb = fallback[0]
	}
	norm := strings.TrimSpace(modelID)
	if norm == "" {
		return fb
	}
	return norm
}

// ---- argv and request helpers (used by the adapter) ----

// ccArgv builds the flags for one child. Everything that lets Claude's own
// context, tools, and settings leak in is switched off; ask supplies the
// system prompt, the tools (as an sdk MCP server), and the permission decision.
func ccArgv(modelID, effort, systemPrompt string) []string {
	mcp, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{"ask": map[string]any{"type": "sdk", "name": "ask"}},
	})
	argv := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--tools", "",
		"--mcp-config", string(mcp),
		"--strict-mcp-config",
		"--allowedTools", "mcp__ask",
		"--setting-sources", "",
		"--settings", `{"autoMemoryEnabled":false}`,
		"--no-session-persistence",
		"--system-prompt", systemPrompt,
	}
	if m := strings.TrimSpace(modelID); m != "" && m != ClaudeCodeDefaultModel {
		argv = append(argv, "--model", m)
	}
	if e := ccEffortFlag(effort); e != "" {
		argv = append(argv, "--effort", e)
	}
	return argv
}

// ccEffortFlag maps a genai ThinkingLevel (set by CallOptions) back to a CLI
// --effort value.
func ccEffortFlag(effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	switch e {
	case "low", "medium", "high", "xhigh", "max":
		return e
	default:
		return ""
	}
}

func reqTools(req *model.LLMRequest) []*genai.Tool {
	if req == nil || req.Config == nil {
		return nil
	}
	return req.Config.Tools
}

func reqSystemPrompt(req *model.LLMRequest) string {
	if req == nil || req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func reqEffort(req *model.LLMRequest) string {
	if req == nil || req.Config == nil || req.Config.ThinkingConfig == nil {
		return ""
	}
	return string(req.Config.ThinkingConfig.ThinkingLevel)
}

// ---- content helpers ----

func isModelContent(c *genai.Content) bool {
	return c != nil && (c.Role == "model" || c.Role == "assistant")
}

func hasFunctionResponses(c *genai.Content) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Parts {
		if p != nil && p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// ccHistoryPreamble flattens prior conversation into one readable context
// message for a fresh child (cross-provider resume / materialize; the child
// has no native transcript because ask runs with --no-session-persistence).
func ccHistoryPreamble(contents []*genai.Content) string {
	var b strings.Builder
	b.WriteString("Conversation so far (for context; do not re-answer, continue from the newest user message):\n\n")
	for _, c := range contents {
		if c == nil {
			continue
		}
		role := "User"
		if isModelContent(c) {
			role = "Assistant"
		}
		var line strings.Builder
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			switch {
			case p.Text != "":
				line.WriteString(p.Text)
			case p.FunctionCall != nil:
				line.WriteString("[called " + p.FunctionCall.Name + "]")
			case p.FunctionResponse != nil:
				if t, _ := ccToolResultText(p.FunctionResponse.Response); t != "" {
					line.WriteString("[" + p.FunctionResponse.Name + " result]")
				}
			}
		}
		if s := strings.TrimSpace(line.String()); s != "" {
			b.WriteString(role + ": " + s + "\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// ccDelta extracts a text or thinking delta from a stream_event frame's event.
func ccDelta(raw json.RawMessage) (text, kind string) {
	if len(raw) == 0 {
		return "", ""
	}
	var ev ccStreamEvent
	if json.Unmarshal(raw, &ev) != nil || ev.Type != "content_block_delta" {
		return "", ""
	}
	if ev.Delta.Text != "" {
		return ev.Delta.Text, "text"
	}
	if ev.Delta.Thinking != "" {
		return ev.Delta.Thinking, "thinking"
	}
	return "", ""
}

// ccAssistantContent extracts the completed text, thinking, and usage from an
// assistant frame's message. The CLI emits one assistant frame per completed
// content block, so this is the authoritative (non-partial) text for the turn.
func ccAssistantContent(raw json.RawMessage) (text, thought string, usage *ccUsage) {
	if len(raw) == 0 {
		return "", "", nil
	}
	var msg ccAssistantMessage
	if json.Unmarshal(raw, &msg) != nil {
		return "", "", nil
	}
	var tb, thb strings.Builder
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			tb.WriteString(b.Text)
		case "thinking":
			thb.WriteString(b.Thinking)
		}
	}
	return tb.String(), thb.String(), msg.Usage
}

// ccToolResultText mirrors engine.ToolResultText for the small set of result
// shapes ask's tools produce. It lives here, not via an import, because
// pkg/providers must not depend on pkg/engine. Keep the field list in sync
// with engine.toolResultTextFields.
func ccToolResultText(resp map[string]any) (string, bool) {
	if len(resp) == 0 {
		return "", false
	}
	errText, _ := resp["error"].(string)
	isErr := errText != ""
	if legacy, ok := resp["is_error"].(bool); ok && legacy {
		isErr = true
	}
	fields := []string{
		"content", "listing", "output", "body", "results", "memories",
		"answers", "report", "outcome", "note", "notice", "summary",
	}
	for _, f := range fields {
		if s, ok := resp[f].(string); ok && s != "" {
			if errText != "" {
				return errText + "\n" + s, true
			}
			return s, isErr
		}
	}
	if errText != "" {
		return errText, true
	}
	if raw, err := json.Marshal(resp); err == nil {
		return string(raw), isErr
	}
	return "", isErr
}

// ctxCwd extracts a working directory from an ADK invocation context when one
// is available. The context passed to GenerateContent implements this in the
// engine; a plain context returns "".
func ctxCwd(ctx context.Context) string {
	type cwder interface{ Cwd() string }
	if c, ok := ctx.(cwder); ok {
		return c.Cwd()
	}
	return ""
}
