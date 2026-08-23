package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/plugin"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/workflow"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The extension tools live in the deferred registry (search_tools /
// invoke_tool), never on the wire: they manage skills, agents, plugins,
// and marketplaces, and a session that never touches them pays nothing
// for their schemas. Every mutation bumps the skills generation (so the
// live ADK skill source rescans) and emits ExtensionsChangedEvent (so
// the TUI re-registers slash commands and refreshes the browser).

const (
	SkillListToolDescription = `List every skill, agent, and workflow available to this session, with its origin.

Each item carries: kind ('skill', 'agent', or 'workflow'), name (the invocation name — plugin items are namespaced 'plugin:name'), description, origin ('user', 'project', or 'plugin <name@marketplace>'), the path on disk, and for skills whether the user can run it as a slash command and whether you may load it on your own. Workflows list their steps and warn when a step's provider is not configured. Also lists the enabled plugins. Pass kind to filter.`

	SkillGetToolDescription = `Read one skill or agent in full — its frontmatter fields and the complete body (instructions / system prompt).

Use this before editing, and when you need the exact wording of a skill rather than the summary in skill_list.`

	SkillCreateToolDescription = `Create a new skill: a SKILL.md package the user can invoke as /name and that you will be offered as a trigger in future sessions.

name must be kebab-case (lowercase letters, digits, single hyphens). description is the trigger: one or two sentences saying WHEN the skill applies and WHAT it does — the model matches tasks against it, so be concrete. body is the instruction markdown the model follows once the skill is loaded; write it for a model that has none of this conversation's context. scope is 'project' (default — <project root>/.ask/skills/, committed with the repo) or 'user' (~/.config/ask/skills/, follows the user across projects).

When asked to turn the current conversation into a skill, distil the reusable procedure (steps, commands, gotchas, file locations), not the one-off details. The skill is live immediately: its slash command registers as soon as this call returns, and the user can iterate on it with skill_edit.`

	SkillEditToolDescription = `Edit an existing user- or project-scope skill in place. Omit a field to leave it unchanged; body replaces the whole instruction markdown. Plugin skills are read-only — create a user/project copy with skill_create instead. Pass scope when the same name exists in both user and project scope.`

	SkillDeleteToolDescription = `Delete a user- or project-scope skill package (the whole directory, including its scripts/ and references/). Plugin skills cannot be deleted here — uninstall the plugin. Pass scope when the name is ambiguous.`

	AgentCreateToolDescription = `Create a named subagent definition the task tool can run with agent:"<name>".

name is kebab-case. description says when to delegate to it. prompt is its system prompt. tools is the optional allow-list (default: the read-only set; '*' for the coding core). provider/model optionally pin a different in-process provider. scope is 'project' (default, <root>/.ask/agents/) or 'user' (~/.config/ask/agents/).`

	AgentEditToolDescription = `Edit an existing user- or project-scope agent definition in place. Omit a field to leave it unchanged; an empty string clears provider/model; prompt replaces the system prompt. Plugin agents are read-only.`

	AgentDeleteToolDescription = `Delete a user- or project-scope agent definition. Plugin agents cannot be deleted here — uninstall the plugin.`

	MarketplaceListToolDescription = `List the registered plugin marketplaces (Claude Code plugin-marketplace format) with their scope, source, whether their catalog is fetched locally, whether you can publish into them, and how many plugins they carry.`

	MarketplaceSearchToolDescription = `Search the registered marketplaces' catalogs for plugins whose name, description, category, or listed skills match the query (case-insensitive substring; '*' lists everything). Results carry the plugin reference ('name@marketplace') to pass to plugin_install, and whether it is already installed.

Use this when a task would benefit from a capability ask does not have yet (a document format, a vendor API, a review checklist): find a fitting plugin, tell the user what it provides, and offer to install it.`

	MarketplaceAddToolDescription = `Register a plugin marketplace so its plugins can be searched and installed. source is a GitHub 'owner/repo', a git URL, a local directory, or a direct URL to a marketplace.json. scope is 'user' (default, this machine) or 'project' (recorded in <root>/.ask/plugins.json so the team gets it). Asks the user for approval.`

	PluginInstallToolDescription = `Install and enable a plugin from a registered marketplace. plugin is 'name@marketplace' (from marketplace_search). scope is 'user' (default) or 'project' (enabled for everyone on this repo via <root>/.ask/plugins.json). Asks the user for approval. The plugin's skills, agents, and workflows are live immediately, namespaced 'plugin:name'.`

	PluginUninstallToolDescription = `Disable and remove an installed plugin in the given scope ('user' default, or 'project').`

	SkillPublishToolDescription = `Publish a user- or project-scope skill, agent, or workflow into a marketplace you can write to (a local directory marketplace or a git clone you own), as a plugin other people — and Claude Code users — can install.

name is the item's name; kind is 'skill' (default), 'agent', or 'workflow'; marketplace is the registered marketplace name (see marketplace_list — it must be writable). plugin_name defaults to the item name. The local copy stays the source of truth and is linked to the plugin: publishing the same item again is an update (the plugin's patch version bumps), and skill_list reports the link's sync state ('in sync', 'local changes', 'marketplace newer', 'diverged'). Git-backed marketplaces are pulled, committed, and pushed; set no_push to keep the commit local. Asks the user for approval.`

	SkillPullToolDescription = `Replace a local skill, agent, or workflow with the copy in the marketplace it was published to — the reverse of skill_publish, for when skill_list reports 'marketplace newer' (someone updated the published copy). Overwrites local changes; asks the user for approval.`
)

type ExtensionItemView struct {
	Kind           string         `json:"kind" jsonschema:"'skill', 'agent', or 'workflow'"`
	Name           string         `json:"name" jsonschema:"invocation name; plugin items are 'plugin:name'"`
	Description    string         `json:"description,omitempty"`
	Origin         string         `json:"origin" jsonschema:"'user', 'project', or 'plugin <name@marketplace>'"`
	Scope          string         `json:"scope" jsonschema:"'user', 'project', or 'plugin'"`
	Plugin         string         `json:"plugin,omitempty" jsonschema:"name@marketplace for plugin items"`
	Path           string         `json:"path,omitempty"`
	SlashCommand   string         `json:"slash_command,omitempty" jsonschema:"how the user invokes the skill"`
	UserInvocable  *bool          `json:"user_invocable,omitempty"`
	ModelInvocable *bool          `json:"model_invocable,omitempty" jsonschema:"false when disable-model-invocation is set"`
	Provider       string         `json:"provider,omitempty" jsonschema:"agents: pinned provider"`
	Model          string         `json:"model,omitempty" jsonschema:"agents: pinned model"`
	Tools          []string       `json:"tools,omitempty" jsonschema:"agents: tool allow-list"`
	Steps          []string       `json:"steps,omitempty" jsonschema:"workflows: 'name (provider/model)' per step"`
	Warnings       []string       `json:"warnings,omitempty" jsonschema:"workflows: steps whose provider is not configured"`
	Published      *PublishedView `json:"published,omitempty" jsonschema:"set when this local item was published to a marketplace"`
}

// PublishedView is the link between a local item and its plugin.
type PublishedView struct {
	Marketplace string `json:"marketplace"`
	Plugin      string `json:"plugin" jsonschema:"name@marketplace"`
	Version     string `json:"version,omitempty"`
	Status      string `json:"status" jsonschema:"'in sync', 'local changes' (skill_publish updates it), 'marketplace newer' (skill_pull takes it), 'diverged', or 'missing'"`
}

type PluginView struct {
	Ref         string   `json:"ref" jsonschema:"name@marketplace"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"`
	Scopes      []string `json:"scopes" jsonschema:"where it is enabled: user and/or project"`
	Missing     bool     `json:"missing,omitempty" jsonschema:"enabled by the project file but not fetched on this machine"`
	Skills      []string `json:"skills,omitempty"`
	Agents      []string `json:"agents,omitempty"`
	Workflows   []string `json:"workflows,omitempty"`
}

type SkillListInput struct {
	Kind string `json:"kind,omitempty" jsonschema:"optional filter: 'skill', 'agent', or 'workflow'"`
}

type SkillListOutput struct {
	Items   []ExtensionItemView `json:"items"`
	Plugins []PluginView        `json:"plugins,omitempty"`
}

type SkillGetInput struct {
	Name string `json:"name" jsonschema:"skill or agent name (plugin items: 'plugin:name')"`
	Kind string `json:"kind,omitempty" jsonschema:"'skill' (default) or 'agent'"`
}

type SkillGetOutput struct {
	Item ExtensionItemView `json:"item"`
	Body string            `json:"body" jsonschema:"the full instruction markdown (skills) or system prompt (agents)"`
}

type SkillCreateInput struct {
	Name                   string `json:"name" jsonschema:"kebab-case skill name"`
	Description            string `json:"description" jsonschema:"the trigger: when the skill applies and what it does"`
	Body                   string `json:"body" jsonschema:"instruction markdown the model follows"`
	Scope                  string `json:"scope,omitempty" jsonschema:"'project' (default) or 'user'"`
	UserInvocable          *bool  `json:"user_invocable,omitempty" jsonschema:"false hides the /name slash command"`
	DisableModelInvocation *bool  `json:"disable_model_invocation,omitempty" jsonschema:"true keeps the skill out of the model's trigger list"`
}

type SkillCreateOutput struct {
	Skill ExtensionItemView `json:"skill"`
}

type SkillEditInput struct {
	Name                   string  `json:"name" jsonschema:"existing skill name"`
	Scope                  string  `json:"scope,omitempty" jsonschema:"'user' or 'project' when the name is ambiguous"`
	Description            *string `json:"description,omitempty"`
	Body                   *string `json:"body,omitempty" jsonschema:"replaces the whole instruction markdown"`
	UserInvocable          *bool   `json:"user_invocable,omitempty"`
	DisableModelInvocation *bool   `json:"disable_model_invocation,omitempty"`
}

type SkillEditOutput struct {
	Skill ExtensionItemView `json:"skill"`
}

type SkillDeleteInput struct {
	Name  string `json:"name" jsonschema:"skill name to delete"`
	Scope string `json:"scope,omitempty" jsonschema:"'user' or 'project' when the name is ambiguous"`
}

type SkillDeleteOutput struct {
	Deleted bool `json:"deleted"`
}

type AgentCreateInput struct {
	Name        string   `json:"name" jsonschema:"kebab-case agent name"`
	Description string   `json:"description" jsonschema:"when to delegate to this agent"`
	Prompt      string   `json:"prompt" jsonschema:"the agent's system prompt"`
	Scope       string   `json:"scope,omitempty" jsonschema:"'project' (default) or 'user'"`
	Tools       []string `json:"tools,omitempty" jsonschema:"tool allow-list; '*' for the coding core"`
	Provider    string   `json:"provider,omitempty" jsonschema:"pin an in-process provider id"`
	Model       string   `json:"model,omitempty" jsonschema:"pin a model id"`
}

type AgentCreateOutput struct {
	Agent ExtensionItemView `json:"agent"`
}

type AgentEditInput struct {
	Name        string    `json:"name" jsonschema:"existing agent name"`
	Scope       string    `json:"scope,omitempty" jsonschema:"'user' or 'project' when the name is ambiguous"`
	Description *string   `json:"description,omitempty"`
	Prompt      *string   `json:"prompt,omitempty" jsonschema:"replaces the system prompt"`
	Tools       *[]string `json:"tools,omitempty"`
	Provider    *string   `json:"provider,omitempty" jsonschema:"empty string clears the pin"`
	Model       *string   `json:"model,omitempty" jsonschema:"empty string clears the pin"`
}

type AgentEditOutput struct {
	Agent ExtensionItemView `json:"agent"`
}

type AgentDeleteInput struct {
	Name  string `json:"name" jsonschema:"agent name to delete"`
	Scope string `json:"scope,omitempty" jsonschema:"'user' or 'project' when the name is ambiguous"`
}

type AgentDeleteOutput struct {
	Deleted bool `json:"deleted"`
}

type MarketplaceView struct {
	Name     string `json:"name"`
	Scope    string `json:"scope" jsonschema:"'user' or 'project'"`
	Source   string `json:"source"`
	Fetched  bool   `json:"fetched" jsonschema:"catalog available on this machine"`
	Writable bool   `json:"writable" jsonschema:"skill_publish can land plugins here"`
	Plugins  int    `json:"plugins"`
	Error    string `json:"error,omitempty"`
}

type MarketplaceListInput struct{}

type MarketplaceListOutput struct {
	Marketplaces []MarketplaceView `json:"marketplaces"`
}

type MarketplaceSearchInput struct {
	Query       string `json:"query" jsonschema:"case-insensitive substring; '*' lists every plugin"`
	Marketplace string `json:"marketplace,omitempty" jsonschema:"restrict to one marketplace"`
}

type MarketplacePluginView struct {
	Ref         string   `json:"ref" jsonschema:"name@marketplace — pass to plugin_install"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"`
	Category    string   `json:"category,omitempty"`
	Source      string   `json:"source,omitempty"`
	Skills      []string `json:"skills,omitempty" jsonschema:"skill paths the catalog lists"`
	Installed   bool     `json:"installed"`
}

type MarketplaceSearchOutput struct {
	Matches []MarketplacePluginView `json:"matches"`
}

type MarketplaceAddInput struct {
	Source string `json:"source" jsonschema:"owner/repo, git URL, directory, or marketplace.json URL"`
	Scope  string `json:"scope,omitempty" jsonschema:"'user' (default) or 'project'"`
}

type MarketplaceAddOutput struct {
	Marketplace MarketplaceView `json:"marketplace"`
}

type PluginInstallInput struct {
	Plugin string `json:"plugin" jsonschema:"name@marketplace"`
	Scope  string `json:"scope,omitempty" jsonschema:"'user' (default) or 'project'"`
}

type PluginInstallOutput struct {
	Plugin PluginView `json:"plugin"`
}

type PluginUninstallInput struct {
	Plugin string `json:"plugin" jsonschema:"name@marketplace"`
	Scope  string `json:"scope,omitempty" jsonschema:"'user' (default) or 'project'"`
}

type PluginUninstallOutput struct {
	Removed bool `json:"removed"`
}

type SkillPublishInput struct {
	Name        string `json:"name" jsonschema:"skill, agent, or workflow name"`
	Kind        string `json:"kind,omitempty" jsonschema:"'skill' (default), 'agent', or 'workflow'"`
	Marketplace string `json:"marketplace" jsonschema:"registered, writable marketplace name"`
	PluginName  string `json:"plugin_name,omitempty" jsonschema:"plugin to publish into (defaults to the item name)"`
	Description string `json:"description,omitempty" jsonschema:"plugin description shown in the catalog"`
	Version     string `json:"version,omitempty" jsonschema:"plugin version (default 1.0.0, or the existing one)"`
	Message     string `json:"message,omitempty" jsonschema:"git commit message"`
	NoPush      bool   `json:"no_push,omitempty" jsonschema:"commit without pushing (git-backed marketplaces push by default)"`
}

type SkillPublishOutput struct {
	PluginDir string `json:"plugin_dir"`
	Version   string `json:"version,omitempty"`
	Committed bool   `json:"committed"`
	Pushed    bool   `json:"pushed"`
	Note      string `json:"note,omitempty"`
}

type SkillPullInput struct {
	Name string `json:"name" jsonschema:"local skill, agent, or workflow name"`
	Kind string `json:"kind,omitempty" jsonschema:"'skill' (default), 'agent', or 'workflow'"`
}

type SkillPullOutput struct {
	Item ExtensionItemView `json:"item"`
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}
}

func textResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

// ExtensionTools returns the registry tools that manage skills, agents,
// plugins, and marketplaces.
func ExtensionTools(env *ToolEnv) []Tool {
	cwd := func() string { return env.Cwd }
	changed := func(what string) {
		engine.BumpSkillsGeneration()
		if env != nil && env.Emit != nil {
			env.Emit(engine.ExtensionsChangedEvent{BaseEvent: engine.BaseEvent{TabID: env.TabID}, What: what})
		}
	}
	approve := func(ctx context.Context, tool string, input map[string]any) error {
		if env == nil {
			return nil
		}
		if denied := env.ApprovalDenied(ctx, tool, input); denied != "" {
			return fmt.Errorf("%s", denied)
		}
		return nil
	}
	return []Tool{
		NativeBridgeTool("skill_list", SkillListToolDescription,
			func(_ context.Context, in SkillListInput) (*mcp.CallToolResult, SkillListOutput, error) {
				return SkillListCore(cwd(), in)
			}),
		NativeBridgeTool("skill_get", SkillGetToolDescription,
			func(_ context.Context, in SkillGetInput) (*mcp.CallToolResult, SkillGetOutput, error) {
				return SkillGetCore(cwd(), in)
			}),
		NativeBridgeTool("skill_create", SkillCreateToolDescription,
			func(_ context.Context, in SkillCreateInput) (*mcp.CallToolResult, SkillCreateOutput, error) {
				res, out, err := SkillCreateCore(cwd(), in)
				if err == nil && !res.IsError {
					changed("skill")
				}
				return res, out, err
			}),
		NativeBridgeTool("skill_edit", SkillEditToolDescription,
			func(_ context.Context, in SkillEditInput) (*mcp.CallToolResult, SkillEditOutput, error) {
				res, out, err := SkillEditCore(cwd(), in)
				if err == nil && !res.IsError {
					changed("skill")
				}
				return res, out, err
			}),
		NativeBridgeTool("skill_delete", SkillDeleteToolDescription,
			func(_ context.Context, in SkillDeleteInput) (*mcp.CallToolResult, SkillDeleteOutput, error) {
				res, out, err := SkillDeleteCore(cwd(), in)
				if err == nil && !res.IsError {
					changed("skill")
				}
				return res, out, err
			}),
		NativeBridgeTool("agent_create", AgentCreateToolDescription,
			func(_ context.Context, in AgentCreateInput) (*mcp.CallToolResult, AgentCreateOutput, error) {
				res, out, err := AgentCreateCore(cwd(), in)
				if err == nil && !res.IsError {
					changed("agent")
				}
				return res, out, err
			}),
		NativeBridgeTool("agent_edit", AgentEditToolDescription,
			func(_ context.Context, in AgentEditInput) (*mcp.CallToolResult, AgentEditOutput, error) {
				res, out, err := AgentEditCore(cwd(), in)
				if err == nil && !res.IsError {
					changed("agent")
				}
				return res, out, err
			}),
		NativeBridgeTool("agent_delete", AgentDeleteToolDescription,
			func(_ context.Context, in AgentDeleteInput) (*mcp.CallToolResult, AgentDeleteOutput, error) {
				res, out, err := AgentDeleteCore(cwd(), in)
				if err == nil && !res.IsError {
					changed("agent")
				}
				return res, out, err
			}),
		NativeBridgeTool("marketplace_list", MarketplaceListToolDescription,
			func(_ context.Context, in MarketplaceListInput) (*mcp.CallToolResult, MarketplaceListOutput, error) {
				return MarketplaceListCore(cwd(), in)
			}),
		NativeBridgeTool("marketplace_search", MarketplaceSearchToolDescription,
			func(_ context.Context, in MarketplaceSearchInput) (*mcp.CallToolResult, MarketplaceSearchOutput, error) {
				return MarketplaceSearchCore(cwd(), in)
			}),
		NativeBridgeTool("marketplace_add", MarketplaceAddToolDescription,
			func(ctx context.Context, in MarketplaceAddInput) (*mcp.CallToolResult, MarketplaceAddOutput, error) {
				if err := approve(ctx, "marketplace_add", map[string]any{"source": in.Source, "scope": in.Scope}); err != nil {
					return errorResult(err), MarketplaceAddOutput{}, nil
				}
				res, out, err := MarketplaceAddCore(ctx, cwd(), in)
				if err == nil && !res.IsError {
					changed("marketplace")
				}
				return res, out, err
			}),
		NativeBridgeTool("plugin_install", PluginInstallToolDescription,
			func(ctx context.Context, in PluginInstallInput) (*mcp.CallToolResult, PluginInstallOutput, error) {
				if err := approve(ctx, "plugin_install", map[string]any{"plugin": in.Plugin, "scope": in.Scope}); err != nil {
					return errorResult(err), PluginInstallOutput{}, nil
				}
				res, out, err := PluginInstallCore(ctx, cwd(), in)
				if err == nil && !res.IsError {
					changed("plugin")
				}
				return res, out, err
			}),
		NativeBridgeTool("plugin_uninstall", PluginUninstallToolDescription,
			func(_ context.Context, in PluginUninstallInput) (*mcp.CallToolResult, PluginUninstallOutput, error) {
				res, out, err := PluginUninstallCore(cwd(), in)
				if err == nil && !res.IsError {
					changed("plugin")
				}
				return res, out, err
			}),
		NativeBridgeTool("skill_publish", SkillPublishToolDescription,
			func(ctx context.Context, in SkillPublishInput) (*mcp.CallToolResult, SkillPublishOutput, error) {
				if err := approve(ctx, "skill_publish", map[string]any{"name": in.Name, "kind": in.Kind, "marketplace": in.Marketplace, "no_push": in.NoPush}); err != nil {
					return errorResult(err), SkillPublishOutput{}, nil
				}
				res, out, err := SkillPublishCore(ctx, cwd(), in)
				if err == nil && !res.IsError {
					changed("marketplace")
				}
				return res, out, err
			}),
		NativeBridgeTool("skill_pull", SkillPullToolDescription,
			func(ctx context.Context, in SkillPullInput) (*mcp.CallToolResult, SkillPullOutput, error) {
				if err := approve(ctx, "skill_pull", map[string]any{"name": in.Name, "kind": in.Kind}); err != nil {
					return errorResult(err), SkillPullOutput{}, nil
				}
				res, out, err := SkillPullCore(cwd(), in)
				if err == nil && !res.IsError {
					changed("skill")
				}
				return res, out, err
			}),
	}
}

