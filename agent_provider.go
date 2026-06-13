package main

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
)

// agentProviderSpec describes one fantasy-backed in-process API
// provider. agentAPIProvider turns a spec into a full Provider
// implementation: every API provider shares the session runtime
// (agent_run.go), the tool set, the session store, and the message
// protocol — a spec contributes only identity, wire construction, and
// the provider-specific knobs (effort mapping, image capability,
// prompt-cache breakpoints).
type agentProviderSpec struct {
	id            string
	displayName   string
	defaultModel  string
	modelOptions  []string
	effortOptions []string

	// buildModel constructs the fantasy LanguageModel for a session.
	// Each spec routes through a package-level swappable var so tests
	// can stub the wire (swapDeepseekLM and friends).
	buildModel func(cfg askConfig, modelID string) (fantasy.LanguageModel, error)

	// callOptions maps ask's effort picker onto per-call provider
	// options and an optional sampling temperature.
	callOptions func(modelID, effort string) (fantasy.ProviderOptions, *float64)

	// prepareStep, when non-nil, runs before every agent step.
	// Anthropic uses it to place prompt-cache breakpoints — without
	// them the API bills the full prompt at uncached rates each turn.
	prepareStep fantasy.PrepareStepFunction

	// decorateTools post-processes the assembled tool list once per
	// session (anthropic marks the last tool definition cacheable so
	// the whole tool block joins the cached prefix).
	decorateTools func(tools []fantasy.AgentTool)

	// supportsImages gates image attachments per model.
	supportsImages func(modelID string) bool

	contextWindow func(modelID string) int64

	// maxOutputTokens is the per-turn output budget sent as max_tokens.
	// Without it fantasy's anthropic provider defaults to 4096 — with
	// always-on thinking the model burns that mid-reasoning and the
	// turn ends silently with a lone empty thinking block.
	maxOutputTokens func(modelID string) int64

	loadSettings func(askConfig) ProviderSettings
	saveSettings func(*askConfig, ProviderSettings)
}

// agentAPIProvider implements Provider generically over a spec.
type agentAPIProvider struct{ spec *agentProviderSpec }

func (p agentAPIProvider) ID() string          { return p.spec.id }
func (p agentAPIProvider) DisplayName() string { return p.spec.displayName }

func (p agentAPIProvider) Capabilities() ProviderCapabilities {
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

func (p agentAPIProvider) ModelPicker() ProviderPicker {
	return ProviderPicker{
		Prompt:      "Select " + p.spec.displayName + " model",
		Options:     p.spec.modelOptions,
		AllowCustom: true,
	}
}

func (p agentAPIProvider) EffortOptions() []string { return p.spec.effortOptions }

func (p agentAPIProvider) BaseSlashCommands() []slashCmd {
	name := p.spec.displayName
	return []slashCmd{
		{"/resume", "resume a previous " + name + " session"},
		{"/new", "start a new " + name + " session"},
		{"/clear", "start a new " + name + " session"},
		{"/effort", "select the " + name + " reasoning effort"},
	}
}

// ProbeInit discovers user-invocable skills as slash commands. The
// /name lines forward to the session (the registry match in update.go
// uses bare names), where runTurn expands them into the full skill
// invocation message.
func (p agentAPIProvider) ProbeInit(args ProviderSessionArgs) tea.Cmd {
	return func() tea.Msg {
		var entries []providerSlashEntry
		for _, s := range discoverSkills(args.Cwd) {
			if s.UserInvocable {
				entries = append(entries, providerSlashEntry{Name: s.Name, Description: s.Description})
			}
		}
		return providerInitLoadedMsg{tabID: args.TabID, slashCmds: entries}
	}
}

// PreMintSessionID: session ids are ours (the store keys on them), so
// minting up front gives the same first-turn-cancel safety claude's
// --session-id path has.
func (p agentAPIProvider) PreMintSessionID(_ ProviderSessionArgs) string { return newUUIDv4() }

func (p agentAPIProvider) NativeSessionID(_ *providerProc) string { return "" }

func (p agentAPIProvider) store() *agentSessionStore {
	return &agentSessionStore{provider: p.spec.id}
}

func (p agentAPIProvider) StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
	cfg, _ := loadConfig()
	modelID := args.Model
	if modelID == "" {
		modelID = p.spec.defaultModel
	}
	lm, err := p.spec.buildModel(cfg, modelID)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", p.spec.id, err)
	}

	store := p.store()
	providerOpts, temperature := p.spec.callOptions(modelID, args.Effort)
	session := &agentSession{
		args:          args,
		spec:          p.spec,
		model:         lm,
		system:        buildAgentSystemPrompt(args),
		providerOpts:  providerOpts,
		temperature:   temperature,
		contextWindow: p.spec.contextWindow(modelID),
		modelID:       modelID,
		ch:            make(chan tea.Msg, 32),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		store:         store,
	}
	if p.spec.maxOutputTokens != nil {
		session.maxOutputTokens = p.spec.maxOutputTokens(modelID)
	}

	switch {
	case args.SessionID != "":
		file, err := store.load(args.SessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: resume %s: %w", p.spec.id, short(args.SessionID), err)
		}
		session.sessionID = args.SessionID
		session.messages = repairDanglingToolCalls(file.Messages)
	case args.NewSessionID != "":
		session.sessionID = args.NewSessionID
	default:
		session.sessionID = newUUIDv4()
	}

	session.env = newAgentToolEnv(args.Cwd, args.TabID, args.SkipAllPermissions, session.emit)
	setupAgentSessionTools(session, cfg)

	proc := &providerProc{
		stdin:   agentStdin{s: session},
		stderr:  &stderrBuf{},
		payload: session,
	}
	session.proc = proc
	go session.run()
	return proc, session.ch, nil
}

