package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/plugin"
	"github.com/Cidan/ask/pkg/workflow"
)

func extensionToolByName(t *testing.T, env *ToolEnv, name string) Tool {
	t.Helper()
	for _, tool := range ExtensionTools(env) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("extension tool %q missing", name)
	return nil
}

func writeFixtureMarketplace(t *testing.T, dir string) {
	t.Helper()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude-plugin/marketplace.json", `{"name":"mkt","owner":{"name":"t"},"plugins":[
  {"name":"tools","source":"./plugins/tools","description":"Spreadsheet and review tooling","version":"1.0.0","category":"productivity"},
  {"name":"other","source":"./plugins/other","description":"Unrelated"}]}`)
	write("plugins/tools/skills/xlsx/SKILL.md", "---\nname: xlsx\ndescription: Work with spreadsheets\n---\nUse openpyxl.\n")
	write("plugins/tools/agents/reviewer.md", "---\nname: reviewer\ndescription: Reviews code\n---\nReview.\n")
	write("plugins/tools/workflows/release.json", `{"name":"release","description":"Cut a release","steps":[{"name":"plan","provider":"nosuch","prompt":"p"}]}`)
	write("plugins/other/skills/misc/SKILL.md", "---\nname: misc\ndescription: Misc\n---\nx\n")
}

// decodeBridge unpacks the BridgeResult a NativeBridgeTool returns
// (RunToolWithJSON hands it back as JSON in Content) into out.
func decodeBridge(t *testing.T, resp ToolResponse, out any) {
	t.Helper()
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp.Content), &wrapper); err != nil {
		t.Fatalf("bridge result is not JSON: %v\n%s", err, resp.Content)
	}
	if err := json.Unmarshal(wrapper.Data, out); err != nil {
		t.Fatalf("bridge data: %v\n%s", err, wrapper.Data)
	}
}

func extensionEvents(events *[]engine.EngineEvent) []string {
	var out []string
	for _, ev := range *events {
		if e, ok := ev.(engine.ExtensionsChangedEvent); ok {
			out = append(out, e.What)
		}
	}
	return out
}

func TestExtensionTools_CoverageAndRegistryPlacement(t *testing.T) {
	env, _ := newTestToolEnv(t)
	want := []string{
		"skill_list", "skill_get", "skill_create", "skill_edit", "skill_delete",
		"agent_create", "agent_edit", "agent_delete",
		"marketplace_list", "marketplace_search", "marketplace_add",
		"plugin_install", "plugin_uninstall", "skill_publish", "skill_pull",
	}
	got := map[string]bool{}
	for _, tool := range ExtensionTools(env) {
		got[tool.Name()] = true
		if IsCoreTool(tool.Name()) {
			t.Errorf("%s must not be a core tool", tool.Name())
		}
		info := ExtractToolInfo(tool)
		if _, ok := info.Parameters["description"]; !ok {
			t.Errorf("%s must carry the description phrase", tool.Name())
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing %s", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tool count %d want %d", len(got), len(want))
	}
	coreNames := map[string]bool{}
	for _, tool := range CoreTools(env, func() []Tool { return nil }, false) {
		coreNames[tool.Name()] = true
	}
	for _, name := range want {
		if coreNames[name] {
			t.Errorf("%s must stay off the wire", name)
		}
	}
}

func TestSkillTools_CRUDAndEvents(t *testing.T) {
	env, events := newTestToolEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := testAgentCtx()

	create := extensionToolByName(t, env, "skill_create")
	resp, err := RunToolWithJSON(ctx, create, `{"name":"release-notes","description":"Write release notes from git history","body":"1. git log\n2. summarise"}`)
	if err != nil || resp.IsError {
		t.Fatalf("create: %+v %v", resp, err)
	}
	if !strings.Contains(resp.Content, "/release-notes is registered") {
		t.Errorf("create content: %s", resp.Content)
	}
	path := filepath.Join(env.Cwd, ".ask", "skills", "release-notes", "SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("project scope default: %v", err)
	}
	if got := extensionEvents(events); len(got) != 1 || got[0] != "skill" {
		t.Fatalf("create must emit one extensions event: %v", got)
	}
	resp, _ = RunToolWithJSON(ctx, create, `{"name":"release-notes","description":"d","body":"b"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "already exists") {
		t.Errorf("duplicate create: %+v", resp)
	}
	resp, _ = RunToolWithJSON(ctx, create, `{"name":"Nope","description":"d","body":"b"}`)
	if !resp.IsError {
		t.Errorf("bad name: %+v", resp)
	}
	resp, err = RunToolWithJSON(ctx, create, `{"name":"personal","description":"user scope","body":"b","scope":"user","user_invocable":false}`)
	if err != nil || resp.IsError {
		t.Fatalf("user-scope create: %+v %v", resp, err)
	}

	list := extensionToolByName(t, env, "skill_list")
	resp, err = RunToolWithJSON(ctx, list, `{"kind":"skill"}`)
	if err != nil || resp.IsError {
		t.Fatalf("list: %+v %v", resp, err)
	}
	var out SkillListOutput
	decodeBridge(t, resp, &out)
	if len(out.Items) != 2 {
		t.Fatalf("list items: %+v", out.Items)
	}
	byName := map[string]ExtensionItemView{}
	for _, it := range out.Items {
		byName[it.Name] = it
	}
	if it := byName["release-notes"]; it.Scope != "project" || it.SlashCommand != "/release-notes" || it.Kind != "skill" {
		t.Errorf("list view: %+v", it)
	}
	if it := byName["personal"]; it.Scope != "user" || it.SlashCommand != "" {
		t.Errorf("user-invocable:false hides the slash command: %+v", it)
	}

	get := extensionToolByName(t, env, "skill_get")
	resp, err = RunToolWithJSON(ctx, get, `{"name":"release-notes"}`)
	if err != nil || resp.IsError {
		t.Fatalf("get: %+v %v", resp, err)
	}
	var got SkillGetOutput
	decodeBridge(t, resp, &got)
	if !strings.Contains(got.Body, "2. summarise") || got.Item.Path != path {
		t.Errorf("get body: %+v", got)
	}

	edit := extensionToolByName(t, env, "skill_edit")
	resp, err = RunToolWithJSON(ctx, edit, `{"name":"release-notes","body":"new body","disable_model_invocation":true}`)
	if err != nil || resp.IsError {
		t.Fatalf("edit: %+v %v", resp, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "new body") || !strings.Contains(string(data), "disable-model-invocation: true") {
		t.Fatalf("edit on disk:\n%s", data)
	}

	del := extensionToolByName(t, env, "skill_delete")
	resp, err = RunToolWithJSON(ctx, del, `{"name":"release-notes"}`)
	if err != nil || resp.IsError {
		t.Fatalf("delete: %+v %v", resp, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("package must be removed")
	}
	if got := extensionEvents(events); len(got) != 4 {
		t.Fatalf("create+create+edit+delete events: %v", got)
	}
}

func TestAgentTools_CRUD(t *testing.T) {
	env, _ := newTestToolEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := testAgentCtx()
	create := extensionToolByName(t, env, "agent_create")
	resp, err := RunToolWithJSON(ctx, create, `{"name":"critic","description":"Critiques plans","prompt":"Be harsh.","tools":["read","grep"],"provider":"vertex"}`)
	if err != nil || resp.IsError {
		t.Fatalf("create: %+v %v", resp, err)
	}
	path := filepath.Join(env.Cwd, ".ask", "agents", "critic.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	get := extensionToolByName(t, env, "skill_get")
	resp, err = RunToolWithJSON(ctx, get, `{"name":"critic","kind":"agent"}`)
	if err != nil || resp.IsError {
		t.Fatalf("get agent: %+v %v", resp, err)
	}
	var got SkillGetOutput
	decodeBridge(t, resp, &got)
	if got.Item.Kind != "agent" || got.Item.Provider != "vertex" || got.Body != "Be harsh." || len(got.Item.Tools) != 2 {
		t.Errorf("agent view: %+v", got)
	}
	edit := extensionToolByName(t, env, "agent_edit")
	resp, err = RunToolWithJSON(ctx, edit, `{"name":"critic","provider":"","prompt":"Be kind."}`)
	if err != nil || resp.IsError {
		t.Fatalf("edit: %+v %v", resp, err)
	}
	d, ok := engine.FindSubagent(env.Cwd, "critic")
	if !ok || d.Provider != "" || d.Prompt != "Be kind." {
		t.Fatalf("after edit: %v %+v", ok, d)
	}
	del := extensionToolByName(t, env, "agent_delete")
	if resp, err := RunToolWithJSON(ctx, del, `{"name":"critic"}`); err != nil || resp.IsError {
		t.Fatalf("delete: %+v %v", resp, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("agent file must be removed")
	}
}

func TestMarketplaceAndPluginTools(t *testing.T) {
	env, events := newTestToolEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	mkt := filepath.Join(home, "fixture-mkt")
	writeFixtureMarketplace(t, mkt)
	ctx := testAgentCtx()

	search := extensionToolByName(t, env, "marketplace_search")
	resp, err := RunToolWithJSON(ctx, search, `{"query":"spreadsheet"}`)
	if err != nil || resp.IsError || !strings.Contains(resp.Content, "no marketplaces are registered") {
		t.Fatalf("search before add: %+v %v", resp, err)
	}

	add := extensionToolByName(t, env, "marketplace_add")
	resp, err = RunToolWithJSON(ctx, add, `{"source":"`+mkt+`"}`)
	if err != nil || resp.IsError {
		t.Fatalf("add: %+v %v", resp, err)
	}
	var added MarketplaceAddOutput
	decodeBridge(t, resp, &added)
	if added.Marketplace.Name != "mkt" || added.Marketplace.Plugins != 2 || !added.Marketplace.Writable || added.Marketplace.Scope != "user" {
		t.Fatalf("added view: %+v", added)
	}

	resp, err = RunToolWithJSON(ctx, search, `{"query":"spreadsheet"}`)
	if err != nil || resp.IsError {
		t.Fatalf("search: %+v %v", resp, err)
	}
	var found MarketplaceSearchOutput
	decodeBridge(t, resp, &found)
	if len(found.Matches) != 1 || found.Matches[0].Ref != "tools@mkt" || found.Matches[0].Installed {
		t.Fatalf("search matches: %+v", found.Matches)
	}
	resp, _ = RunToolWithJSON(ctx, search, `{"query":"*"}`)
	decodeBridge(t, resp, &found)
	if len(found.Matches) != 2 {
		t.Fatalf("wildcard: %+v", found.Matches)
	}
	resp, _ = RunToolWithJSON(ctx, search, `{"query":"zzz"}`)
	if resp.IsError || !strings.Contains(resp.Content, "no plugins matched") {
		t.Errorf("no-match notice: %+v", resp)
	}

	install := extensionToolByName(t, env, "plugin_install")
	resp, err = RunToolWithJSON(ctx, install, `{"plugin":"tools@mkt","scope":"project"}`)
	if err != nil || resp.IsError {
		t.Fatalf("install: %+v %v", resp, err)
	}
	var inst PluginInstallOutput
	decodeBridge(t, resp, &inst)
	if inst.Plugin.Ref != "tools@mkt" || len(inst.Plugin.Skills) != 1 || inst.Plugin.Skills[0] != "tools:xlsx" || len(inst.Plugin.Agents) != 1 || len(inst.Plugin.Workflows) != 1 {
		t.Fatalf("install view: %+v", inst.Plugin)
	}
	if pf := plugin.ReadProjectFile(env.Cwd); !pf.Enabled["tools@mkt"] {
		t.Fatalf("project scope install writes .ask/plugins.json: %+v", pf)
	}
	if _, ok := engine.FindSkill(env.Cwd, "tools:xlsx"); !ok {
		t.Fatal("installed plugin skill must be discoverable")
	}
	if _, ok := engine.FindSubagent(env.Cwd, "tools:reviewer"); !ok {
		t.Fatal("installed plugin agent must be discoverable")
	}

	list := extensionToolByName(t, env, "skill_list")
	resp, _ = RunToolWithJSON(ctx, list, `{}`)
	var all SkillListOutput
	decodeBridge(t, resp, &all)
	var wf *ExtensionItemView
	for i := range all.Items {
		if all.Items[i].Kind == "workflow" && all.Items[i].Name == "release" {
			wf = &all.Items[i]
		}
	}
	if wf == nil || wf.Scope != "plugin" || wf.Plugin != "tools@mkt" || len(wf.Warnings) != 1 || !strings.Contains(wf.Warnings[0], `provider "nosuch" is not configured`) {
		t.Fatalf("plugin workflow with provider warning: %+v", wf)
	}
	if len(all.Plugins) != 1 || all.Plugins[0].Scopes[0] != "project" {
		t.Fatalf("plugins in list: %+v", all.Plugins)
	}

	resp, _ = RunToolWithJSON(ctx, search, `{"query":"tools"}`)
	decodeBridge(t, resp, &found)
	if len(found.Matches) != 1 || !found.Matches[0].Installed {
		t.Fatalf("installed flag after install: %+v", found.Matches)
	}

	mlist := extensionToolByName(t, env, "marketplace_list")
	resp, _ = RunToolWithJSON(ctx, mlist, `{}`)
	var ml MarketplaceListOutput
	decodeBridge(t, resp, &ml)
	if len(ml.Marketplaces) != 1 || !ml.Marketplaces[0].Fetched {
		t.Fatalf("marketplace list: %+v", ml)
	}

	uninstall := extensionToolByName(t, env, "plugin_uninstall")
	resp, err = RunToolWithJSON(ctx, uninstall, `{"plugin":"tools@mkt","scope":"project"}`)
	if err != nil || resp.IsError {
		t.Fatalf("uninstall: %+v %v", resp, err)
	}
	if _, ok := engine.FindSkill(env.Cwd, "tools:xlsx"); ok {
		t.Fatal("uninstalled plugin skill must disappear")
	}
	resp, _ = RunToolWithJSON(ctx, install, `{"plugin":"ghost@mkt"}`)
	if !resp.IsError {
		t.Errorf("unknown plugin: %+v", resp)
	}
	if got := extensionEvents(events); len(got) != 3 || got[0] != "marketplace" || got[1] != "plugin" || got[2] != "plugin" {
		t.Fatalf("events: %v", got)
	}
}

func TestExtensionTools_ApprovalGate(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.SkipPermissions = false
	var asked []string
	env.Approve = func(_ context.Context, toolName string, _ map[string]any) (bool, error) {
		asked = append(asked, toolName)
		return false, nil
	}
	ctx := testAgentCtx()
	for _, c := range []struct{ tool, input string }{
		{"marketplace_add", `{"source":"owner/repo"}`},
		{"plugin_install", `{"plugin":"x@y"}`},
		{"skill_publish", `{"name":"x","marketplace":"m"}`},
		{"skill_pull", `{"name":"x"}`},
	} {
		resp, err := RunToolWithJSON(ctx, extensionToolByName(t, env, c.tool), c.input)
		if err != nil || !resp.IsError || !strings.Contains(resp.Content, "denied") {
			t.Errorf("%s must be refused on denial: %+v %v", c.tool, resp, err)
		}
	}
	if len(asked) != 4 {
		t.Fatalf("every gated tool asks once: %v", asked)
	}
}

func TestSkillPublishTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	mkt := filepath.Join(home, "fixture-mkt")
	writeFixtureMarketplace(t, mkt)
	ctx := testAgentCtx()
	if _, err := plugin.AddMarketplace(context.Background(), env.Cwd, mkt, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateSkill(env.Cwd, engine.OriginProject, engine.SkillSpec{Name: "deploy", Description: "Deploys", Body: "ship"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateAgent(env.Cwd, engine.OriginUser, engine.AgentSpec{Name: "critic", Description: "Critiques", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := workflow.MutateWorkflows(env.Cwd, func(items []workflow.Def) ([]workflow.Def, error) {
		return append(items, workflow.Def{Name: "ship it", Scope: workflow.ScopeRepo, Steps: []workflow.Step{{Name: "a", Provider: "vertex", Prompt: "p"}}}), nil
	}); err != nil {
		t.Fatal(err)
	}

	publish := extensionToolByName(t, env, "skill_publish")
	resp, err := RunToolWithJSON(ctx, publish, `{"name":"deploy","marketplace":"mkt","description":"Deploy plugin"}`)
	if err != nil || resp.IsError {
		t.Fatalf("publish skill: %+v %v", resp, err)
	}
	if _, err := os.Stat(filepath.Join(mkt, "plugins", "deploy", "skills", "deploy", "SKILL.md")); err != nil {
		t.Fatal("skill must land in the marketplace")
	}
	resp, err = RunToolWithJSON(ctx, publish, `{"name":"critic","kind":"agent","marketplace":"mkt","plugin_name":"review-kit"}`)
	if err != nil || resp.IsError {
		t.Fatalf("publish agent: %+v %v", resp, err)
	}
	if _, err := os.Stat(filepath.Join(mkt, "plugins", "review-kit", "agents", "critic.md")); err != nil {
		t.Fatal("agent must land under plugin_name")
	}
	resp, err = RunToolWithJSON(ctx, publish, `{"name":"ship it","kind":"workflow","marketplace":"mkt"}`)
	if err != nil || resp.IsError {
		t.Fatalf("publish workflow: %+v %v", resp, err)
	}
	if _, err := os.Stat(filepath.Join(mkt, "plugins", "ship-it", "workflows", "ship-it.json")); err != nil {
		t.Fatal("workflow must land as a JSON file")
	}
	m, _ := plugin.ReadMarketplaceManifest(mkt)
	if len(m.Plugins) != 5 {
		t.Fatalf("catalog must list the three new plugins: %d", len(m.Plugins))
	}
	resp, _ = RunToolWithJSON(ctx, publish, `{"name":"deploy","marketplace":"nope"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "not registered") {
		t.Errorf("unknown marketplace: %+v", resp)
	}
	resp, _ = RunToolWithJSON(ctx, publish, `{"name":"ghost","marketplace":"mkt"}`)
	if !resp.IsError {
		t.Errorf("unknown skill: %+v", resp)
	}
}

func TestWorkflowProviderWarnings(t *testing.T) {
	cfg := config.Config{}
	w := workflow.Def{Name: "w", Steps: []workflow.Step{
		{Name: "a", Provider: "vertex"},
		{Name: "loop", Kind: "loop", Steps: []workflow.Step{{Name: "inner", Provider: "openrouter"}}},
		{Name: "b"},
	}}
	warn := WorkflowProviderWarnings(cfg, w, "")
	if len(warn) != 2 || !strings.Contains(warn[0], `step "a"`) || !strings.Contains(warn[1], `step "inner"`) {
		t.Fatalf("unconfigured providers: %v", warn)
	}
	cfg.SetProviderConfig("vertex", config.ProviderConfig{}.WithField("project", "proj"))
	cfg.SetProviderConfig("openrouter", config.ProviderConfig{}.WithField("apiKey", "k"))
	if warn := WorkflowProviderWarnings(cfg, w, "vertex"); len(warn) != 0 {
		t.Fatalf("configured providers: %v", warn)
	}
	if warn := WorkflowProviderWarnings(config.Config{}, w, "vertex"); len(warn) != 3 {
		t.Fatalf("fallback provider applies to unpinned steps: %v", warn)
	}
}

func TestSkillPublish_LinksAndPull(t *testing.T) {
	env, _ := newTestToolEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	mkt := filepath.Join(home, "fixture-mkt")
	writeFixtureMarketplace(t, mkt)
	ctx := testAgentCtx()
	if _, err := plugin.AddMarketplace(context.Background(), env.Cwd, mkt, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateSkill(env.Cwd, engine.OriginProject, engine.SkillSpec{Name: "deploy", Description: "Deploys", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := workflow.MutateWorkflows(env.Cwd, func(items []workflow.Def) ([]workflow.Def, error) {
		return append(items, workflow.Def{Name: "ship", Scope: workflow.ScopeRepo, Steps: []workflow.Step{{Name: "a", Provider: "vertex", Prompt: "p"}}}), nil
	}); err != nil {
		t.Fatal(err)
	}
	tempBefore := workflowExportDirs(t)
	publish := extensionToolByName(t, env, "skill_publish")
	resp, err := RunToolWithJSON(ctx, publish, `{"name":"deploy","marketplace":"mkt"}`)
	if err != nil || resp.IsError {
		t.Fatalf("publish: %+v %v", resp, err)
	}
	var out SkillPublishOutput
	decodeBridge(t, resp, &out)
	if out.Version != "1.0.0" {
		t.Fatalf("first publish: %+v", out)
	}
	list := extensionToolByName(t, env, "skill_list")
	resp, _ = RunToolWithJSON(ctx, list, `{"kind":"skill"}`)
	var items SkillListOutput
	decodeBridge(t, resp, &items)
	if len(items.Items) != 1 || items.Items[0].Published == nil || items.Items[0].Published.Plugin != "deploy@mkt" || items.Items[0].Published.Status != "in sync" {
		t.Fatalf("skill_list must report the link: %+v", items.Items)
	}

	body := "v2"
	if _, err := engine.UpdateSkill(env.Cwd, "deploy", "", engine.SkillPatch{Body: &body}); err != nil {
		t.Fatal(err)
	}
	resp, _ = RunToolWithJSON(ctx, list, `{"kind":"skill"}`)
	decodeBridge(t, resp, &items)
	if items.Items[0].Published.Status != "local changes" {
		t.Fatalf("after a local edit: %+v", items.Items[0].Published)
	}
	resp, err = RunToolWithJSON(ctx, publish, `{"name":"deploy","marketplace":"mkt"}`)
	if err != nil || resp.IsError {
		t.Fatalf("update publish: %+v %v", resp, err)
	}
	decodeBridge(t, resp, &out)
	if out.Version != "1.0.1" {
		t.Fatalf("update bumps the version: %+v", out)
	}

	remote := filepath.Join(mkt, "plugins", "deploy", "skills", "deploy", "SKILL.md")
	if err := os.WriteFile(remote, []byte("---\nname: deploy\ndescription: Deploys\n---\nv3 remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, _ = RunToolWithJSON(ctx, list, `{"kind":"skill"}`)
	decodeBridge(t, resp, &items)
	if items.Items[0].Published.Status != "marketplace newer" {
		t.Fatalf("after a marketplace edit: %+v", items.Items[0].Published)
	}
	pull := extensionToolByName(t, env, "skill_pull")
	resp, err = RunToolWithJSON(ctx, pull, `{"name":"deploy"}`)
	if err != nil || resp.IsError {
		t.Fatalf("pull: %+v %v", resp, err)
	}
	if s, _ := engine.FindSkill(env.Cwd, "deploy"); !strings.Contains(engine.SkillBody(s), "v3 remote") {
		t.Fatal("pull must replace the local copy")
	}
	resp, _ = RunToolWithJSON(ctx, pull, `{"name":"ghost"}`)
	if !resp.IsError {
		t.Error("pulling an unknown skill must error")
	}

	// Workflows: publish links too, status checks and publishes leave no
	// exports behind in the temp dir.
	resp, err = RunToolWithJSON(ctx, publish, `{"name":"ship","kind":"workflow","marketplace":"mkt"}`)
	if err != nil || resp.IsError {
		t.Fatalf("publish workflow: %+v %v", resp, err)
	}
	resp, _ = RunToolWithJSON(ctx, list, `{"kind":"workflow"}`)
	decodeBridge(t, resp, &items)
	if len(items.Items) != 1 || items.Items[0].Published == nil || items.Items[0].Published.Status != "in sync" {
		t.Fatalf("workflow link: %+v", items.Items)
	}
	for name := range workflowExportDirs(t) {
		if !tempBefore[name] {
			t.Fatalf("workflow export leaked: %s", name)
		}
	}
}

func workflowExportDirs(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, _ := os.ReadDir(os.TempDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ask-workflow-") {
			out[e.Name()] = true
		}
	}
	return out
}