// publishedView resolves the publication link and its sync state for a
// local item, or nil when it was never published.
func publishedView(cwd, kind, name, scope string) *PublishedView {
	pub, ok := plugin.FindPublication(cwd, kind, name, scope)
	if !ok {
		return nil
	}
	target, err := PublishTargetFor(cwd, kind, name)
	status := plugin.SyncMissing
	if err == nil {
		status = plugin.Status(cwd, pub, target.LocalHash)
	}
	return &PublishedView{Marketplace: pub.Marketplace, Plugin: pub.Ref().String(), Version: pub.Version, Status: status.String()}
}

func boolPtr(b bool) *bool { return &b }

func skillView(cwd string, s engine.Skill) ExtensionItemView {
	v := ExtensionItemView{
		Kind:           "skill",
		Name:           s.Name,
		Description:    s.Description,
		Origin:         s.Origin.String(),
		Scope:          string(s.Origin.Scope),
		Plugin:         s.Origin.Plugin,
		Path:           s.Path,
		UserInvocable:  boolPtr(s.UserInvocable),
		ModelInvocable: boolPtr(!s.DisableModelInvocation),
	}
	if s.UserInvocable {
		v.SlashCommand = "/" + s.Name
	}
	if s.Origin.Editable() {
		v.Published = publishedView(cwd, "skill", s.Name, string(s.Origin.Scope))
	}
	return v
}

