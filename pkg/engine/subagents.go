package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/agenttool"
	"google.golang.org/genai"
)

// SubagentDef is a named subagent definition.
type SubagentDef struct {
	Name        string
	Description string
	Provider    string
	Model       string
	Tools       []string
	Prompt      string
	Source      string
}

// SubagentSearchDirs returns the subagent search directories in precedence order.
func SubagentSearchDirs(cwd string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".config", "ask", "agents"),
			filepath.Join(home, ".claude", "agents"),
		)
	}
	roots := []string{cwd}
	if root := config.ProjectRoot(cwd); root != "" && root != cwd {
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

// DiscoverSubagents reads every *.md definition.
func DiscoverSubagents(cwd string) []SubagentDef {
	byName := map[string]SubagentDef{}
	repoRoot := config.ProjectRoot(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}
	for _, dir := range SubagentSearchDirs(cwd) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			fields, body, ok := ParseMarkdownFrontmatter(path)
			if !ok {
				continue
			}
			name := fields["name"]
			if name == "" {
				name = strings.TrimSuffix(e.Name(), ".md")
			}
			if fields["description"] == "" {
				continue
			}
			var subTools []string
			for _, t := range strings.Split(fields["tools"], ",") {
				if t = strings.TrimSpace(t); t != "" {
					subTools = append(subTools, t)
				}
			}
			prompt := strings.TrimSpace(body)
			if linked := RuleLinkedDocs(repoRoot, prompt); len(linked) > 0 {
				var lb strings.Builder
				lb.WriteString(prompt)
				lb.WriteString("\n\n## @-linked docs\n\n")
				for _, d := range linked {
					fmt.Fprintf(&lb, "<file path=%q>\n%s\n</file>\n", d.Path, d.Body)
				}
				prompt = lb.String()
			}
			byName[name] = SubagentDef{
				Name:        name,
				Description: fields["description"],
				Provider:    strings.TrimSpace(fields["provider"]),
				Model:       strings.TrimSpace(fields["model"]),
				Tools:       subTools,
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
	out := make([]SubagentDef, 0, len(byName))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

// SubagentsPromptBlock lists the named subagents in the system prompt.
func SubagentsPromptBlock(defs []SubagentDef) string {
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

// ResolveSubagentModel builds the child Client for a def.
func ResolveSubagentModel(def SubagentDef, parentProviderID string, parent *genai.Client, cfg config.Config) (*genai.Client, int64, error) {
	providerID := def.Provider
	if providerID == "" && def.Model == "" {
		return parent, 0, nil
	}
	if providerID == "" {
		providerID = parentProviderID
	}
	spec, ok := providers.GetAgentProviderSpec(providerID)
	if !ok {
		return nil, 0, fmt.Errorf("subagent %s: provider %q is not an in-process provider", def.Name, providerID)
	}
	model := def.Model
	if model == "" {
		model = spec.DefaultModel
	}
	client, err := spec.BuildClient(cfg)
	if err != nil {
		return nil, 0, err
	}
	var budget int64
	if spec.MaxOutputTokens != nil {
		budget = spec.MaxOutputTokens(model)
	}
	return client, budget, nil
}

// SubagentToolNames returns the slice of tool names allowed for the subagent.
func SubagentToolNames(def SubagentDef) []string {
	readOnly := []string{"read", "glob", "grep", "ls"}
	full := []string{"read", "glob", "grep", "ls", "write", "edit", "bash", "job_output", "job_kill", "fetch", "todos"}

	switch {
	case len(def.Tools) == 1 && def.Tools[0] == "*":
		return full
	case len(def.Tools) > 0:
		var names []string
		valid := map[string]bool{
			"read": true, "glob": true, "grep": true, "ls": true,
			"write": true, "edit": true, "bash": true,
			"job_output": true, "job_kill": true, "fetch": true, "todos": true,
		}
		for _, t := range def.Tools {
			key := strings.ToLower(strings.TrimSpace(t))
			if valid[key] {
				names = append(names, key)
			}
		}
		if len(names) == 0 {
			return readOnly
		}
		return names
	default:
		return readOnly
	}
}

// SubagentTools filters the provided tools map by the subagent's allowed tool names.
func SubagentTools(def SubagentDef, available map[string]Tool) []Tool {
	names := SubagentToolNames(def)
	out := make([]Tool, 0, len(names))
	for _, name := range names {
		if t, ok := available[name]; ok {
			out = append(out, t)
		}
	}
	return out
}

const ResearchSubagentInstruction = `You are a specialized research subagent inside ask.
Your role is to perform deep investigations, code sweeps, searches, and file readings.
Investigate the task you are given thoroughly: search broadly, read the relevant files, and chase cross-references until you can answer with confidence.
Your final response is returned verbatim to the calling agent as data, so make it a complete, self-contained report: state the answer first, then the supporting evidence as file_path:line_number references. Report honestly when something cannot be found.`

// BuildResearchSubagent creates the default research subagent as an ADK agent.Agent.
func BuildResearchSubagent(ctx context.Context, llm model.LLM, tools []tool.Tool) (agent.Agent, error) {
	return llmagent.New(llmagent.Config{
		Name:        "researcher",
		Description: "A specialized research subagent for file investigation, code sweeps, and deep searches.",
		Model:       llm,
		Instruction: ResearchSubagentInstruction,
		Tools:       tools,
	})
}

// BuildNamedSubagent creates an ADK agent.Agent for a discovered SubagentDef.
func BuildNamedSubagent(ctx context.Context, def SubagentDef, llm model.LLM, tools []tool.Tool) (agent.Agent, error) {
	instruction := def.Prompt
	if instruction == "" {
		instruction = ResearchSubagentInstruction
	}
	return llmagent.New(llmagent.Config{
		Name:        def.Name,
		Description: def.Description,
		Model:       llm,
		Instruction: instruction,
		Tools:       tools,
	})
}

// BuildResearchAgentTool wraps the research subagent as an ADK tool.Tool using agenttool.
func BuildResearchAgentTool(ctx context.Context, llm model.LLM, tools []tool.Tool) (tool.Tool, error) {
	sub, err := BuildResearchSubagent(ctx, llm, tools)
	if err != nil {
		return nil, err
	}
	return agenttool.New(sub, &agenttool.Config{SkipSummarization: true}), nil
}

// BuildNamedAgentTool wraps a named subagent as an ADK tool.Tool using agenttool.
func BuildNamedAgentTool(ctx context.Context, def SubagentDef, llm model.LLM, tools []tool.Tool) (tool.Tool, error) {
	sub, err := BuildNamedSubagent(ctx, def, llm, tools)
	if err != nil {
		return nil, err
	}
	return agenttool.New(sub, &agenttool.Config{SkipSummarization: true}), nil
}
