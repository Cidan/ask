package engine

import (
	"fmt"
	"google.golang.org/genai"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/plugin"
	"github.com/Cidan/ask/pkg/providers"
)

// SubagentDef is a named subagent definition.
type SubagentDef struct {
	// Name is the task-tool name; plugin agents carry the "plugin:"
	// prefix the way Claude Code namespaces them.
	Name        string
	BareName    string
	Description string
	Provider    string
	Model       string
	Tools       []string
	Prompt      string
	Source      string
	Origin      Origin
}

// SubagentRoot is one directory of agent definitions plus the origin
// its agents are tagged with.
type SubagentRoot struct {
	Dir    string
	Origin Origin
}

// SubagentSearchRoots returns the user and project roots in precedence
// order (later wins). Plugin agents are namespaced and come from
// plugin.EnabledPlugins.
func SubagentSearchRoots(cwd string) []SubagentRoot {
	var roots []SubagentRoot
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range []string{
			filepath.Join(home, ".config", "ask", "agents"),
			filepath.Join(home, ".claude", "agents"),
		} {
			roots = append(roots, SubagentRoot{Dir: d, Origin: Origin{Scope: OriginUser}})
		}
	}
	projectRoots := []string{cwd}
	if root := config.ProjectRoot(cwd); root != "" && root != cwd {
		projectRoots = append(projectRoots, root)
	}
	for _, root := range projectRoots {
		for _, d := range []string{
			filepath.Join(root, ".claude", "agents"),
			filepath.Join(root, ".ask", "agents"),
		} {
			roots = append(roots, SubagentRoot{Dir: d, Origin: Origin{Scope: OriginProject}})
		}
	}
	return roots
}

// SubagentSearchDirs is SubagentSearchRoots without the origin tags.
func SubagentSearchDirs(cwd string) []string {
	roots := SubagentSearchRoots(cwd)
	dirs := make([]string, 0, len(roots))
	for _, r := range roots {
		dirs = append(dirs, r.Dir)
	}
	return dirs
}

// loadSubagentDef parses one definition file. prefix namespaces plugin
// agents.
func loadSubagentDef(path, repoRoot, prefix string, origin Origin) (SubagentDef, bool) {
	fields, body, ok := ParseMarkdownFrontmatter(path)
	if !ok {
		return SubagentDef{}, false
	}
	bare := strings.TrimSpace(fields["name"])
	if bare == "" {
		bare = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if fields["description"] == "" {
		return SubagentDef{}, false
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
	return SubagentDef{
		Name:        prefix + bare,
		BareName:    bare,
		Description: fields["description"],
		Provider:    strings.TrimSpace(fields["provider"]),
		Model:       strings.TrimSpace(fields["model"]),
		Tools:       subTools,
		Prompt:      prompt,
		Source:      path,
		Origin:      origin,
	}, true
}

// loadSubagentDefs lists every valid definition in root order, shadowed
// copies included; plugin agents follow.
func loadSubagentDefs(cwd string) []SubagentDef {
	repoRoot := config.ProjectRoot(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}
	var out []SubagentDef
	for _, root := range SubagentSearchRoots(cwd) {
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if d, ok := loadSubagentDef(filepath.Join(root.Dir, e.Name()), repoRoot, "", root.Origin); ok {
				out = append(out, d)
			}
		}
	}
	for _, in := range plugin.EnabledPlugins(cwd) {
		if in.Dir == "" {
			continue
		}
		origin := Origin{Scope: OriginPlugin, Plugin: in.Ref.String()}
		for _, f := range in.Contents().AgentFiles {
			if d, ok := loadSubagentDef(f, repoRoot, in.Ref.Plugin+":", origin); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

// DiscoverSubagents reads every *.md definition; later roots win on a
// name clash.
func DiscoverSubagents(cwd string) []SubagentDef {
	byName := map[string]SubagentDef{}
	for _, d := range loadSubagentDefs(cwd) {
		byName[d.Name] = d
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

// FindSubagent returns the discovered agent with name.
func FindSubagent(cwd, name string) (SubagentDef, bool) {
	for _, d := range DiscoverSubagents(cwd) {
		if d.Name == name {
			return d, true
		}
	}
	return SubagentDef{}, false
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

// AllSubagentTools is the full list of tool names available to subagents.
var AllSubagentTools = []string{
	"read", "glob", "grep", "ls", "write", "edit", "bash",
	"job_output", "job_kill", "fetch", "todos", "search_tools", "invoke_tool", "web_search",
	"workflow_list", "workflow_get", "workflow_create", "workflow_edit", "workflow_delete", "workflow_copy",
}

// SubagentToolNames returns the slice of tool names allowed for the subagent.
func SubagentToolNames(def SubagentDef) []string {
	valid := map[string]bool{
		"read": true, "glob": true, "grep": true, "ls": true,
		"write": true, "edit": true, "bash": true,
		"job_output": true, "job_kill": true, "fetch": true, "todos": true,
		"search_tools": true, "invoke_tool": true, "web_search": true,
		"workflow_list": true, "workflow_get": true, "workflow_create": true,
		"workflow_edit": true, "workflow_delete": true, "workflow_copy": true,
	}

	switch {
	case len(def.Tools) == 0 || (len(def.Tools) == 1 && def.Tools[0] == "*"):
		return append([]string(nil), AllSubagentTools...)
	default:
		var names []string
		for _, t := range def.Tools {
			key := strings.ToLower(strings.TrimSpace(t))
			if valid[key] {
				names = append(names, key)
			}
		}
		if len(names) == 0 {
			return append([]string(nil), AllSubagentTools...)
		}
		return names
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

// Subagents deliberately do NOT use ADK's tool/agenttool.
//
// agenttool.New wraps an agent as a tool, but it builds its own runner
// with a hardcoded config (agent_tool.go): no PluginConfig, so the
// subagent loses retryandreflect, and MemoryService is
// memory.InMemoryService(), so it loses ask's real memory. It also
// produces one tool per agent, which would replace task(agent: "foo")
// with a tool named foo.
//
// The task tool instead runs a nested engine.Run, which goes through
// RunnerBuilder — ask's plugins, memory service, and file session
// service — and keeps the background-job path and the subagent UI
// events. BuildResearchSubagent / BuildNamedSubagent /
// BuildResearchAgentTool / BuildNamedAgentTool were added for the
// agenttool migration, never called from production, and are deleted.