func agentView(cwd string, d engine.SubagentDef) ExtensionItemView {
	v := ExtensionItemView{
		Kind:        "agent",
		Name:        d.Name,
		Description: d.Description,
		Origin:      d.Origin.String(),
		Scope:       string(d.Origin.Scope),
		Plugin:      d.Origin.Plugin,
		Path:        d.Source,
		Provider:    d.Provider,
		Model:       d.Model,
		Tools:       d.Tools,
	}
	if d.Origin.Editable() {
		v.Published = publishedView(cwd, "agent", d.Name, string(d.Origin.Scope))
	}
	return v
}

// WorkflowProviderWarnings lists the steps of w whose provider has no
// credentials configured, so the user can switch them to a model that
// does before running.
func WorkflowProviderWarnings(cfg config.Config, w workflow.Def, fallbackProvider string) []string {
	var warn []string
	check := func(s workflow.Step) {
		id := s.Provider
		if id == "" {
			id = fallbackProvider
		}
		if id == "" {
			return
		}
		if !providers.ProviderConfigured(cfg, id) {
			warn = append(warn, fmt.Sprintf("step %q: provider %q is not configured — change this step's model to a configured provider", s.Name, id))
		}
	}
	for _, s := range w.Steps {
		if s.IsLoop() {
			for _, inner := range s.Steps {
				check(inner)
			}
			continue
		}
		check(s)
	}
	return warn
}

