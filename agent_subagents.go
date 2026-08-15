package main

import (
	"fmt"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/tools"
)

type subagentDef = engine.SubagentDef

var (
	subagentSearchDirs   = engine.SubagentSearchDirs
	discoverSubagents    = engine.DiscoverSubagents
	subagentsPromptBlock = engine.SubagentsPromptBlock
)

func agentSpecByID(id string) (*agentProviderSpec, bool) {
	for _, p := range providerRegistry {
		if ap, ok := p.(agentAPIProvider); ok && ap.spec.ID == id {
			return ap.spec, true
		}
	}
	return nil, false
}

func anthropicModelAlias(model string) string {
	return providers.AnthropicModelAlias(model)
}

func resolveSubagentModel(def subagentDef, parentProviderID string, parent fantasy.LanguageModel) (fantasy.LanguageModel, int64, error) {
	providerID := def.Provider
	if providerID == "" && def.Model == "" {
		return parent, 0, nil
	}
	if providerID == "" {
		providerID = parentProviderID
	}
	spec, ok := agentSpecByID(providerID)
	if !ok {
		return nil, 0, fmt.Errorf("subagent %s: provider %q is not an in-process provider", def.Name, providerID)
	}
	model := def.Model
	if model == "" {
		model = spec.DefaultModel
	}
	if spec.ID == anthropicProviderID {
		model = anthropicModelAlias(model)
	}
	cfg, _ := loadConfig()
	lm, err := spec.BuildModel(toPkgConfig(cfg), model)
	if err != nil {
		return nil, 0, err
	}
	var budget int64
	if spec.MaxOutputTokens != nil {
		budget = spec.MaxOutputTokens(model)
	}
	return lm, budget, nil
}

func subagentTools(def subagentDef, env *agentToolEnv) []fantasy.AgentTool {
	available := map[string]fantasy.AgentTool{
		"read":       tools.ReadTool(env),
		"glob":       tools.GlobTool(env),
		"grep":       tools.GrepTool(env),
		"ls":         tools.LsTool(env),
		"write":      tools.WriteTool(env),
		"edit":       tools.EditTool(env),
		"bash":       tools.BashTool(env),
		"job_output": tools.JobOutputTool(env),
		"job_kill":   tools.JobKillTool(env),
		"fetch":      tools.FetchTool(env),
		"todos":      tools.TodosTool(env),
	}
	return engine.SubagentTools(def, available)
}
