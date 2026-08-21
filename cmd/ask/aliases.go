package main

import (
	"context"
	"strings"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/tools"
	"github.com/Cidan/ask/pkg/workflow"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"
)

// Workflow aliases
const (
	workflowSourceIssue = workflow.SourceKindIssue
	workflowSourceChat  = workflow.SourceKindChat
	workflowSourceText  = workflow.SourceKindText
)

type (
	workflowSource = workflow.Source
	chatTurn       = workflow.ChatTurn
	stepPromptCtx  = workflow.StepPromptCtx
	loopPromptCtx  = workflow.LoopPromptCtx
)

func toPkgWorkflowDef(d workflowDef) workflow.Def {
	return workflow.ConfigDefToDef(d)
}

func fromPkgWorkflowDef(d workflow.Def) workflowDef {
	return workflow.DefToConfigDef(d)
}

func issueWorkflowSource(it issueRef) workflowSource {
	return workflow.Source{
		Kind:         workflow.SourceKindIssue,
		IssueDisplay: it.Display(),
		IssueKey:     it.Key(),
	}
}

func chatWorkflowSource(originTabID int, turns any) workflowSource {
	switch t := turns.(type) {
	case []historyEntry:
		return workflow.NewChatSource(originTabID, chatTurnsFromHistory(t))
	case []chatTurn:
		return workflow.NewChatSource(originTabID, t)
	default:
		return workflow.NewChatSource(originTabID, nil)
	}
}

func textWorkflowSource(originTabID int, appendText string) workflowSource {
	return workflow.NewTextSource(originTabID, appendText)
}

func chatTurnsFromHistory(history []historyEntry) []chatTurn {
	var out []chatTurn
	for _, e := range history {
		var role string
		switch e.kind {
		case histUser:
			role = "user"
		case histResponse:
			role = "assistant"
		default:
			continue
		}
		txt := strings.TrimSpace(e.text)
		if txt == "" {
			continue
		}
		out = append(out, chatTurn{Role: role, Text: txt})
	}
	return out
}

func currentWorkflowStepMeta(r *workflowRunState) (name, provider, model string) {
	if r == nil || r.StepIdx < 0 || r.StepIdx >= len(r.Workflow.Steps) {
		return "", "", ""
	}
	top := r.Workflow.Steps[r.StepIdx]
	return top.Name, top.Provider, top.Model
}

func toPkgWorkflowStep(s workflowStep) workflow.Step {
	inner := make([]workflow.Step, len(s.Steps))
	for j, in := range s.Steps {
		inner[j] = workflow.Step{
			Name:          in.Name,
			Kind:          in.Kind,
			Provider:      in.Provider,
			Model:         in.Model,
			Prompt:        in.Prompt,
			MaxIterations: in.MaxIterations,
			ExitCondition: in.ExitCondition,
		}
	}
	return workflow.Step{
		Name:          s.Name,
		Kind:          s.Kind,
		Provider:      s.Provider,
		Model:         s.Model,
		Prompt:        s.Prompt,
		Steps:         inner,
		MaxIterations: s.MaxIterations,
		ExitCondition: s.ExitCondition,
	}
}

func buildWorkflowStepInstruction(step workflowStep, source workflowSource, pc *stepPromptCtx) string {
	return workflow.BuildStepInstruction(toPkgWorkflowStep(step), source, pc)
}

var (
	loopNoteLine = workflow.LoopNoteLine
)

func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// Tools aliases
const (
	mcpServerTypeHTTP  = tools.MCPServerTypeHTTP
	mcpServerTypeStdio = tools.MCPServerTypeStdio
	mcpServerTypeSSE   = tools.MCPServerTypeSSE
)

type (
	mcpManager               = tools.MCPManager
	mcpServerConfig          = tools.MCPServerConfig
	agentMCPServer           = tools.MCPServer
	agentFinalizedPlanParams = tools.FinalizedPlanParams
)

func ensureSudoIPCServer(interaction ...engine.InteractionHandler) *tools.SudoIPCServer {
	var handler engine.InteractionHandler = globalTUIInteractionHandler
	if len(interaction) > 0 && interaction[0] != nil {
		handler = interaction[0]
	}
	return tools.EnsureSudoIPCServer(handler)
}

var (
	newMCPManager            = tools.NewMCPManager
	resolveMCPServers        = tools.ResolveMCPServers
	unwrapInvokeToolCall     = tools.UnwrapInvokeToolCall
	runAskPassHelper         = tools.RunAskPassHelper
	applyBashFilter          = tools.ApplyBashFilter
	agentAskUserQuestionTool = tools.AskUserQuestionTool
	agentEndTurnTool         = tools.EndTurnTool
	agentFinalizedPlanTool   = tools.FinalizedPlanTool
	agentFinishWorkflowTool  = tools.FinishWorkflowTool
	agentBashTool            = tools.BashTool
	agentJobOutputTool       = tools.JobOutputTool
	agentJobKillTool         = tools.JobKillTool
	agentReadTool            = tools.ReadTool
	agentWriteTool           = tools.WriteTool
	agentEditTool            = tools.EditTool
	agentGlobTool            = tools.GlobTool
	agentGrepTool            = tools.GrepTool
	agentLsTool              = tools.LsTool
	agentFetchTool           = tools.FetchTool
	agentTodosTool           = tools.TodosTool
	agentWorkflowTools       = tools.WorkflowTools
	agentSearchToolsTool     = tools.SearchToolsTool
	agentInvokeToolTool      = tools.InvokeToolTool
	agentWebSearchTool       = tools.WebSearchTool
	agentLoadMemoryTool      = tools.LoadMemoryTool
	agentPreloadMemoryTool   = tools.PreloadMemoryTool
)

func errResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

func okResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// Memory aliases
func openMemoryService(cfg ...askConfig) error {
	return memory.Open(memory.Options{})
}

func closeMemoryService() error {
	return memory.Close()
}

func memoryServiceOpen() bool {
	return memory.IsOpen()
}

func sweepOldMemories(ctx context.Context) error {
	return memory.Sweep(ctx)
}

func agentMemoryPromptContext(cwd, prompt string) string {
	return memory.PromptContext(context.Background(), cwd, prompt)
}

func agentMemorySystemBlock(cwd string) string {
	return memory.SystemBlock(context.Background(), cwd)
}

func wrapFileToolsWithMemory(ts []tools.Tool, cwd string) []tools.Tool {
	return tools.WrapFileToolsWithMemory(ts, cwd)
}

func agentMemoryIndexTool(env *agentToolEnv) tools.Tool {
	return tools.MemoryIndexTool(env.Cwd, env.ApprovalDenied)
}

// Provider aliases
const (
	vertexProviderID      = providers.VertexProviderID
	vertexDefaultLocation = providers.VertexDefaultLocation
	vertexDefaultModel    = providers.VertexDefaultModel
)

func vertexAgentProvider() agentAPIProvider {
	return agentAPIProvider{spec: &providers.VertexSpec}
}

var (
	catalogModel             = providers.CatalogModel
	catalogContextWindow     = providers.CatalogContextWindow
	catalogDefaultMaxTokens  = providers.CatalogDefaultMaxTokens
	catalogClampEffort       = providers.CatalogClampEffort
	catalogResolveEffort     = providers.CatalogResolveEffort
	catalogSupportsImages    = providers.CatalogSupportsImages
	catalogModelIDs          = providers.CatalogModelIDs
	agentSpecByID            = providers.GetAgentProviderSpec
	vertexResolveLocation    = providers.VertexResolveLocation
	vertexResolveProject     = providers.VertexResolveProject
	filterVertexModelOptions = providers.FilterVertexModelOptions
	vertexEffortOptions      = providers.VertexEffortOptions
	globalEffortOptions      = providers.GlobalEffortOptions
)

func validateProviderID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	if _, ok := providers.GetSpec(id); ok {
		return nil
	}
	return nil
}

func providerMeta(provider, model string) string {
	switch {
	case provider == "":
		return model
	case model == "":
		return provider
	default:
		return provider + "/" + model
	}
}

// Engine aliases
type (
	subagentDef = engine.SubagentDef
)

var (
	discoverSubagents     = engine.DiscoverSubagents
	discoverRules         = engine.DiscoverRules
	discoverSkills        = engine.DiscoverSkills
	wrapContextAwareTools = engine.WrapContextAwareTools
	expandSkillInvocation = engine.ExpandSkillInvocation
	agentGitStatus        = engine.AgentGitStatus
)

func resolveSubagentModel(def subagentDef, parentProviderID string, parent *genai.Client) (*genai.Client, int64, error) {
	cfg, _ := loadConfig()
	return engine.ResolveSubagentModel(def, parentProviderID, parent, toPkgConfig(cfg))
}

func subagentTools(def subagentDef, env *agentToolEnv, deferred func() []tools.Tool) []tools.Tool {
	available := map[string]tools.Tool{
		"read":         tools.ReadTool(env),
		"glob":         tools.GlobTool(env),
		"grep":         tools.GrepTool(env),
		"ls":           tools.LsTool(env),
		"write":        tools.WriteTool(env),
		"edit":         tools.EditTool(env),
		"bash":         tools.BashTool(env),
		"job_output":   tools.JobOutputTool(env),
		"job_kill":     tools.JobKillTool(env),
		"fetch":        tools.FetchTool(env),
		"todos":        tools.TodosTool(env),
		"web_search":   tools.WebSearchTool(env),
		"search_tools": tools.SearchToolsTool(deferred),
	}
	for _, wt := range tools.WorkflowTools(env) {
		available[wt.Name()] = wt
	}
	isCore := func(name string) bool {
		return available[name] != nil
	}
	available["invoke_tool"] = tools.InvokeToolTool(deferred, isCore, env)
	return engine.SubagentTools(def, available)
}

func buildAgentSystemPrompt(args ProviderSessionArgs) string {
	return engine.BuildSystemPrompt(engine.PromptOptions{
		Cwd:         args.Cwd,
		InWorkflow:  args.InWorkflow,
		GitStatusFn: agentGitStatus,
	})
}