// setupAgentSessionTools assembles the session's tool surface in two
// tiers. The CORE tools below are the only tools sent to the model's
// API tool definitions — every turn, every session. Everything else
// (the native bridge twins, every MCP server tool) goes into the
// DEFERRED REGISTRY: registered and callable, but discovered through
// search_tools and dispatched through invoke_tool instead of riding
// the wire (agent_tools_registry.go). MCP failures are logged and
// skipped — a dead remote must not block a coding session.
//
// ⚠ DO NOT add new tools to the core list. New tools belong in the
// deferred registry (s.deferredBase or an MCP server). Every core
// addition costs context tokens on every call of every session and
// churns anthropic's cached tool block. A tool earns a core slot only
// by deliberate, documented exception — the bar is "the agent cannot
// function without seeing it unprompted" (see "Tool registry vs core
// tools" in CLAUDE.md).
func setupAgentSessionTools(s *agentSession, cfg askConfig) {
	env := s.env
	s.coreTools = []fantasy.AgentTool{
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
		agentTaskTool(env,
			func() fantasy.LanguageModel { return s.model },
			func() int64 { return s.maxOutputTokens }),
		agentAskUserQuestionTool(env),
		agentEndTurnTool(env),
		agentSearchToolsTool(s.deferredTools),
		agentInvokeToolTool(s.deferredTools, s.isCoreToolName),
	}
	s.coreTools = wrapFileToolsWithMemory(s.coreTools, s.args.Cwd)
	s.coreTools = wrapReadToolWithRules(s.coreTools, s.args.Cwd, discoverRules(s.args.Cwd))
	s.deferredBase = agentBridgeTools(env)
	s.mcp = newMCPManager(s.args.TabID,
		func() bool {
			return s.spec != nil && s.spec.supportsImages != nil && s.spec.supportsImages(s.modelID)
		},
		s.refreshToolset,
	)
	s.mcp.attachAll(context.Background(), agentSessionMCPServers(s.args, cfg))
	s.refreshToolset()
}

// agentSessionMCPServers resolves every external server one session
// attaches: the project GitHub MCP slot, then the user-configured map
// (.mcp.json ← global ← project). The loopback bridge is deliberately
// absent — its tools are native in-process (agent_tools_bridge.go).
func agentSessionMCPServers(args ProviderSessionArgs, cfg askConfig) []agentMCPServer {
	var servers []agentMCPServer
	if args.ProjectMCP != nil {
		servers = append(servers, agentMCPServer{
			name: args.ProjectMCP.Name,
			cfg: mcpServerConfig{
				Type:    mcpServerTypeHTTP,
				URL:     args.ProjectMCP.URL,
				Headers: args.ProjectMCP.Headers,
			},
		})
	}
	for _, named := range resolveMCPServers(cfg, args.Cwd) {
		servers = append(servers, agentMCPServer{name: named.Name, cfg: named.Config})
	}
	return servers
}

// Send queues a user turn. Image attachments are gated on the model's
// capability: silently dropping a paste would be worse than saying so.
func (p agentAPIProvider) Send(proc *providerProc, text string, attachments []pendingAttachment) error {
	session, ok := proc.payload.(*agentSession)
	if !ok {
		return errors.New(p.spec.id + ": proc payload is not an agent session")
	}
	if len(attachments) > 0 && (p.spec.supportsImages == nil || !p.spec.supportsImages(session.modelID)) {
		return errors.New(p.spec.displayName + " model " + session.modelID +
			" does not support image attachments — remove the image and resend")
	}
	return session.queueTurn(text, attachmentFileParts(attachments))
}

// attachmentFileParts converts pasted attachments into fantasy file
// parts for the user message.
func attachmentFileParts(attachments []pendingAttachment) []fantasy.FilePart {
	if len(attachments) == 0 {
		return nil
	}
	parts := make([]fantasy.FilePart, 0, len(attachments))
	for i, a := range attachments {
		parts = append(parts, fantasy.FilePart{
			Filename:  fmt.Sprintf("attachment-%d", i+1),
			Data:      a.data,
			MediaType: a.mime,
		})
	}
	return parts
}

// Interrupt cancels the in-flight turn cooperatively; the session
// stays alive and emits its own turn completion, so handled=true keeps
// killProc out of the picture (same contract as codex).
func (p agentAPIProvider) Interrupt(proc *providerProc) (bool, error) {
	session, ok := proc.payload.(*agentSession)
	if !ok {
		return false, nil
	}
	return session.interruptTurn(), nil
}

func (p agentAPIProvider) ListSessions(cwd string) ([]sessionEntry, error) {
	return p.store().list(cwd)
}

func (p agentAPIProvider) LoadHistory(sessionID string, opts HistoryOpts) ([]historyEntry, error) {
	return p.store().loadHistory(sessionID, opts)
}

func (p agentAPIProvider) LoadSettings() ProviderSettings {
	cfg, _ := loadConfig()
	return p.spec.loadSettings(cfg)
}

func (p agentAPIProvider) SaveSettings(s ProviderSettings) error {
	return withConfigLock(func() error {
		cfg, _ := loadConfig()
		p.spec.saveSettings(&cfg, s)
		return saveConfig(cfg)
	})
}

func (p agentAPIProvider) Materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	return p.store().materialize(workspace, turns)
}
