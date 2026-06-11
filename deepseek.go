package main

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
)

// deepseekProvider is the first in-process API provider: no CLI
// subprocess, the agent loop runs inside ask (agent_run.go) against
// DeepSeek's OpenAI-compatible chat-completions API via fantasy.
type deepseekProvider struct{}

func init() { registerProvider(deepseekProvider{}) }

const (
	deepseekProviderID    = "deepseek"
	deepseekDefaultModel  = "deepseek-v4-pro"
	deepseekContextWindow = 1_000_000
)

// deepseekModelOptions are the API model ids as of the V4 line. The
// deprecated deepseek-chat/deepseek-reasoner aliases (retired
// 2026-07-24) are deliberately absent; AllowCustom covers stragglers.
var deepseekModelOptions = []string{"deepseek-v4-pro", "deepseek-v4-flash"}

// deepseekEffortOptions map onto the API's thinking controls: "off"
// disables thinking entirely; "high"/"max" select reasoning_effort
// (xhigh is DeepSeek's wire name for max).
var deepseekEffortOptions = []string{"off", "high", "max"}

func (deepseekProvider) ID() string          { return deepseekProviderID }
func (deepseekProvider) DisplayName() string { return "DeepSeek" }

func (deepseekProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Resume:       true,
		ModelPicker:  true,
		EffortPicker: true,
		// The question modal and tool approvals are wired natively
		// in-process (agent_tools_ask.go); no MCP redirect hooks needed.
		AskUserQuestionMCP:  false,
		PermissionPromptMCP: false,
	}
}

func (deepseekProvider) ModelPicker() ProviderPicker {
	return ProviderPicker{
		Prompt:      "Select DeepSeek model",
		Options:     deepseekModelOptions,
		AllowCustom: true,
	}
}

func (deepseekProvider) EffortOptions() []string { return deepseekEffortOptions }

func (deepseekProvider) BaseSlashCommands() []slashCmd {
	return []slashCmd{
		{"/resume", "resume a previous DeepSeek session"},
		{"/new", "start a new DeepSeek session"},
		{"/clear", "start a new DeepSeek session"},
		{"/model", "select the DeepSeek model"},
		{"/effort", "select the DeepSeek reasoning effort"},
	}
}

func (deepseekProvider) ProbeInit(_ ProviderSessionArgs) tea.Cmd { return nil }

// PreMintSessionID: session ids are ours (the store keys on them), so
// minting up front gives the same first-turn-cancel safety claude's
// --session-id path has.
func (deepseekProvider) PreMintSessionID(_ ProviderSessionArgs) string { return newUUIDv4() }

func (deepseekProvider) NativeSessionID(_ *providerProc) string { return "" }

func deepseekStore() *agentSessionStore {
	return &agentSessionStore{provider: deepseekProviderID}
}

// deepseekLanguageModel builds the fantasy LanguageModel for one
// session. Swappable in tests so StartSession can run against a fake
// model with zero network.
var deepseekLanguageModel = func(cfg deepseekConfig, modelID string) (fantasy.LanguageModel, error) {
	key := resolveDeepSeekAPIKey(cfg)
	if key == "" {
		return nil, errors.New("no API key configured — set one in /config → DeepSeek..., or export " + deepseekEnvAPIKey)
	}
	provider, err := openaicompat.New(
		openaicompat.WithName(deepseekProviderID),
		openaicompat.WithBaseURL(resolveDeepSeekBaseURL(cfg)),
		openaicompat.WithAPIKey(key),
	)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// deepseekProviderOptions translates ask's effort picker onto the wire
// controls, returning the per-call provider options and the sampling
// temperature (DeepSeek recommends 0.0 for coding, but thinking mode
// does not accept sampling params at all, so it only applies to
// thinking=off).
func deepseekProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	opts := &openaicompat.ProviderOptions{}
	var temperature *float64
	switch effort {
	case "off":
		opts.ExtraBody = map[string]any{"thinking": map[string]any{"type": "disabled"}}
		t := 0.0
		temperature = &t
	case "max":
		e := openai.ReasoningEffortXHigh
		opts.ReasoningEffort = &e
	default: // "high" and unset both ride the default thinking mode
		e := openai.ReasoningEffortHigh
		opts.ReasoningEffort = &e
	}
	return fantasy.ProviderOptions{deepseekProviderID: opts}, temperature
}