func workflowView(cwd string, cfg config.Config, w workflow.Def) ExtensionItemView {
	v := ExtensionItemView{
		Kind:        "workflow",
		Name:        w.Name,
		Description: w.Description,
		Scope:       string(w.Scope),
		Plugin:      w.Plugin,
	}
	if w.Scope == workflow.ScopePlugin {
		v.Origin = "plugin " + w.Plugin
	} else {
		v.Origin = string(w.Scope)
	}
	for _, s := range w.Steps {
		if s.IsLoop() {
			v.Steps = append(v.Steps, fmt.Sprintf("%s (loop, %d inner steps)", s.Name, len(s.Steps)))
			continue
		}
		v.Steps = append(v.Steps, fmt.Sprintf("%s (%s/%s)", s.Name, s.Provider, s.Model))
	}
	v.Warnings = WorkflowProviderWarnings(cfg, w, cfg.Provider)
	if w.Scope != workflow.ScopePlugin {
		v.Published = publishedView(cwd, "workflow", w.Name, "")
	}
	return v
}

func pluginView(in plugin.Installed) PluginView {
	v := PluginView{Ref: in.Ref.String(), Description: in.Description(), Version: in.Version, Missing: in.Missing}
	for _, s := range in.Scopes {
		v.Scopes = append(v.Scopes, string(s))
	}
	c := in.Contents()
	for _, d := range c.SkillDirs {
		v.Skills = append(v.Skills, in.Ref.Plugin+":"+baseName(d))
	}
	for _, f := range c.CommandFiles {
		v.Skills = append(v.Skills, in.Ref.Plugin+":"+strings.TrimSuffix(baseName(f), ".md"))
	}
	for _, f := range c.AgentFiles {
		v.Agents = append(v.Agents, in.Ref.Plugin+":"+strings.TrimSuffix(baseName(f), ".md"))
	}
	for _, f := range c.WorkflowFiles {
		v.Workflows = append(v.Workflows, strings.TrimSuffix(baseName(f), ".json"))
	}
	return v
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func SkillListCore(cwd string, in SkillListInput) (*mcp.CallToolResult, SkillListOutput, error) {
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind != "" && kind != "skill" && kind != "agent" && kind != "workflow" {
		return errorResult(fmt.Errorf("kind must be 'skill', 'agent', or 'workflow'")), SkillListOutput{}, nil
	}
	cfg, _ := config.Load()
	out := SkillListOutput{Items: []ExtensionItemView{}}
	if kind == "" || kind == "skill" {
		for _, s := range engine.DiscoverSkills(cwd) {
			out.Items = append(out.Items, skillView(cwd, s))
		}
	}
	if kind == "" || kind == "agent" {
		for _, d := range engine.DiscoverSubagents(cwd) {
			out.Items = append(out.Items, agentView(cwd, d))
		}
	}
	if kind == "" || kind == "workflow" {
		for _, w := range workflow.ListAll(cwd) {
			out.Items = append(out.Items, workflowView(cwd, cfg, w))
		}
	}
	for _, p := range plugin.EnabledPlugins(cwd) {
		out.Plugins = append(out.Plugins, pluginView(p))
	}
	return textResult("listed %d items, %d plugins", len(out.Items), len(out.Plugins)), out, nil
}

func SkillGetCore(cwd string, in SkillGetInput) (*mcp.CallToolResult, SkillGetOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), SkillGetOutput{}, nil
	}
	switch strings.ToLower(strings.TrimSpace(in.Kind)) {
	case "", "skill":
		s, ok := engine.FindSkill(cwd, name)
		if !ok {
			return errorResult(fmt.Errorf("skill %q not found", name)), SkillGetOutput{}, nil
		}
		return textResult("skill %s (%s)", s.Name, s.Origin), SkillGetOutput{Item: skillView(cwd, s), Body: engine.SkillBody(s)}, nil
	case "agent":
		d, ok := engine.FindSubagent(cwd, name)
		if !ok {
			return errorResult(fmt.Errorf("agent %q not found", name)), SkillGetOutput{}, nil
		}
		return textResult("agent %s (%s)", d.Name, d.Origin), SkillGetOutput{Item: agentView(cwd, d), Body: d.Prompt}, nil
	}
	return errorResult(fmt.Errorf("kind must be 'skill' or 'agent'")), SkillGetOutput{}, nil
}

func SkillCreateCore(cwd string, in SkillCreateInput) (*mcp.CallToolResult, SkillCreateOutput, error) {
	scope, err := engine.NormalizeOriginScope(in.Scope)
	if err != nil {
		return errorResult(err), SkillCreateOutput{}, nil
	}
	s, err := engine.CreateSkill(cwd, scope, engine.SkillSpec{
		Name:                   strings.TrimSpace(in.Name),
		Description:            in.Description,
		Body:                   in.Body,
		UserInvocable:          in.UserInvocable,
		DisableModelInvocation: in.DisableModelInvocation,
	})
	if err != nil {
		return errorResult(err), SkillCreateOutput{}, nil
	}
	return textResult("created skill %s in %s scope at %s — /%s is registered", s.Name, scope, s.Path, s.Name), SkillCreateOutput{Skill: skillView(cwd, s)}, nil
}

