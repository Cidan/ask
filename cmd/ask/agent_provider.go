package main

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/tools"
	adksession "google.golang.org/adk/v2/session"
)

// toPkgConfig returns config.Config (which askConfig is aliased to).
func toPkgConfig(c askConfig) config.Config {
	return c
}

// agentAPIProvider adapts one providers.Provider to the TUI's Provider
// interface: sessions, settings, the picker, and slash commands are all
// generic over the registry entry.
type agentAPIProvider struct{ prov providers.Provider }

func (p agentAPIProvider) ID() string          { return p.prov.ID() }
func (p agentAPIProvider) DisplayName() string { return p.prov.DisplayName() }

func (p agentAPIProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Resume:              true,
		ModelPicker:         true,
		EffortPicker:        true,
		AskUserQuestionMCP:  false,
		PermissionPromptMCP: false,
	}
}

// ModelPicker is synchronous and never touches the network: it serves the
// provider's live listing once a catalog load (model_catalog.go) has cached
// one, and the static catalog ids until then.
func (p agentAPIProvider) ModelPicker() ProviderPicker {
	options := p.prov.ModelOptions()
	if live, ok := cachedModelOptions(p.prov.ID()); ok && len(live) > 0 {
		options = live
	}
	return ProviderPicker{
		Prompt:      "Select " + p.prov.DisplayName() + " model",
		Options:     options,
		AllowCustom: true,
	}
}

// ListModels is the network path behind the catalog load. Providers
// without the ModelLister capability report no listing and keep the
// static catalog.
func (p agentAPIProvider) ListModels(ctx context.Context) ([]string, error) {
	lister, ok := p.prov.(providers.ModelLister)
	if !ok {
		return nil, nil
	}
	cfg, _ := loadConfig()
	return lister.ListModels(ctx, cfg.ProviderConfig(p.prov.ID()))
}

func (p agentAPIProvider) EffortOptions() []string { return p.prov.EffortOptions() }

func (p agentAPIProvider) BaseSlashCommands() []slashCmd {
	name := p.prov.DisplayName()
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
	return &agentSessionStore{provider: p.prov.ID()}
}

// StartSession builds the session's model up front so a provider that
// cannot run (no project, no key) fails here, with the provider's own
// message, instead of on the first turn.
func (p agentAPIProvider) StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
	cfg, _ := loadConfig()
	modelID := p.prov.CanonicalModelID(args.Model, p.prov.DefaultModel())

	store := p.store()
	callOpts, temperature := p.prov.CallOptions(modelID, args.Effort)
	session := &agentSession{
		args:            args,
		provider:        p.prov,
		system:          buildAgentSystemPrompt(args),
		callOpts:        callOpts,
		temperature:     temperature,
		contextWindow:   p.prov.ContextWindow(modelID),
		maxOutputTokens: p.prov.MaxOutputTokens(modelID),
		modelID:         modelID,
		ch:              make(chan tea.Msg, 256),
		sendCh:          make(chan agentTurn, 8),
		midTurnQueue:    &engine.MidTurnQueue{},
		closed:          make(chan struct{}),
		store:           store,
		sessSvc:         engine.NewFileSessionService(p.prov.ID(), args.Cwd),
		topic:           memory.NormalizeTopic(args.Topic),
	}
	// Build the model up front so a provider that cannot run fails here, with
	// its own message, instead of on the first turn. Thread an observed-tool
	// sink so tools the provider runs natively (Claude Code's WebSearch
	// fallback when no Brave key is set) still render and record in ask's
	// transcript rather than being invisible.
	buildCtx := providers.WithObservedToolSink(context.Background(), newObservedToolSink(session.emit))
	llm, err := engine.ModelBuilder(buildCtx, p.prov, toPkgConfig(cfg), modelID)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", p.prov.ID(), err)
	}
	session.model = llm
	session.retryMaxRetries, session.retryInitialDelay, session.retryBackoffFactor = agentRetryOptions(cfg)

	switch {
	case args.SessionID != "":
		if _, err := session.sessSvc.Get(context.Background(), &adksession.GetRequest{
			AppName:   "ask",
			UserID:    "user",
			SessionID: args.SessionID,
		}); err != nil {
			return nil, nil, fmt.Errorf("%s: resume %s: %w", p.prov.ID(), short(args.SessionID), err)
		}
		session.sessionID = args.SessionID
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
		agentLoadMemoryTool(s.args.Cwd),
		agentPreloadMemoryTool(s.args.Cwd, s.currentTopic, s.setTopic),
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
	// Omit ask's Brave-backed web_search when the provider supplies a native
	// fallback and no Brave key is set, so the model sees exactly one web
	// search tool (the provider's native one) instead of a dead ask tool.
	if !nativeWebSearchActive(s.provider, cfg) {
		s.coreTools = append(s.coreTools, agentWebSearchTool(env))
	}
	s.coreTools = wrapFileToolsWithMemory(s.coreTools, s.args.Cwd)
	s.coreTools = wrapContextAwareTools(s.coreTools, s.args.Cwd, discoverRules(s.args.Cwd))
	s.deferredBase = agentLinearTools(env)
	s.deferredBase = append(s.deferredBase, agentMemoryTools(env)...)
	s.deferredBase = append(s.deferredBase, agentExtensionTools(env)...)
	s.mcp = newMCPManager(s.args.TabID,
		func() bool {
			return s.provider != nil && s.provider.SupportsImages(s.modelID)
		},
		s.refreshToolset,
		func() { s.emit(engine.MCPStatusChangedEvent{BaseEvent: engine.BaseEvent{TabID: s.args.TabID}}) },
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
		return errors.New(p.prov.ID() + ": proc payload is not an agent session")
	}
	if len(attachments) > 0 && !p.prov.SupportsImages(session.modelID) {
		return errors.New(p.prov.DisplayName() + " model " + session.modelID +
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

func (p agentAPIProvider) LoadHistory(sessionID string) ([]transcriptItem, error) {
	return p.store().loadTranscript(sessionID)
}

func (p agentAPIProvider) LoadSettings() ProviderSettings {
	cfg, _ := loadConfig()
	return providers.LoadSettings(toPkgConfig(cfg), p.prov.ID())
}

func (p agentAPIProvider) SaveSettings(s ProviderSettings) error {
	return withConfigLock(func() error {
		cfg, _ := loadConfig()
		providers.SaveSettings(&cfg, p.prov.ID(), s)
		return saveConfig(cfg)
	})
}

func (p agentAPIProvider) Materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	return p.store().materialize(workspace, turns)
}
