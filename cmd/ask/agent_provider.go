package main

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/tools"
)

// agentProviderSpec describes one in-process API provider.
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
		AskUserQuestionMCP:  false,
		PermissionPromptMCP: false,
	}
}

func (p agentAPIProvider) ModelPicker() ProviderPicker {
	cfg, _ := loadConfig()
	options := p.spec.ModelOptions
	if p.spec.ID == "vertex" {
		if dynamicModels, err := providers.ListVertexModels(context.Background(), cfg.Vertex); err == nil && len(dynamicModels) > 0 {
			options = dynamicModels
		}
	}
	return ProviderPicker{
		Prompt:      "Select " + p.spec.DisplayName + " model",
		Options:     options,
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

// ProbeInit discovers user-invocable skills as slash commands.
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

func (p agentAPIProvider) PreMintSessionID(_ ProviderSessionArgs) string { return newUUIDv4() }

func (p agentAPIProvider) NativeSessionID(_ *providerProc) string { return "" }

func (p agentAPIProvider) store() *agentSessionStore {
	return &agentSessionStore{provider: p.spec.ID}
}

func (p agentAPIProvider) StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
	cfg, _ := loadConfig()
	modelID := providers.CanonicalVertexModelID(args.Model, p.spec.DefaultModel)
	client, err := p.spec.BuildClient(toPkgConfig(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", p.spec.ID, err)
	}

	store := p.store()
	callOpts, temperature := p.spec.CallOptions(modelID, args.Effort)
	session := &agentSession{
		args:          args,
		spec:          p.spec,
		client:        client,
		system:        buildAgentSystemPrompt(args),
		callOpts:      callOpts,
		temperature:   temperature,
		contextWindow: p.spec.ContextWindow(modelID),
		modelID:       modelID,
		ch:            make(chan tea.Msg, 256),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		store:         store,
		sessSvc:       engine.NewFileSessionService(p.spec.ID, args.Cwd),
	}
	if p.spec.MaxOutputTokens != nil {
		session.maxOutputTokens = p.spec.MaxOutputTokens(modelID)
	}
	if p.spec.BuildModel != nil {
		if llm, err := p.spec.BuildModel(context.Background(), toPkgConfig(cfg), modelID); err == nil {
			session.model = llm
		}
	}
	session.retryMaxRetries, session.retryInitialDelay, session.retryBackoffFactor = agentRetryOptions(cfg)

	switch {
	case args.SessionID != "":
		file, err := store.load(args.SessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: resume %s: %w", p.spec.ID, short(args.SessionID), err)
		}
		session.sessionID = args.SessionID
		session.messages = file.Messages()
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
	s.coreTools = []tools.Tool{
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
			func() *agentSession { return s }),
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
	s.coreTools = append(s.coreTools, agentWebSearchTool(env))
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

func attachmentFileParts(attachments []pendingAttachment) []engine.FilePart {
	if len(attachments) == 0 {
		return nil
	}
	parts := make([]engine.FilePart, 0, len(attachments))
	for i, a := range attachments {
		parts = append(parts, engine.FilePart{
			Path:     fmt.Sprintf("attachment-%d", i+1),
			Data:     a.data,
			MIMEType: a.mime,
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
		cfg.Vertex = pkgCfg.Vertex
		cfg.Effort = pkgCfg.Effort
		return saveConfig(cfg)
	})
}

func (p agentAPIProvider) Materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	return p.store().materialize(workspace, turns)
}
