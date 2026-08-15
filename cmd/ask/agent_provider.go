package main

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

// agentProviderSpec describes one fantasy-backed in-process API provider.
type agentProviderSpec = providers.AgentProviderSpec

// toPkgConfig returns config.Config (which askConfig is aliased to).
func toPkgConfig(c askConfig) config.Config {
	return c
}

// agentAPIProvider implements Provider generically over a spec.
type agentAPIProvider struct{ spec *agentProviderSpec }

func (p agentAPIProvider) ID() string          { return p.spec.ID }
func (p agentAPIProvider) DisplayName() string { return p.spec.DisplayName }

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
		Prompt:      "Select " + p.spec.DisplayName + " model",
		Options:     p.spec.ModelOptions,
		AllowCustom: true,
	}
}

func (p agentAPIProvider) EffortOptions() []string { return p.spec.EffortOptions }

func (p agentAPIProvider) BaseSlashCommands() []slashCmd {
	name := p.spec.DisplayName
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
	return &agentSessionStore{provider: p.spec.ID}
}

func (p agentAPIProvider) StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
	cfg, _ := loadConfig()
	modelID := args.Model
	if modelID == "" {
		modelID = p.spec.DefaultModel
	}
	lm, err := p.spec.BuildModel(toPkgConfig(cfg), modelID)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", p.spec.ID, err)
	}

	store := p.store()
	providerOpts, temperature := p.spec.CallOptions(modelID, args.Effort)
	session := &agentSession{
		args:          args,
		spec:          p.spec,
		model:         lm,
		system:        buildAgentSystemPrompt(args),
		providerOpts:  providerOpts,
		temperature:   temperature,
		contextWindow: p.spec.ContextWindow(modelID),
		modelID:       modelID,
		ch:            make(chan tea.Msg, 256),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		store:         store,
	}
	if p.spec.MaxOutputTokens != nil {
		session.maxOutputTokens = p.spec.MaxOutputTokens(modelID)
	}
	session.retryMaxRetries, session.retryInitialDelay, session.retryBackoffFactor = agentRetryOptions(cfg)

	switch {
	case args.SessionID != "":
		file, err := store.load(args.SessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: resume %s: %w", p.spec.ID, short(args.SessionID), err)
		}
		session.sessionID = args.SessionID
		session.messages = repairDanglingToolCalls(file.Messages)
	case args.NewSessionID != "":
		session.sessionID = args.NewSessionID
	default:
		session.sessionID = newUUIDv4()
	}

	session.env = newAgentToolEnv(args.Cwd, args.TabID, args.SkipAllPermissions, cfg.UI.GateTodosBeforeMutate != nil && *cfg.UI.GateTodosBeforeMutate, session.emit)
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

// setupAgentSessionTools assembles the session's tool surface in two tiers.
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
		agentInvokeToolTool(s.deferredTools, s.isCoreToolName, env),
	}
	if !s.args.InWorkflow {
		s.coreTools = append(s.coreTools, agentFinalizedPlanTool(env))
	}
	if s.args.IsWorkflowFinalStep {
		s.coreTools = append(s.coreTools, agentFinishWorkflowTool(env))
	}
	if !s.args.InWorkflow {
		s.coreTools = append(s.coreTools, agentWorkflowTools(env)...)
	}
	if s.spec != nil && s.spec.NativeWebSearch != nil {
		s.providerWebSearch = s.spec.NativeWebSearch(s.modelID)
	} else {
		s.coreTools = append(s.coreTools, agentWebSearchTool(env))
	}
	s.coreTools = wrapFileToolsWithMemory(s.coreTools, s.args.Cwd)
	s.coreTools = wrapContextAwareTools(s.coreTools, s.args.Cwd, discoverRules(s.args.Cwd))
	s.deferredBase = agentLinearTools(env)
	s.deferredBase = append(s.deferredBase, agentMemoryIndexTool(env))
	s.mcp = newMCPManager(s.args.TabID,
		func() bool {
			return s.spec != nil && s.spec.SupportsImages != nil && s.spec.SupportsImages(s.modelID)
		},
		s.refreshToolset,
		globalTUIInteractionHandler,
	)
	s.mcp.AttachAll(context.Background(), agentSessionMCPServers(s.args, cfg))
	s.refreshToolset()
}

func agentSessionMCPServers(args ProviderSessionArgs, cfg askConfig) []agentMCPServer {
	var servers []agentMCPServer
	if args.ProjectMCP != nil {
		servers = append(servers, agentMCPServer{
			Name: args.ProjectMCP.Name,
			Cfg: mcpServerConfig{
				Type:    mcpServerTypeHTTP,
				URL:     args.ProjectMCP.URL,
				Headers: args.ProjectMCP.Headers,
			},
		})
	}
	for _, named := range resolveMCPServers(toPkgConfig(cfg), args.Cwd) {
		servers = append(servers, agentMCPServer{Name: named.Name, Cfg: named.Config})
	}
	return servers
}

func (p agentAPIProvider) Send(proc *providerProc, text string, attachments []pendingAttachment) error {
	session, ok := proc.payload.(*agentSession)
	if !ok {
		return errors.New(p.spec.ID + ": proc payload is not an agent session")
	}
	if len(attachments) > 0 && (p.spec.SupportsImages == nil || !p.spec.SupportsImages(session.modelID)) {
		return errors.New(p.spec.DisplayName + " model " + session.modelID +
			" does not support image attachments — remove the image and resend")
	}
	return session.queueTurn(text, attachmentFileParts(attachments))
}

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
	return p.spec.LoadSettings(toPkgConfig(cfg))
}

func (p agentAPIProvider) SaveSettings(s ProviderSettings) error {
	return withConfigLock(func() error {
		cfg, _ := loadConfig()
		pkgCfg := toPkgConfig(cfg)
		p.spec.SaveSettings(&pkgCfg, s)
		cfg.Anthropic = pkgCfg.Anthropic
		cfg.OpenAI = pkgCfg.OpenAI
		cfg.DeepSeek = pkgCfg.DeepSeek
		cfg.Moonshot = pkgCfg.Moonshot
		cfg.MiniMax = pkgCfg.MiniMax
		cfg.GoogleAI = pkgCfg.GoogleAI
		cfg.Vertex = pkgCfg.Vertex
		cfg.Effort = pkgCfg.Effort
		return saveConfig(cfg)
	})
}

func (p agentAPIProvider) Materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	return p.store().materialize(workspace, turns)
}
