package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/fantasy"
)

// agent_subagents.go discovers named subagent definitions — the
// claude-code `.claude/agents/*.md` format (frontmatter + body system
// prompt) — and resolves them into runnable child agents for the task
// tool. ask extends the format with a `provider` field so a subagent
// can pin a DIFFERENT in-process provider than the parent session
// (e.g. an anthropic session delegating research to deepseek), and
// the `model` field accepts either a full model id or the claude
// aliases (sonnet/opus/haiku) when the target provider is anthropic.
type subagentDef struct {
	Name        string
	Description string
	// Provider pins an in-process API provider id ("anthropic",
	// "openai", "deepseek"). Empty inherits the parent session's model.
	Provider string
	// Model pins a model id under Provider (or under the parent's
	// provider when Provider is empty). Empty = provider default.
	Model string
	// Tools is the allowlist from the frontmatter (comma-separated).
	// Empty = the read-only research set; "*" grants the full core
	// coding set.
	Tools []string
	// Prompt is the markdown body — the subagent's system prompt.
	Prompt string
	Source string
}

// subagentSearchDirs mirrors skillSearchDirs: user-global then
// project, later wins.
func subagentSearchDirs(cwd string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".config", "ask", "agents"),
			filepath.Join(home, ".claude", "agents"),
		)
	}
	roots := []string{cwd}
	if root := projectRoot(cwd); root != "" && root != cwd {
		roots = append(roots, root)
	}
	for _, root := range roots {
		dirs = append(dirs,
			filepath.Join(root, ".claude", "agents"),
			filepath.Join(root, ".ask", "agents"),
		)
	}
	return dirs
}

// discoverSubagents reads every *.md definition. Files without a name
// or description are skipped with a debug note.
func discoverSubagents(cwd string) []subagentDef {
	byName := map[string]subagentDef{}
	repoRoot := projectRoot(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}
	for _, dir := range subagentSearchDirs(cwd) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			fields, body, ok := parseMarkdownFrontmatter(path)
			if !ok {
				continue
			}
			name := fields["name"]
			if name == "" {
				name = strings.TrimSuffix(e.Name(), ".md")
			}
			if fields["description"] == "" {
				debugLog("subagent %s skipped: description is required", path)
				continue
			}
			var tools []string
			for _, t := range strings.Split(fields["tools"], ",") {
				if t = strings.TrimSpace(t); t != "" {
					tools = append(tools, t)
				}
			}
			prompt := strings.TrimSpace(body)
			if linked := ruleLinkedDocs(repoRoot, prompt); len(linked) > 0 {
				var lb strings.Builder
				lb.WriteString(prompt)
				lb.WriteString("\n\n## @-linked docs\n\n")
				for _, d := range linked {
					fmt.Fprintf(&lb, "<file path=%q>\n%s\n</file>\n", d.Path, d.Body)
				}
				prompt = lb.String()
			}
			byName[name] = subagentDef{
				Name:        name,
				Description: fields["description"],
				Provider:    strings.TrimSpace(fields["provider"]),
				Model:       strings.TrimSpace(fields["model"]),
				Tools:       tools,
				Prompt:      prompt,
				Source:      path,
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]subagentDef, 0, len(byName))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

// subagentsPromptBlock lists the named subagents in the system prompt
// so the model knows what it can delegate to via the task tool.
func subagentsPromptBlock(defs []subagentDef) string {
	if len(defs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<available_agents>\n")
	for _, d := range defs {
		fmt.Fprintf(&b, "  <agent><name>%s</name><description>%s</description></agent>\n", d.Name, d.Description)
	}
	b.WriteString("</available_agents>\n")
	b.WriteString(`<agents_usage>
Named agents run through the task tool: pass agent:"<name>" with a self-contained prompt. Each runs in its own context with its own instructions (and possibly its own model), and returns one final report. Set run_in_background:true to keep working while it runs, then collect the report with job_output.
</agents_usage>`)
	return b.String()
}

// agentSpecByID resolves an in-process provider spec from the
// registry. Subprocess providers (claude/codex CLI) are not specs and
// cannot host subagents.
func agentSpecByID(id string) (*agentProviderSpec, bool) {
	for _, p := range providerRegistry {
		if ap, ok := p.(agentAPIProvider); ok && ap.spec.id == id {
			return ap.spec, true
		}
	}
	return nil, false
}

// anthropicModelAlias maps the claude-code model aliases onto catalog
// ids so existing .claude/agents files work unchanged. Non-alias
// values pass through.
func anthropicModelAlias(model string) string {
	alias := strings.ToLower(strings.TrimSpace(model))
	switch alias {
	case "sonnet", "opus", "haiku", "fable":
	default:
		return model
	}
	for _, id := range catalogModelIDs("anthropic") {
		if strings.Contains(id, alias) {
			return id
		}
	}
	return model
}

// resolveSubagentModel builds the child LanguageModel for a def:
// inherit the parent's model when nothing is pinned, otherwise build
// from the pinned provider spec (cross-provider delegation). The
// returned budget is the pinned model's max-output-tokens; 0 means
// inherit the parent's budget.
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
		model = spec.defaultModel
	}
	if spec.id == anthropicProviderID {
		model = anthropicModelAlias(model)
	}
	cfg, _ := loadConfig()
	lm, err := spec.buildModel(cfg, model)
	if err != nil {
		return nil, 0, err
	}
	var budget int64
	if spec.maxOutputTokens != nil {
		budget = spec.maxOutputTokens(model)
	}
	return lm, budget, nil
}

// subagentTools maps a def's allowlist onto the core tools. The
// default (no tools listed) is the read-only research set; "*" grants
// the full coding core. Modal-coupled tools (ask_user_question,
// end_turn) and nested task are never granted.
func subagentTools(def subagentDef, env *agentToolEnv) []fantasy.AgentTool {
	available := map[string]func() fantasy.AgentTool{
		"read":       func() fantasy.AgentTool { return agentReadTool(env) },
		"glob":       func() fantasy.AgentTool { return agentGlobTool(env) },
		"grep":       func() fantasy.AgentTool { return agentGrepTool(env) },
		"ls":         func() fantasy.AgentTool { return agentLsTool(env) },
		"write":      func() fantasy.AgentTool { return agentWriteTool(env) },
		"edit":       func() fantasy.AgentTool { return agentEditTool(env) },
		"bash":       func() fantasy.AgentTool { return agentBashTool(env) },
		"job_output": func() fantasy.AgentTool { return agentJobOutputTool(env) },
		"job_kill":   func() fantasy.AgentTool { return agentJobKillTool(env) },
		"fetch":      func() fantasy.AgentTool { return agentFetchTool(env) },
		"todos":      func() fantasy.AgentTool { return agentTodosTool(env) },
	}
	readOnly := []string{"read", "glob", "grep", "ls"}
	full := []string{"read", "glob", "grep", "ls", "write", "edit", "bash", "job_output", "job_kill", "fetch", "todos"}

	names := readOnly
	switch {
	case len(def.Tools) == 1 && def.Tools[0] == "*":
		names = full
	case len(def.Tools) > 0:
		names = nil
		for _, t := range def.Tools {
			key := strings.ToLower(t)
			if _, ok := available[key]; ok {
				names = append(names, key)
			} else {
				debugLog("subagent %s: unknown tool %q ignored", def.Name, t)
			}
		}
		if len(names) == 0 {
			names = readOnly
		}
	}
	tools := make([]fantasy.AgentTool, 0, len(names))
	for _, name := range names {
		tools = append(tools, available[name]())
	}
	return tools
}
