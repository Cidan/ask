package tools

import (
	"github.com/Cidan/ask/pkg/engine"
)

func init() {
	engine.RegisterToolFactory(func(args engine.ToolFactoryArgs) []engine.Tool {
		coreTools := BuildCoreTools(args, args.AttachWebSearch)
		out := make([]engine.Tool, len(coreTools))
		for i, t := range coreTools {
			out[i] = t
		}
		return out
	})
}

// BuildCoreTools constructs standard core wire tools for the provided engine arguments.
func BuildCoreTools(args engine.ToolFactoryArgs, attachWebSearch bool) []Tool {
	env := NewToolEnv(
		args.Cwd,
		args.TabID,
		args.SkipPermissions,
		args.EventListener,
		args.InteractionHandler,
	)
	env.SupportsImages = args.SupportsImages
	registry := append(ExtensionTools(env), MemoryTools(env.Cwd, env.ApprovalDenied)...)
	registryFunc := func() []Tool {
		return registry
	}
	core := CoreTools(env, registryFunc, attachWebSearch)
	if args.WorkflowStep {
		// end_turn records each step's workflow-log summary, so it is only
		// on the wire during a workflow run — never in an ordinary turn.
		core = append(core, EndTurnTool(env))
		core = append(core, WorkflowStepTools(env, args.WorkflowFinalStep)...)
	}
	return core
}

// BuildSubagentTools constructs the centralized toolset configured for an isolated subagent session.
func BuildSubagentTools(args engine.ToolFactoryArgs, attachWebSearch bool) []Tool {
	env := NewToolEnv(
		args.Cwd,
		args.TabID,
		true, // SkipPermissions: subagents bypass interactive prompts
		args.EventListener,
		args.InteractionHandler,
	)
	env.IsSubagent = true
	env.SupportsImages = args.SupportsImages
	registryFunc := func() []Tool {
		return nil
	}
	return CoreTools(env, registryFunc, attachWebSearch)
}

// CoreTools returns the standard core wire tools for an agent session.
func CoreTools(env *ToolEnv, registry func() []Tool, attachWebSearch bool) []Tool {
	var isCore func(string) bool

	if env.ImageSink == nil {
		env.ImageSink = NewImageSink()
	}
	tools := []Tool{
		ReadTool(env),
		WriteTool(env),
		EditTool(env),
		GlobTool(env),
		GrepTool(env),
		LsTool(env),
		BashTool(env),
		JobOutputTool(env),
		JobKillTool(env),
		FetchTool(env),
		TodosTool(env),
		LoadMemoryTool(env.Cwd),
		PreloadMemoryTool(env.Cwd, nil, nil),
		RenderDesignTool(env.ImageSink),
		NewImageInjectionHook(env.ImageSink, env.SupportsImages),
		SearchToolsTool(registry),
	}

	// Add workflow tools
	tools = append(tools, WorkflowTools(env)...)

	if attachWebSearch {
		tools = append(tools, WebSearchTool(env))
	}

	coreMap := make(map[string]bool, len(tools)+1)
	for _, t := range tools {
		coreMap[t.Name()] = true
	}
	coreMap["invoke_tool"] = true

	isCore = func(name string) bool {
		return coreMap[name]
	}

	tools = append(tools, InvokeToolTool(registry, isCore, env))
	return tools
}

// IsCoreTool reports whether a tool name belongs to the core toolset.
func IsCoreTool(name string) bool {
	switch name {
	case "read", "write", "edit", "glob", "grep", "ls", "bash", "job_output", "job_kill",
		"fetch", "todos", "task", "end_turn", "search_tools", "invoke_tool",
		"web_search", "workflow_list", "workflow_get", "workflow_create", "workflow_edit",
		"workflow_delete", "workflow_copy", "load_memory", "preload_memory",
		"render_design", "inject_tool_images":
		return true
	default:
		return false
	}
}