func SkillEditCore(cwd string, in SkillEditInput) (*mcp.CallToolResult, SkillEditOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), SkillEditOutput{}, nil
	}
	scope, err := optionalOriginScope(in.Scope)
	if err != nil {
		return errorResult(err), SkillEditOutput{}, nil
	}
	s, err := engine.UpdateSkill(cwd, name, scope, engine.SkillPatch{
		Description:            in.Description,
		Body:                   in.Body,
		UserInvocable:          in.UserInvocable,
		DisableModelInvocation: in.DisableModelInvocation,
	})
	if err != nil {
		return errorResult(err), SkillEditOutput{}, nil
	}
	return textResult("updated skill %s at %s", s.Name, s.Path), SkillEditOutput{Skill: skillView(cwd, s)}, nil
}

func SkillDeleteCore(cwd string, in SkillDeleteInput) (*mcp.CallToolResult, SkillDeleteOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), SkillDeleteOutput{}, nil
	}
	scope, err := optionalOriginScope(in.Scope)
	if err != nil {
		return errorResult(err), SkillDeleteOutput{}, nil
	}
	if err := engine.DeleteSkill(cwd, name, scope); err != nil {
		return errorResult(err), SkillDeleteOutput{}, nil
	}
	return textResult("deleted skill %s", name), SkillDeleteOutput{Deleted: true}, nil
}

func optionalOriginScope(s string) (engine.OriginScope, error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	return engine.NormalizeOriginScope(s)
}

func AgentCreateCore(cwd string, in AgentCreateInput) (*mcp.CallToolResult, AgentCreateOutput, error) {
	scope, err := engine.NormalizeOriginScope(in.Scope)
	if err != nil {
		return errorResult(err), AgentCreateOutput{}, nil
	}
	d, err := engine.CreateAgent(cwd, scope, engine.AgentSpec{
		Name:        strings.TrimSpace(in.Name),
		Description: in.Description,
		Prompt:      in.Prompt,
		Provider:    in.Provider,
		Model:       in.Model,
		Tools:       in.Tools,
	})
	if err != nil {
		return errorResult(err), AgentCreateOutput{}, nil
	}
	return textResult("created agent %s in %s scope at %s", d.Name, scope, d.Source), AgentCreateOutput{Agent: agentView(cwd, d)}, nil
}

func AgentEditCore(cwd string, in AgentEditInput) (*mcp.CallToolResult, AgentEditOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), AgentEditOutput{}, nil
	}
	scope, err := optionalOriginScope(in.Scope)
	if err != nil {
		return errorResult(err), AgentEditOutput{}, nil
	}
	d, err := engine.UpdateAgent(cwd, name, scope, engine.AgentPatch{
		Description: in.Description,
		Prompt:      in.Prompt,
		Provider:    in.Provider,
		Model:       in.Model,
		Tools:       in.Tools,
	})
	if err != nil {
		return errorResult(err), AgentEditOutput{}, nil
	}
	return textResult("updated agent %s at %s", d.Name, d.Source), AgentEditOutput{Agent: agentView(cwd, d)}, nil
}

func AgentDeleteCore(cwd string, in AgentDeleteInput) (*mcp.CallToolResult, AgentDeleteOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), AgentDeleteOutput{}, nil
	}
	scope, err := optionalOriginScope(in.Scope)
	if err != nil {
		return errorResult(err), AgentDeleteOutput{}, nil
	}
	if err := engine.DeleteAgent(cwd, name, scope); err != nil {
		return errorResult(err), AgentDeleteOutput{}, nil
	}
	return textResult("deleted agent %s", name), AgentDeleteOutput{Deleted: true}, nil
}

func marketplaceView(m plugin.Marketplace) MarketplaceView {
	v := MarketplaceView{
		Name:     m.Name,
		Scope:    string(m.Scope),
		Source:   m.Source.Display(),
		Fetched:  m.Fetched(),
		Writable: m.Writable(),
		Error:    m.Err,
	}
	if m.Manifest != nil {
		v.Plugins = len(m.Manifest.Plugins)
	}
	return v
}

func MarketplaceListCore(cwd string, _ MarketplaceListInput) (*mcp.CallToolResult, MarketplaceListOutput, error) {
	out := MarketplaceListOutput{Marketplaces: []MarketplaceView{}}
	for _, m := range plugin.ListMarketplaces(cwd) {
		out.Marketplaces = append(out.Marketplaces, marketplaceView(m))
	}
	return textResult("listed %d marketplaces", len(out.Marketplaces)), out, nil
}

