package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/plugin"
)

// installFixturePlugin registers a directory marketplace holding one
// plugin ("tools@mkt": a skill, a command, an agent, a workflow) and
// installs it at user scope.
func installFixturePlugin(t *testing.T, home, cwd string) {
	t.Helper()
	mkt := filepath.Join(home, "fixture-mkt")
	write := func(rel, content string) {
		p := filepath.Join(mkt, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude-plugin/marketplace.json", `{"name":"mkt","owner":{"name":"t"},"plugins":[{"name":"tools","source":"./plugins/tools","version":"1.0.0"}]}`)
	write("plugins/tools/.claude-plugin/plugin.json", `{"name":"tools","description":"Dev tools"}`)
	write("plugins/tools/skills/hello/SKILL.md", "---\nname: hello\ndescription: Say hello\n---\nSay hello.\n")
	write("plugins/tools/commands/ship.md", "---\ndescription: Ship it\n---\nShip $ARGUMENTS now.\n")
	write("plugins/tools/commands/bare.md", "Just do the thing.\nSecond line.\n")
	write("plugins/tools/agents/reviewer.md", "---\nname: reviewer\ndescription: Reviews code\nprovider: vertex\n---\nYou review.\n")
	write("plugins/tools/workflows/release.json", `{"name":"release","steps":[{"name":"plan","provider":"vertex","prompt":"p"}]}`)
	ctx := context.Background()
	if _, err := plugin.AddMarketplace(ctx, cwd, mkt, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.InstallPlugin(ctx, cwd, plugin.Ref{Plugin: "tools", Marketplace: "mkt"}, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}
	BumpSkillsGeneration()
}

func TestDiscoverSkills_PluginOriginsAndCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	writeTestSkill(t, filepath.Join(cwd, ".ask", "skills"), "deploy", "", "project deploy")
	writeTestSkill(t, filepath.Join(home, ".config", "ask", "skills"), "hello", "", "a user skill also called hello")
	installFixturePlugin(t, home, cwd)

	skills := DiscoverSkills(cwd)
	byName := map[string]Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}
	want := []string{"deploy", "hello", "tools:hello", "tools:ship", "tools:bare"}
	for _, n := range want {
		if _, ok := byName[n]; !ok {
			t.Errorf("missing %s in %v", n, byName)
		}
	}
	if len(skills) != len(want) {
		t.Fatalf("want %d skills, got %d: %v", len(want), len(skills), byName)
	}
	if s := byName["deploy"]; s.Origin.Scope != OriginProject || s.Origin.Plugin != "" || s.BareName != "deploy" {
		t.Errorf("project origin: %+v", s.Origin)
	}
	if s := byName["hello"]; s.Origin.Scope != OriginUser {
		t.Errorf("user origin: %+v", s.Origin)
	}
	ph := byName["tools:hello"]
	if ph.Origin.Scope != OriginPlugin || ph.Origin.Plugin != "tools@mkt" || ph.BareName != "hello" || ph.Command {
		t.Errorf("plugin skill: %+v", ph)
	}
	if ph.Frontmatter == nil || ph.Frontmatter.Name != "tools:hello" {
		t.Errorf("ADK frontmatter must carry the namespaced name: %+v", ph.Frontmatter)
	}
	if ph.Origin.Editable() {
		t.Error("plugin skills are not editable")
	}
	ship := byName["tools:ship"]
	if !ship.Command || ship.Description != "Ship it" || !ship.UserInvocable {
		t.Errorf("command file skill: %+v", ship)
	}
	if bare := byName["tools:bare"]; !bare.Command || bare.Description != "Just do the thing." {
		t.Errorf("frontmatter-less command gets its first line as description: %+v", bare)
	}
	if !strings.Contains(SkillsPromptBlock(skills), "<name>tools:ship</name>") {
		t.Error("plugin skills must be listed in the trigger block")
	}

	msg, ok := ExpandSkillInvocation(cwd, "/tools:ship v2.0 to prod")
	if !ok || !strings.Contains(msg, "Ship v2.0 to prod now.") || !strings.Contains(msg, "arguments: v2.0 to prod") {
		t.Errorf("$ARGUMENTS substitution: %v %q", ok, msg)
	}
	if msg, ok := ExpandSkillInvocation(cwd, "/tools:bare"); !ok || !strings.Contains(msg, "Second line.") {
		t.Errorf("frontmatter-less command expands with its whole file: %v %q", ok, msg)
	}
	if msg, ok := ExpandSkillInvocation(cwd, "/hello"); !ok || !strings.Contains(msg, "a user skill also called hello") {
		t.Errorf("bare name must resolve to the local skill, not the plugin's: %v %q", ok, msg)
	}
}

func TestSkillSource_RescansAfterGenerationBump(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	ctx := context.Background()
	src := NewSkillSource(cwd)
	if fms, _ := src.ListFrontmatters(ctx); len(fms) != 0 {
		t.Fatalf("empty to start: %+v", fms)
	}
	writeTestSkill(t, filepath.Join(cwd, ".ask", "skills"), "late", "", "arrived later")
	if fms, _ := src.ListFrontmatters(ctx); len(fms) != 0 {
		t.Fatalf("no rescan without a generation bump: %+v", fms)
	}
	BumpSkillsGeneration()
	fms, _ := src.ListFrontmatters(ctx)
	if len(fms) != 1 || fms[0].Name != "late" {
		t.Fatalf("bump must make the source rescan: %+v", fms)
	}
	if body, err := src.LoadInstructions(ctx, "late"); err != nil || !strings.Contains(body, "arrived later") {
		t.Fatalf("body after rescan: %v %q", err, body)
	}
}

func TestSkillStore_CreateUpdateDelete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(cwd, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := CreateSkill(sub, OriginProject, SkillSpec{Name: "release-notes", Description: "Write release notes: use when the user asks for a changelog", Body: "1. Read git log.\n2. Summarise."})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(cwd, ".ask", "skills", "release-notes", "SKILL.md")
	if s.Path != wantPath || s.Origin.Scope != OriginProject || !s.UserInvocable {
		t.Fatalf("created: %+v", s)
	}
	data, _ := os.ReadFile(wantPath)
	if !strings.Contains(string(data), `description: "Write release notes: use when the user asks for a changelog"`) {
		t.Fatalf("description with a colon must be quoted:\n%s", data)
	}
	if got, ok := FindSkill(sub, "release-notes"); !ok || got.Description != "Write release notes: use when the user asks for a changelog" {
		t.Fatalf("discover after create: %v %+v", ok, got)
	}
	if _, err := CreateSkill(sub, OriginProject, SkillSpec{Name: "release-notes", Description: "d", Body: "b"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create: %v", err)
	}
	if _, err := CreateSkill(sub, OriginProject, SkillSpec{Name: "Bad Name", Description: "d", Body: "b"}); err == nil {
		t.Fatal("bad name must be rejected")
	}
	if _, err := CreateSkill(sub, OriginProject, SkillSpec{Name: "nodesc", Body: "b"}); err == nil {
		t.Fatal("missing description must be rejected")
	}
	if _, err := CreateSkill(sub, OriginPlugin, SkillSpec{Name: "x", Description: "d", Body: "b"}); err == nil {
		t.Fatal("plugin scope is not writable")
	}

	u, err := CreateSkill(sub, OriginUser, SkillSpec{Name: "release-notes", Description: "user copy", Body: "user body", UserInvocable: func() *bool { b := false; return &b }()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.Path, filepath.Join(home, ".config", "ask", "skills")) || u.UserInvocable {
		t.Fatalf("user copy: %+v", u)
	}
	if _, err := UpdateSkill(sub, "release-notes", "", SkillPatch{}); err == nil || !strings.Contains(err.Error(), "more than one scope") {
		t.Fatalf("ambiguous update must ask for scope: %v", err)
	}
	desc := "Updated description"
	body := "New body"
	on := true
	updated, err := UpdateSkill(sub, "release-notes", OriginProject, SkillPatch{Description: &desc, Body: &body, DisableModelInvocation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != desc || !updated.DisableModelInvocation || updated.Path != wantPath {
		t.Fatalf("updated: %+v", updated)
	}
	data, _ = os.ReadFile(wantPath)
	if !strings.Contains(string(data), "name: release-notes\n") || !strings.Contains(string(data), "disable-model-invocation: true") || !strings.HasSuffix(string(data), "New body\n") {
		t.Fatalf("updated file:\n%s", data)
	}
	off := false
	if _, err := UpdateSkill(sub, "release-notes", OriginProject, SkillPatch{DisableModelInvocation: &off}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(wantPath)
	if strings.Contains(string(data), "disable-model-invocation") {
		t.Fatalf("clearing a flag drops the key:\n%s", data)
	}

	if err := DeleteSkill(sub, "release-notes", OriginUser); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(u.Dir); !os.IsNotExist(err) {
		t.Fatal("user package must be gone")
	}
	if err := DeleteSkill(sub, "release-notes", ""); err != nil {
		t.Fatalf("unambiguous delete without scope: %v", err)
	}
	if _, ok := FindSkill(sub, "release-notes"); ok {
		t.Fatal("project package must be gone")
	}
	if err := DeleteSkill(sub, "release-notes", ""); err == nil {
		t.Fatal("deleting a missing skill must error")
	}
}

func TestSkillStore_PluginItemsAreReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	installFixturePlugin(t, home, cwd)
	d := "x"
	if _, err := UpdateSkill(cwd, "tools:hello", "", SkillPatch{Description: &d}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("plugin skill edit: %v", err)
	}
	if err := DeleteSkill(cwd, "tools:hello", ""); err == nil {
		t.Fatal("plugin skill delete must fail")
	}
	if _, err := UpdateAgent(cwd, "tools:reviewer", "", AgentPatch{Description: &d}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("plugin agent edit: %v", err)
	}
	if err := DeleteAgent(cwd, "tools:reviewer", ""); err == nil {
		t.Fatal("plugin agent delete must fail")
	}
}

func TestDiscoverSubagents_PluginOrigins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	writeTestSubagent(t, filepath.Join(cwd, ".ask", "agents"), "reviewer", "", "Project reviewer.")
	installFixturePlugin(t, home, cwd)
	defs := DiscoverSubagents(cwd)
	byName := map[string]SubagentDef{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	if len(defs) != 2 {
		t.Fatalf("want reviewer + tools:reviewer, got %v", byName)
	}
	if d := byName["reviewer"]; d.Origin.Scope != OriginProject || d.BareName != "reviewer" {
		t.Errorf("project agent: %+v", d)
	}
	if d := byName["tools:reviewer"]; d.Origin.Scope != OriginPlugin || d.Origin.Plugin != "tools@mkt" || d.Provider != "vertex" || d.BareName != "reviewer" {
		t.Errorf("plugin agent: %+v", d)
	}
	if d, ok := FindSubagent(cwd, "tools:reviewer"); !ok || d.Prompt != "You review." {
		t.Errorf("FindSubagent: %v %+v", ok, d)
	}
}

func TestAgentStore_CreateUpdateDelete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := CreateAgent(cwd, OriginProject, AgentSpec{Name: "critic", Description: "Critiques: plans", Prompt: "Be harsh.", Tools: []string{"read", "grep"}, Provider: "vertex", Model: "gemini-x"})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(cwd, ".ask", "agents", "critic.md")
	if d.Source != wantPath || d.Origin.Scope != OriginProject || len(d.Tools) != 2 || d.Provider != "vertex" || d.Model != "gemini-x" {
		t.Fatalf("created agent: %+v", d)
	}
	if _, err := CreateAgent(cwd, OriginProject, AgentSpec{Name: "critic", Description: "d", Prompt: "p"}); err == nil {
		t.Fatal("duplicate create must fail")
	}
	if _, err := CreateAgent(cwd, OriginUser, AgentSpec{Name: "critic", Description: "", Prompt: "p"}); err == nil {
		t.Fatal("missing description must fail")
	}
	empty := ""
	prompt := "Be kind."
	tools := []string{"*"}
	u, err := UpdateAgent(cwd, "critic", "", AgentPatch{Provider: &empty, Prompt: &prompt, Tools: &tools})
	if err != nil {
		t.Fatal(err)
	}
	if u.Provider != "" || u.Model != "gemini-x" || u.Prompt != "Be kind." || len(u.Tools) != 1 || u.Tools[0] != "*" {
		t.Fatalf("updated agent: %+v", u)
	}
	data, _ := os.ReadFile(wantPath)
	if strings.Contains(string(data), "provider:") || !strings.Contains(string(data), "model: gemini-x") {
		t.Fatalf("updated file:\n%s", data)
	}
	if err := DeleteAgent(cwd, "critic", OriginProject); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Fatal("agent file must be gone")
	}
}

func TestRenderSkillFile_RoundTrip(t *testing.T) {
	off := false
	data, err := RenderSkillFile(SkillSpec{Name: "quote-me", Description: `Has "quotes" and a # hash`, Body: "body", UserInvocable: &off, License: "MIT"})
	if err != nil {
		t.Fatal(err)
	}
	fields, body, ok := ParseFrontmatterBytes(data)
	if !ok || fields["name"] != "quote-me" || fields["description"] != `Has "quotes" and a # hash` || fields["user-invocable"] != "false" || fields["license"] != "MIT" || strings.TrimSpace(body) != "body" {
		t.Fatalf("round trip: %v %v %q\n%s", ok, fields, body, data)
	}
}