func (deepseekProvider) StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
	cfg, _ := loadConfig()
	modelID := args.Model
	if modelID == "" {
		modelID = deepseekDefaultModel
	}
	lm, err := deepseekLanguageModel(cfg.DeepSeek, modelID)
	if err != nil {
		return nil, nil, fmt.Errorf("deepseek: %w", err)
	}

	store := deepseekStore()
	providerOpts, temperature := deepseekProviderOptions(args.Effort)
	session := &agentSession{
		args:          args,
		model:         lm,
		system:        buildAgentSystemPrompt(args),
		providerOpts:  providerOpts,
		temperature:   temperature,
		contextWindow: deepseekContextWindow,
		modelID:       modelID,
		ch:            make(chan tea.Msg, 32),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		store:         store,
	}

	switch {
	case args.SessionID != "":
		file, err := store.load(args.SessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("deepseek: resume %s: %w", short(args.SessionID), err)
		}
		session.sessionID = args.SessionID
		session.messages = repairDanglingToolCalls(file.Messages)
	case args.NewSessionID != "":
		session.sessionID = args.NewSessionID
	default:
		session.sessionID = newUUIDv4()
	}

	session.env = newAgentToolEnv(args.Cwd, args.TabID, args.SkipAllPermissions, session.emit)
	session.tools = deepseekSessionTools(session)

	proc := &providerProc{
		stdin:   agentStdin{s: session},
		stderr:  &stderrBuf{},
		payload: session,
	}
	session.proc = proc
	go session.run()
	return proc, session.ch, nil
}

// deepseekSessionTools assembles the full tool set for a session:
// the coding core, the ask-native bridge pair, and any reachable MCP
// servers (ask's loopback bridge for linear/workflow tools, plus the
// project GitHub MCP). MCP failures are logged and skipped — a dead
// remote must not block a coding session.
func deepseekSessionTools(s *agentSession) []fantasy.AgentTool {
	env := s.env
	tools := []fantasy.AgentTool{
		agentReadTool(env),
		agentWriteTool(env),
		agentEditTool(env),
		agentGlobTool(env),
		agentGrepTool(env),
		agentLsTool(env),
		agentBashTool(env),
		agentJobOutputTool(env),
		agentJobKillTool(env),
		agentFetchTool(env),
		agentTodosTool(env),
		agentTaskTool(env, func() fantasy.LanguageModel { return s.model }),
		agentAskUserQuestionTool(env),
		agentEndTurnTool(env),
	}

	var servers []agentMCPServer
	if s.args.MCPPort > 0 {
		servers = append(servers, agentMCPServer{
			name: "ask",
			url:  fmt.Sprintf("http://127.0.0.1:%d/", s.args.MCPPort),
			skip: map[string]bool{
				"ask_user_question": true, // native in-process twin
				"end_turn":          true, // native in-process twin
				"approval_prompt":   true, // claude-internal callback
			},
		})
	}
	if s.args.ProjectMCP != nil {
		servers = append(servers, agentMCPServer{
			name:    s.args.ProjectMCP.Name,
			url:     s.args.ProjectMCP.URL,
			headers: s.args.ProjectMCP.Headers,
		})
	}
	for _, srv := range servers {
		mcpTools, closer, err := connectAgentMCP(context.Background(), srv)
		if err != nil {
			debugLog("deepseek: mcp %s skipped: %v", srv.name, err)
			continue
		}
		tools = append(tools, mcpTools...)
		s.mcpClosers = append(s.mcpClosers, closer)
	}
	return tools
}

// Send queues a user turn. Image attachments are rejected outright:
// the V4 models do not accept image input, and silently dropping a
// paste would be worse than saying so.
func (deepseekProvider) Send(p *providerProc, text string, attachments []pendingAttachment) error {
	session, ok := p.payload.(*agentSession)
	if !ok {
		return errors.New("deepseek: proc payload is not an agent session")
	}
	if len(attachments) > 0 {
		return errors.New("DeepSeek models do not support image attachments — remove the image and resend")
	}
	return session.queueTurn(text)
}

// Interrupt cancels the in-flight turn cooperatively; the session
// stays alive and emits its own turn completion, so handled=true keeps
// killProc out of the picture (same contract as codex).
func (deepseekProvider) Interrupt(p *providerProc) (bool, error) {
	session, ok := p.payload.(*agentSession)
	if !ok {
		return false, nil
	}
	return session.interruptTurn(), nil
}

func (deepseekProvider) ListSessions(cwd string) ([]sessionEntry, error) {
	return deepseekStore().list(cwd)
}

func (deepseekProvider) LoadHistory(sessionID string, opts HistoryOpts) ([]historyEntry, error) {
	return deepseekStore().loadHistory(sessionID, opts)
}

func (deepseekProvider) LoadSettings() ProviderSettings {
	cfg, _ := loadConfig()
	return ProviderSettings{
		Model:         cfg.DeepSeek.Model,
		Effort:        cfg.DeepSeek.Effort,
		SlashCommands: cfg.DeepSeek.SlashCommands,
	}
}

func (deepseekProvider) SaveSettings(s ProviderSettings) error {
	return withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.DeepSeek.Model = s.Model
		cfg.DeepSeek.Effort = s.Effort
		cfg.DeepSeek.SlashCommands = s.SlashCommands
		return saveConfig(cfg)
	})
}

func (deepseekProvider) Materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	return deepseekStore().materialize(workspace, turns)
}