// MarketplaceSearch is the search behind marketplace_search and the
// browser's marketplace lens.
func MarketplaceSearch(cwd, query, onlyMarketplace string) []MarketplacePluginView {
	q := strings.ToLower(strings.TrimSpace(query))
	all := q == "" || q == "*"
	installed := map[string]bool{}
	for _, in := range plugin.EnabledPlugins(cwd) {
		installed[in.Ref.String()] = true
	}
	var out []MarketplacePluginView
	for _, m := range plugin.ListMarketplaces(cwd) {
		if onlyMarketplace != "" && m.Name != onlyMarketplace {
			continue
		}
		if m.Manifest == nil {
			continue
		}
		for _, e := range m.Manifest.Plugins {
			hay := strings.ToLower(strings.Join(append([]string{e.Name, e.Description, e.Category, strings.Join(e.Tags, " "), strings.Join(e.Keywords, " ")}, e.Skills...), " "))
			if !all && !strings.Contains(hay, q) {
				continue
			}
			ref := e.Name + "@" + m.Name
			out = append(out, MarketplacePluginView{
				Ref:         ref,
				Description: e.Description,
				Version:     e.Version,
				Category:    e.Category,
				Source:      e.Source.String(),
				Skills:      e.Skills,
				Installed:   installed[ref],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

func MarketplaceSearchCore(cwd string, in MarketplaceSearchInput) (*mcp.CallToolResult, MarketplaceSearchOutput, error) {
	if strings.TrimSpace(in.Query) == "" {
		return errorResult(fmt.Errorf("query is required ('*' lists every plugin)")), MarketplaceSearchOutput{}, nil
	}
	matches := MarketplaceSearch(cwd, in.Query, strings.TrimSpace(in.Marketplace))
	if matches == nil {
		matches = []MarketplacePluginView{}
	}
	if len(matches) == 0 {
		n := len(plugin.ListMarketplaces(cwd))
		if n == 0 {
			return textResult("no marketplaces are registered — add one with marketplace_add (for example anthropics/skills or anthropics/claude-plugins-official)"), MarketplaceSearchOutput{Matches: matches}, nil
		}
		return textResult("no plugins matched %q across %d marketplace(s)", in.Query, n), MarketplaceSearchOutput{Matches: matches}, nil
	}
	return textResult("%d plugin(s) matched", len(matches)), MarketplaceSearchOutput{Matches: matches}, nil
}

func MarketplaceAddCore(ctx context.Context, cwd string, in MarketplaceAddInput) (*mcp.CallToolResult, MarketplaceAddOutput, error) {
	scope, err := plugin.NormalizeScope(in.Scope)
	if err != nil {
		return errorResult(err), MarketplaceAddOutput{}, nil
	}
	m, err := plugin.AddMarketplace(ctx, cwd, in.Source, scope)
	if err != nil {
		return errorResult(err), MarketplaceAddOutput{}, nil
	}
	return textResult("registered marketplace %s (%d plugins) in %s scope", m.Name, len(m.Manifest.Plugins), scope), MarketplaceAddOutput{Marketplace: marketplaceView(m)}, nil
}

func PluginInstallCore(ctx context.Context, cwd string, in PluginInstallInput) (*mcp.CallToolResult, PluginInstallOutput, error) {
	ref, err := plugin.ParseRef(in.Plugin)
	if err != nil {
		return errorResult(err), PluginInstallOutput{}, nil
	}
	scope, err := plugin.NormalizeScope(in.Scope)
	if err != nil {
		return errorResult(err), PluginInstallOutput{}, nil
	}
	installed, err := plugin.InstallPlugin(ctx, cwd, ref, scope)
	if err != nil {
		return errorResult(err), PluginInstallOutput{}, nil
	}
	v := pluginView(installed)
	return textResult("installed %s in %s scope: %d skill(s), %d agent(s), %d workflow(s)", ref, scope, len(v.Skills), len(v.Agents), len(v.Workflows)), PluginInstallOutput{Plugin: v}, nil
}

func PluginUninstallCore(cwd string, in PluginUninstallInput) (*mcp.CallToolResult, PluginUninstallOutput, error) {
	ref, err := plugin.ParseRef(in.Plugin)
	if err != nil {
		return errorResult(err), PluginUninstallOutput{}, nil
	}
	scope, err := plugin.NormalizeScope(in.Scope)
	if err != nil {
		return errorResult(err), PluginUninstallOutput{}, nil
	}
	if err := plugin.UninstallPlugin(cwd, ref, scope); err != nil {
		return errorResult(err), PluginUninstallOutput{}, nil
	}
	return textResult("removed %s from %s scope", ref, scope), PluginUninstallOutput{Removed: true}, nil
}

// PublishTarget is a local skill, agent, or workflow resolved for
// publishing: the publish request plus what the publication link needs.
type PublishTarget struct {
	Req       plugin.PublishRequest
	Kind      string
	Name      string
	Scope     string
	LocalPath string
	LocalHash string
	File      string
	// prepare materializes files the request needs (a workflow export);
	// nil when the local files are used as they are.
	prepare func() (files []string, cleanup func(), err error)
}

// Prepare materializes anything the publish request needs and returns
// the cleanup to run after publishing.
func (t *PublishTarget) Prepare() (func(), error) {
	if t.prepare == nil {
		return func() {}, nil
	}
	files, cleanup, err := t.prepare()
	if err != nil {
		return nil, err
	}
	t.Req.WorkflowFiles = files
	return cleanup, nil
}

// PublishTargetFor resolves a local item. Shared by skill_publish,
// skill_pull, the sync status, and the browser.
func PublishTargetFor(cwd, kind, name string) (PublishTarget, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return PublishTarget{}, fmt.Errorf("name is required")
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "skill":
		s, err := engine.ResolveEditableSkill(cwd, name, "")
		if err != nil {
			return PublishTarget{}, err
		}
		if s.Command {
			return PublishTarget{}, fmt.Errorf("%q is a single-file command; convert it to a SKILL.md package first", name)
		}
		return PublishTarget{
			Req:       plugin.PublishRequest{PluginName: s.BareName, Description: s.Description, SkillDirs: []string{s.Dir}},
			Kind:      "skill",
			Name:      s.Name,
			Scope:     string(s.Origin.Scope),
			LocalPath: s.Dir,
			LocalHash: plugin.HashPath(s.Dir),
			File:      baseName(s.Dir),
		}, nil
	case "agent":
		d, err := engine.ResolveEditableAgent(cwd, name, "")
		if err != nil {
			return PublishTarget{}, err
		}
		return PublishTarget{
			Req:       plugin.PublishRequest{PluginName: d.BareName, Description: d.Description, AgentFiles: []string{d.Source}},
			Kind:      "agent",
			Name:      d.Name,
			Scope:     string(d.Origin.Scope),
			LocalPath: d.Source,
			LocalHash: plugin.HashPath(d.Source),
			File:      baseName(d.Source),
		}, nil
	case "workflow":
		var matches []workflow.Def
		for _, w := range workflow.ListAll(cwd) {
			if w.Name == name && w.Scope != workflow.ScopePlugin {
				matches = append(matches, w)
			}
		}
		if len(matches) == 0 {
			return PublishTarget{}, fmt.Errorf("workflow %q not found", name)
		}
		w := matches[0]
		data, err := workflow.ExportBytes(w)
		if err != nil {
			return PublishTarget{}, err
		}
		t := PublishTarget{
			Req:       plugin.PublishRequest{PluginName: workflow.FileName(w.Name), Description: w.Description},
			Kind:      "workflow",
			Name:      w.Name,
			Scope:     string(w.Scope),
			LocalHash: plugin.HashBytes(data),
			File:      workflow.FileName(w.Name) + ".json",
		}
		t.prepare = func() ([]string, func(), error) {
			path, err := workflow.ExportFile(w)
			if err != nil {
				return nil, nil, err
			}
			return []string{path}, func() { _ = os.RemoveAll(filepath.Dir(path)) }, nil
		}
		return t, nil
	}
	return PublishTarget{}, fmt.Errorf("kind must be 'skill', 'agent', or 'workflow'")
}

// PublishItem publishes a local item and records (or updates) its
// publication link. Shared by skill_publish and the browser.
func PublishItem(ctx context.Context, cwd string, m plugin.Marketplace, target PublishTarget, pluginName, description, version, message string, noPush bool) (plugin.PublishResult, plugin.Publication, error) {
	cleanup, err := target.Prepare()
	if err != nil {
		return plugin.PublishResult{}, plugin.Publication{}, err
	}
	defer cleanup()
	req := target.Req
	if p := strings.TrimSpace(pluginName); p != "" {
		req.PluginName = p
	} else if prev, ok := plugin.FindPublication(cwd, target.Kind, target.Name, target.Scope); ok && prev.Marketplace == m.Name {
		req.PluginName = prev.Plugin
	}
	if description != "" {
		req.Description = description
	}
	req.Version = version
	req.Message = message
	req.NoPush = noPush
	res, err := plugin.Publish(ctx, m, req)
	if err != nil {
		return res, plugin.Publication{}, err
	}
	pub := plugin.Publication{
		Kind:        target.Kind,
		Name:        target.Name,
		Scope:       target.Scope,
		Marketplace: m.Name,
		Plugin:      req.PluginName,
		File:        target.File,
		Version:     res.Version,
		Hash:        target.LocalHash,
		PublishedAt: plugin.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		Commit:      res.Commit,
	}
	if err := plugin.RecordPublication(cwd, pub); err != nil {
		return res, pub, err
	}
	return res, pub, nil
}

// PullItem replaces a local item with its published copy.
func PullItem(cwd, kind, name string) (plugin.Publication, error) {
	target, err := PublishTargetFor(cwd, kind, name)
	if err != nil {
		return plugin.Publication{}, err
	}
	pub, ok := plugin.FindPublication(cwd, target.Kind, target.Name, target.Scope)
	if !ok {
		return plugin.Publication{}, fmt.Errorf("%s %q was not published from here", target.Kind, target.Name)
	}
	if target.Kind == "workflow" {
		return pullWorkflow(cwd, pub)
	}
	pub, err = plugin.Pull(cwd, pub, target.LocalPath)
	if err != nil {
		return pub, err
	}
	engine.BumpSkillsGeneration()
	return pub, nil
}

// pullWorkflow reads the published JSON back into the workflow store.
func pullWorkflow(cwd string, pub plugin.Publication) (plugin.Publication, error) {
	m, ok := plugin.FindMarketplace(cwd, pub.Marketplace)
	if !ok || m.Dir == "" {
		return pub, fmt.Errorf("marketplace %q is not fetched", pub.Marketplace)
	}
	data, err := os.ReadFile(plugin.PublishedCopyPath(m, pub))
	if err != nil {
		return pub, fmt.Errorf("published copy missing: %w", err)
	}
	var incoming workflow.Def
	if err := json.Unmarshal(data, &incoming); err != nil {
		return pub, err
	}
	err = workflow.MutateWorkflows(cwd, func(items []workflow.Def) ([]workflow.Def, error) {
		for i := range items {
			if items[i].Name == pub.Name && string(items[i].Scope) == pub.Scope {
				incoming.Name = items[i].Name
				incoming.Scope = items[i].Scope
				items[i] = incoming
				return items, nil
			}
		}
		return nil, fmt.Errorf("workflow %q not found in %s scope", pub.Name, pub.Scope)
	})
	if err != nil {
		return pub, err
	}
	target, err := PublishTargetFor(cwd, "workflow", pub.Name)
	if err != nil {
		return pub, err
	}
	pub.Hash = target.LocalHash
	return pub, plugin.RecordPublication(cwd, pub)
}

func SkillPublishCore(ctx context.Context, cwd string, in SkillPublishInput) (*mcp.CallToolResult, SkillPublishOutput, error) {
	target, err := PublishTargetFor(cwd, in.Kind, in.Name)
	if err != nil {
		return errorResult(err), SkillPublishOutput{}, nil
	}
	m, ok := plugin.FindMarketplace(cwd, strings.TrimSpace(in.Marketplace))
	if !ok {
		return errorResult(fmt.Errorf("marketplace %q is not registered", in.Marketplace)), SkillPublishOutput{}, nil
	}
	res, pub, err := PublishItem(ctx, cwd, m, target, in.PluginName, in.Description, in.Version, in.Message, in.NoPush)
	if err != nil {
		return errorResult(err), SkillPublishOutput{}, nil
	}
	out := SkillPublishOutput{PluginDir: res.PluginDir, Version: res.Version, Committed: res.Committed, Pushed: res.Pushed, Note: res.Note}
	return textResult("published %s as plugin %s v%s into %s (%s) — the local copy stays the source of truth; publish again to update", in.Name, pub.Plugin, res.Version, m.Name, publishStatus(res)), out, nil
}

func SkillPullCore(cwd string, in SkillPullInput) (*mcp.CallToolResult, SkillPullOutput, error) {
	pub, err := PullItem(cwd, in.Kind, in.Name)
	if err != nil {
		return errorResult(err), SkillPullOutput{}, nil
	}
	var item ExtensionItemView
	switch pub.Kind {
	case "skill":
		if s, ok := engine.FindSkill(cwd, pub.Name); ok {
			item = skillView(cwd, s)
		}
	case "agent":
		if d, ok := engine.FindSubagent(cwd, pub.Name); ok {
			item = agentView(cwd, d)
		}
	case "workflow":
		cfg, _ := config.Load()
		for _, w := range workflow.ListAll(cwd) {
			if w.Name == pub.Name && string(w.Scope) == pub.Scope {
				item = workflowView(cwd, cfg, w)
			}
		}
	}
	return textResult("pulled %s %s from %s", pub.Kind, pub.Name, pub.Ref()), SkillPullOutput{Item: item}, nil
}

func publishStatus(res plugin.PublishResult) string {
	switch {
	case res.Pushed:
		return "committed and pushed"
	case res.Committed:
		return "committed"
	case res.Note != "":
		return res.Note
	}
	return "written"
}
