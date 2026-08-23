package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeMarketplaceFixture builds a marketplace named "mkt" with a full
// plugin (plugin.json + skill + agent + workflow + command) and a bare-skill
// entry in the anthropics/skills shape (no plugin.json, strict:false).
func writeMarketplaceFixture(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, ".claude-plugin", "marketplace.json"), `{
  "$schema": "https://anthropic.com/claude-code/marketplace.schema.json",
  "name": "mkt",
  "owner": {"name": "Tester"},
  "plugins": [
    {"name": "tools", "source": "./plugins/tools", "description": "Dev tools", "version": "1.0.0", "author": "Someone"},
    {"name": "bare", "source": "./", "strict": false, "skills": ["./skills/xlsx"], "description": "Bare skill"}
  ]
}`)
	writeFile(t, filepath.Join(dir, "plugins", "tools", ".claude-plugin", "plugin.json"),
		`{"name": "tools", "version": "1.0.0", "description": "Dev tools plugin", "author": {"name": "Someone"}}`)
	writeFile(t, filepath.Join(dir, "plugins", "tools", "skills", "hello", "SKILL.md"),
		"---\nname: hello\ndescription: Say hello\n---\nSay hello to the user.\n")
	writeFile(t, filepath.Join(dir, "plugins", "tools", "agents", "reviewer.md"),
		"---\nname: reviewer\ndescription: Reviews code\n---\nYou review code.\n")
	writeFile(t, filepath.Join(dir, "plugins", "tools", "commands", "ship.md"),
		"---\ndescription: Ship it\n---\nShip $ARGUMENTS now.\n")
	writeFile(t, filepath.Join(dir, "plugins", "tools", "workflows", "release.json"),
		`{"name": "release", "description": "Cut a release", "steps": [{"name": "plan", "provider": "vertex", "prompt": "plan"}]}`)
	writeFile(t, filepath.Join(dir, "skills", "xlsx", "SKILL.md"),
		"---\nname: xlsx\ndescription: Work with spreadsheets\n---\nUse openpyxl.\n")
}

func isolate(t *testing.T) (home, cwd string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	cwd = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home, cwd
}

func TestManifest_ParseShapes(t *testing.T) {
	var m MarketplaceManifest
	err := json.Unmarshal([]byte(`{
  "name": "shapes", "owner": {"name": "o"},
  "plugins": [
    {"name": "a", "source": "./plugins/a", "skills": "./skills/", "author": "Ann"},
    {"name": "b", "source": "owner/repo", "skills": ["./x", "./y"], "author": {"name": "Bob", "email": "b@x"}},
    {"name": "c", "source": {"source": "url", "url": "https://example.com/c.git", "sha": "abc"}},
    {"name": "d", "source": {"source": "git-subdir", "url": "https://example.com/d.git", "path": "plugins/d", "ref": "main"}, "strict": false}
  ]}`), &m)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Plugins) != 4 {
		t.Fatalf("plugins: %d", len(m.Plugins))
	}
	a, b, c, d := m.Plugins[0], m.Plugins[1], m.Plugins[2], m.Plugins[3]
	if a.Source.Kind != SourcePath || a.Source.Path != "./plugins/a" || len(a.Skills) != 1 || a.Skills[0] != "./skills/" {
		t.Errorf("a: %+v", a)
	}
	if a.Author == nil || a.Author.Name != "Ann" {
		t.Errorf("a author: %+v", a.Author)
	}
	if b.Source.Kind != SourceGitHub || b.Source.Repo != "owner/repo" || len(b.Skills) != 2 || b.Author.Email != "b@x" {
		t.Errorf("b: %+v", b)
	}
	if b.Source.GitURL() != "https://github.com/owner/repo.git" {
		t.Errorf("b git url: %s", b.Source.GitURL())
	}
	if c.Source.Kind != SourceURL || c.Source.SHA != "abc" || !c.Source.Remote() || !c.IsStrict() {
		t.Errorf("c: %+v", c)
	}
	if d.Source.Kind != SourceGitSubdir || d.Source.Path != "plugins/d" || d.Source.Ref != "main" || d.IsStrict() {
		t.Errorf("d: %+v", d)
	}
	out, _ := json.Marshal(a.Source)
	if string(out) != `"./plugins/a"` {
		t.Errorf("path source must marshal as a string: %s", out)
	}
	out, _ = json.Marshal(d.Source)
	if !strings.Contains(string(out), `"source":"git-subdir"`) || !strings.Contains(string(out), `"ref":"main"`) {
		t.Errorf("object source round-trip: %s", out)
	}
	if err := json.Unmarshal([]byte(`{"name":"x","owner":{"name":"o"},"plugins":[{"name":"p","source":{"url":"u"}}]}`), &m); err == nil {
		t.Error("source object without kind must fail")
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "my-plugin", "x1-2y"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Upper", "has space", "-lead", "trail-", "double--dash", "under_score", strings.Repeat("a", 65)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestParseMarketplaceSource(t *testing.T) {
	_, cwd := isolate(t)
	local := filepath.Join(cwd, "mp")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		kind string
		want string
	}{
		{"anthropics/skills", MarketplaceSourceGitHub, "anthropics/skills"},
		{"https://github.com/x/y.git", MarketplaceSourceGit, "https://github.com/x/y.git"},
		{"git@github.com:x/y.git", MarketplaceSourceGit, "git@github.com:x/y.git"},
		{"./mp", MarketplaceSourceDirectory, local},
		{local, MarketplaceSourceDirectory, local},
		{"https://example.com/.claude-plugin/marketplace.json", MarketplaceSourceURL, "https://example.com/.claude-plugin/marketplace.json"},
	}
	for _, c := range cases {
		src, err := ParseMarketplaceSource(cwd, c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if src.Kind != c.kind || src.Raw() != c.want {
			t.Errorf("%q → %+v, want kind=%s raw=%s", c.in, src, c.kind, c.want)
		}
	}
	for _, bad := range []string{"", "nope", "./missing-dir"} {
		if _, err := ParseMarketplaceSource(cwd, bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestDirectoryMarketplace_InstallAndUninstall(t *testing.T) {
	home, cwd := isolate(t)
	fixture := filepath.Join(home, "fixture")
	writeMarketplaceFixture(t, fixture)
	ctx := context.Background()

	m, err := AddMarketplace(ctx, cwd, fixture, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "mkt" || m.Scope != ScopeUser || m.Dir != fixture || !m.Fetched() || !m.Writable() {
		t.Fatalf("marketplace: %+v", m)
	}
	known := readKnownMarketplaces()
	if km, ok := known["mkt"]; !ok || km.Source.Kind != MarketplaceSourceDirectory || km.Source.Path != fixture || km.InstallLocation != fixture {
		t.Fatalf("known_marketplaces: %+v", known)
	}
	if _, err := AddMarketplace(ctx, cwd, fixture, ScopeUser); err != nil {
		t.Fatalf("re-adding the same source must be idempotent: %v", err)
	}
	if got := ListMarketplaces(cwd); len(got) != 1 {
		t.Fatalf("list: %+v", got)
	}

	in, err := InstallPlugin(ctx, cwd, Ref{"tools", "mkt"}, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(CacheDir(), "mkt", "tools", "1.0.0")
	if in.Dir != wantDir || in.Version != "1.0.0" || in.Manifest == nil || in.Manifest.Description != "Dev tools plugin" {
		t.Fatalf("installed: %+v", in)
	}
	if !fileExists(filepath.Join(wantDir, "skills", "hello", "SKILL.md")) {
		t.Fatal("skill not copied into the cache")
	}
	c := in.Contents()
	if len(c.SkillDirs) != 1 || len(c.AgentFiles) != 1 || len(c.CommandFiles) != 1 || len(c.WorkflowFiles) != 1 {
		t.Fatalf("contents: %+v", c)
	}
	f := readInstalled()
	recs := f.Plugins["tools@mkt"]
	if len(recs) != 1 || recs[0].Scope != ScopeUser || recs[0].InstallPath != wantDir || recs[0].Entry == nil || recs[0].Entry.Name != "tools" {
		t.Fatalf("installed_plugins.json: %+v", recs)
	}

	bare, err := InstallPlugin(ctx, cwd, Ref{"bare", "mkt"}, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if bare.Manifest != nil || bare.Version != "latest" {
		t.Fatalf("bare: %+v", bare)
	}
	bc := bare.Contents()
	if len(bc.SkillDirs) != 1 || filepath.Base(bc.SkillDirs[0]) != "xlsx" || len(bc.AgentFiles) != 0 {
		t.Fatalf("bare contents (strict:false entry must resolve skills): %+v", bc)
	}

	enabled := EnabledPlugins(cwd)
	if len(enabled) != 2 || enabled[0].Ref.String() != "bare@mkt" || enabled[1].Ref.String() != "tools@mkt" {
		t.Fatalf("enabled: %+v", enabled)
	}
	if err := SetEnabled(Ref{"bare", "mkt"}, false); err != nil {
		t.Fatal(err)
	}
	if got := EnabledPlugins(cwd); len(got) != 1 || got[0].Ref.Plugin != "tools" {
		t.Fatalf("disabled plugin must drop out: %+v", got)
	}
	if all := InstalledPlugins(cwd); len(all) != 2 || all[0].Enabled || !all[1].Enabled {
		t.Fatalf("installed lens keeps disabled plugins: %+v", all)
	}

	if _, err := InstallPlugin(ctx, cwd, Ref{"missing", "mkt"}, ScopeUser); err == nil || !strings.Contains(err.Error(), "not in marketplace") {
		t.Errorf("unknown plugin: %v", err)
	}
	if _, err := InstallPlugin(ctx, cwd, Ref{"tools", "nope"}, ScopeUser); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("unknown marketplace: %v", err)
	}

	if err := UninstallPlugin(cwd, Ref{"tools", "mkt"}, ScopeUser); err != nil {
		t.Fatal(err)
	}
	if dirExists(filepath.Join(CacheDir(), "mkt", "tools")) {
		t.Fatal("cache copy must be removed with the last record")
	}
	if err := UninstallPlugin(cwd, Ref{"tools", "mkt"}, ScopeUser); err == nil {
		t.Fatal("second uninstall must error")
	}
	if err := RemoveMarketplace(cwd, "mkt", ScopeUser); err != nil {
		t.Fatal(err)
	}
	if len(readKnownMarketplaces()) != 0 || !dirExists(fixture) {
		t.Fatal("removing a directory marketplace must unregister it and leave the directory alone")
	}
}

func TestProjectScope_SyncOnAnotherMachine(t *testing.T) {
	home, cwd := isolate(t)
	fixture := filepath.Join(home, "fixture")
	writeMarketplaceFixture(t, fixture)
	ctx := context.Background()
	if _, err := AddMarketplace(ctx, cwd, fixture, ScopeUser); err != nil {
		t.Fatal(err)
	}
	in, err := InstallPlugin(ctx, cwd, Ref{"tools", "mkt"}, ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if in.Scope != ScopeProject {
		t.Fatalf("scope: %+v", in)
	}
	pf := ReadProjectFile(cwd)
	if !pf.Enabled["tools@mkt"] || pf.Marketplaces["mkt"].Source.Path != fixture {
		t.Fatalf("project file: %+v", pf)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".ask", "plugins.json")); err != nil {
		t.Fatal("project file must live at <root>/.ask/plugins.json")
	}
	if got := EnabledPlugins(cwd); len(got) != 1 || got[0].Scope != ScopeProject || got[0].Missing {
		t.Fatalf("enabled: %+v", got)
	}

	// Another machine: fresh HOME, same checkout. The project file names a
	// plugin that is not cached here.
	t.Setenv("HOME", t.TempDir())
	got := EnabledPlugins(cwd)
	if len(got) != 1 || !got[0].Missing || got[0].Entry == nil || got[0].Entry.Name != "tools" {
		t.Fatalf("missing plugin must still be listed with its catalog entry: %+v", got)
	}
	if ms := ListMarketplaces(cwd); len(ms) != 1 || ms[0].Scope != ScopeProject || !ms[0].Fetched() {
		t.Fatalf("project marketplace must resolve without a user registration: %+v", ms)
	}
	rep := SyncProject(ctx, cwd)
	if len(rep.Errors) != 0 || len(rep.Installed) != 1 {
		t.Fatalf("sync: %+v", rep)
	}
	if got := EnabledPlugins(cwd); len(got) != 1 || got[0].Missing || got[0].Contents().Count() != 4 {
		t.Fatalf("after sync: %+v", got)
	}
	if err := UninstallPlugin(cwd, Ref{"tools", "mkt"}, ScopeProject); err != nil {
		t.Fatal(err)
	}
	if pf := ReadProjectFile(cwd); pf.Enabled["tools@mkt"] {
		t.Fatalf("project uninstall must clear the enabled flag: %+v", pf)
	}
}

type fakeGit struct {
	calls   [][]string
	fixture string
}

func (g *fakeGit) run(_ context.Context, dir string, args ...string) (string, error) {
	g.calls = append(g.calls, append([]string{dir}, args...))
	switch args[0] {
	case "clone":
		dest := args[len(args)-1]
		if err := copyDir(g.fixture, dest); err != nil {
			return "", err
		}
		return "", os.MkdirAll(filepath.Join(dest, ".git"), 0o755)
	case "rev-parse":
		return "0123456789abcdef0123456789abcdef01234567\n", nil
	case "remote":
		return "origin\n", nil
	}
	return "", nil
}

func (g *fakeGit) has(verb string) bool {
	for _, c := range g.calls {
		if len(c) > 1 && c[1] == verb {
			return true
		}
	}
	return false
}

func TestGitMarketplace_CloneRefreshAndRemoteInstall(t *testing.T) {
	home, cwd := isolate(t)
	fixture := filepath.Join(home, "fixture")
	writeMarketplaceFixture(t, fixture)
	// A remote-source entry in the anthropics shape.
	writeFile(t, filepath.Join(fixture, ".claude-plugin", "marketplace.json"), `{
  "name": "remote", "owner": {"name": "o"},
  "plugins": [
    {"name": "tools", "source": "./plugins/tools", "version": "1.0.0"},
    {"name": "ext", "source": {"source": "git-subdir", "url": "https://example.com/ext.git", "path": "plugins/tools", "ref": "main"}}
  ]}`)
	g := &fakeGit{fixture: fixture}
	orig := RunGit
	RunGit = g.run
	t.Cleanup(func() { RunGit = orig })
	ctx := context.Background()

	m, err := AddMarketplace(ctx, cwd, "owner/repo", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "remote" || m.Dir != filepath.Join(MarketplacesDir(), "remote") || !m.Fetched() {
		t.Fatalf("marketplace: %+v", m)
	}
	if !g.has("clone") || g.calls[0][2] != "--quiet" || g.calls[0][3] != "https://github.com/owner/repo.git" {
		t.Fatalf("clone call: %+v", g.calls)
	}
	if km := readKnownMarketplaces()["remote"]; km.Source.Kind != MarketplaceSourceGitHub || km.Source.Repo != "owner/repo" {
		t.Fatalf("registration: %+v", km)
	}
	if err := RefreshMarketplace(ctx, cwd, m); err != nil {
		t.Fatal(err)
	}
	if !g.has("pull") {
		t.Fatalf("refresh must pull: %+v", g.calls)
	}

	in, err := InstallPlugin(ctx, cwd, Ref{"tools", "remote"}, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if in.Version != "1.0.0" {
		t.Fatalf("path-source install: %+v", in)
	}
	g.calls = nil
	ext, err := InstallPlugin(ctx, cwd, Ref{"ext", "remote"}, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if ext.Version != "0123456789ab" || ext.Contents().Count() != 4 {
		t.Fatalf("remote install: %+v contents=%+v", ext, ext.Contents())
	}
	if !g.has("clone") || !g.has("checkout") {
		t.Fatalf("remote install must clone + checkout: %+v", g.calls)
	}
	if rec := readInstalled().Plugins["ext@remote"]; len(rec) != 1 || rec[0].GitCommitSha == "" {
		t.Fatalf("sha recorded: %+v", rec)
	}
	entries, _ := os.ReadDir(CacheDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temporary clone left behind: %s", e.Name())
		}
	}

	if err := RemoveMarketplace(cwd, "remote", ScopeUser); err != nil {
		t.Fatal(err)
	}
	if dirExists(m.Dir) {
		t.Fatal("unreferenced clone must be deleted")
	}
}

func TestPublish_DirectoryAndGit(t *testing.T) {
	home, cwd := isolate(t)
	fixture := filepath.Join(home, "fixture")
	writeMarketplaceFixture(t, fixture)
	ctx := context.Background()
	m, err := AddMarketplace(ctx, cwd, fixture, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(home, "mine", "deploy")
	writeFile(t, filepath.Join(skill, "SKILL.md"), "---\nname: deploy\ndescription: Deploy things\n---\nbody\n")
	writeFile(t, filepath.Join(skill, "scripts", "run.sh"), "echo hi\n")
	agent := filepath.Join(home, "mine", "critic.md")
	writeFile(t, agent, "---\nname: critic\ndescription: Critiques\n---\nprompt\n")
	wf := filepath.Join(home, "mine", "ship.json")
	writeFile(t, wf, `{"name":"ship","steps":[{"name":"a","provider":"vertex","prompt":"p"}]}`)

	res, err := Publish(ctx, m, PublishRequest{PluginName: "deploy", Description: "Deploy plugin", SkillDirs: []string{skill}, AgentFiles: []string{agent}, WorkflowFiles: []string{wf}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Committed || res.Note == "" {
		t.Fatalf("non-git marketplace must not commit: %+v", res)
	}
	pdir := filepath.Join(fixture, "plugins", "deploy")
	for _, p := range []string{"skills/deploy/SKILL.md", "skills/deploy/scripts/run.sh", "agents/critic.md", "workflows/ship.json", ".claude-plugin/plugin.json"} {
		if !fileExists(filepath.Join(pdir, p)) {
			t.Errorf("missing %s", p)
		}
	}
	man, _ := ReadPluginManifest(pdir)
	if man == nil || man.Name != "deploy" || man.Version != "1.0.0" || man.Description != "Deploy plugin" {
		t.Fatalf("plugin.json: %+v", man)
	}
	raw, _ := os.ReadFile(filepath.Join(fixture, ".claude-plugin", "marketplace.json"))
	if !strings.Contains(string(raw), `"$schema"`) {
		t.Fatal("unknown top-level fields must survive the rewrite")
	}
	mm, _ := ReadMarketplaceManifest(fixture)
	e, ok := mm.Entry("deploy")
	if !ok || e.Source.Path != "./plugins/deploy" || e.Version != "1.0.0" || len(mm.Plugins) != 3 {
		t.Fatalf("catalog entry: %+v (%d plugins)", e, len(mm.Plugins))
	}
	// Installing the published plugin round-trips.
	in, err := InstallPlugin(ctx, cwd, Ref{"deploy", "mkt"}, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if c := in.Contents(); len(c.SkillDirs) != 1 || len(c.AgentFiles) != 1 || len(c.WorkflowFiles) != 1 {
		t.Fatalf("round-trip contents: %+v", c)
	}

	// Re-publish with a bumped version updates the entry instead of adding one.
	if _, err := Publish(ctx, m, PublishRequest{PluginName: "deploy", Version: "1.1.0", SkillDirs: []string{skill}}); err != nil {
		t.Fatal(err)
	}
	mm, _ = ReadMarketplaceManifest(fixture)
	if e, _ := mm.Entry("deploy"); e.Version != "1.1.0" || e.Description != "Deploy plugin" || len(mm.Plugins) != 3 {
		t.Fatalf("entry update: %+v (%d plugins)", e, len(mm.Plugins))
	}

	// Git-backed: commit + push.
	g := &fakeGit{fixture: fixture}
	orig := RunGit
	RunGit = g.run
	t.Cleanup(func() { RunGit = orig })
	if err := os.MkdirAll(filepath.Join(fixture, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err = Publish(ctx, m, PublishRequest{PluginName: "deploy", SkillDirs: []string{skill}, Message: "ship deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed || !res.Pushed || res.Commit == "" || !g.has("pull") || !g.has("add") || !g.has("commit") || !g.has("push") {
		t.Fatalf("git publish pulls, commits, pushes by default: %+v calls=%+v", res, g.calls)
	}
	if res.Version != "1.1.1" {
		t.Fatalf("republishing without a version bumps the patch number: %s", res.Version)
	}
	g.calls = nil
	res, err = Publish(ctx, m, PublishRequest{PluginName: "deploy", SkillDirs: []string{skill}, NoPush: true, Message: "ship deploy"})
	if err != nil || res.Pushed || g.has("push") || g.has("pull") || !res.Committed {
		t.Fatalf("NoPush keeps it local: %+v %v calls=%+v", res, err, g.calls)
	}
	for _, c := range g.calls {
		if c[1] == "commit" && c[len(c)-1] != "ship deploy" {
			t.Errorf("commit message: %+v", c)
		}
	}

	if _, err := Publish(ctx, m, PublishRequest{PluginName: "Bad Name", SkillDirs: []string{skill}}); err == nil {
		t.Error("invalid plugin name must be rejected")
	}
	if _, err := Publish(ctx, m, PublishRequest{PluginName: "empty"}); err == nil {
		t.Error("empty publish must be rejected")
	}
	urlMkt := Marketplace{Name: "u", Source: MarketplaceSource{Kind: MarketplaceSourceURL, URL: "https://x/m.json"}, Dir: fixture, Manifest: mm}
	if _, err := Publish(ctx, urlMkt, PublishRequest{PluginName: "deploy", SkillDirs: []string{skill}}); err == nil {
		t.Error("url marketplaces are read-only")
	}
}

func TestInitMarketplace(t *testing.T) {
	home, _ := isolate(t)
	g := &fakeGit{}
	orig := RunGit
	RunGit = g.run
	t.Cleanup(func() { RunGit = orig })
	dir := filepath.Join(home, "my-mkt")
	if err := InitMarketplace(context.Background(), dir, "my-mkt", "Me"); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMarketplaceManifest(dir)
	if err != nil || m.Name != "my-mkt" || m.Owner.Name != "Me" || len(m.Plugins) != 0 {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	if !g.has("init") {
		t.Fatalf("git init expected: %+v", g.calls)
	}
	if err := InitMarketplace(context.Background(), dir, "my-mkt", "Me"); err == nil {
		t.Fatal("re-init must fail")
	}
	if err := InitMarketplace(context.Background(), filepath.Join(home, "x"), "Bad Name", "Me"); err == nil {
		t.Fatal("bad name must fail")
	}
}

func TestImportFromClaude(t *testing.T) {
	home, cwd := isolate(t)
	fixture := filepath.Join(home, "fixture")
	writeMarketplaceFixture(t, fixture)
	claude := filepath.Join(home, "claude-home")
	origHome := ClaudeHome
	ClaudeHome = func() string { return claude }
	t.Cleanup(func() { ClaudeHome = origHome })
	writeFile(t, filepath.Join(claude, "plugins", "known_marketplaces.json"), `{
  "mkt": {"source": {"source": "directory", "path": "`+fixture+`"}, "installLocation": "`+fixture+`", "lastUpdated": "2026-08-23T01:11:13.464Z"}
}`)
	writeFile(t, filepath.Join(claude, "settings.json"), `{"enabledPlugins": {"tools@mkt": true, "ghost@nowhere": true}, "model": "opus"}`)
	writeFile(t, filepath.Join(cwd, ".claude", "settings.json"), `{"enabledPlugins": {"bare@mkt": true}}`)

	st := ReadClaudeState(cwd)
	if st.Empty() || len(st.Marketplaces) != 1 || !st.UserEnabled["tools@mkt"] || !st.ProjectEnabled["bare@mkt"] {
		t.Fatalf("state: %+v", st)
	}
	if refs := st.EnabledRefs(); len(refs) != 3 {
		t.Fatalf("refs: %v", refs)
	}
	rep := ImportFromClaude(context.Background(), cwd, st, ScopeUser)
	if len(rep.Marketplaces) != 1 || len(rep.Plugins) != 2 || len(rep.Errors) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if !strings.Contains(rep.Errors[0], "ghost@nowhere") {
		t.Fatalf("unknown marketplace plugin must be reported, not fatal: %+v", rep.Errors)
	}
	if got := EnabledPlugins(cwd); len(got) != 2 {
		t.Fatalf("enabled after import: %+v", got)
	}
	if ms := ListMarketplaces(cwd); len(ms) != 1 || ms[0].Source.Path != fixture {
		t.Fatalf("marketplaces: %+v", ms)
	}
	again := ImportFromClaude(context.Background(), cwd, st, ScopeUser)
	if len(again.Marketplaces) != 0 || len(again.Plugins) != 0 || len(again.Skipped) != 3 {
		t.Fatalf("second import must skip everything present: %+v", again)
	}
	if got := ReadClaudeState(filepath.Join(home, "elsewhere")); got.ProjectEnabled != nil {
		t.Fatalf("no project settings outside the checkout: %+v", got)
	}
	if !strings.Contains(rep.Summary(), "1 marketplace(s), 2 plugin(s)") {
		t.Errorf("summary: %s", rep.Summary())
	}
}

func TestResolveContents_ShapesAndSafety(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: root\ndescription: d\n---\nx")
	writeFile(t, filepath.Join(dir, "extra", "one", "SKILL.md"), "---\nname: one\ndescription: d\n---\nx")
	writeFile(t, filepath.Join(dir, "agents", "a.md"), "x")
	writeFile(t, filepath.Join(dir, "agents", ".hidden", "b.md"), "x")
	writeFile(t, filepath.Join(dir, "commands", "nested", "c.md"), "x")
	writeFile(t, filepath.Join(dir, "workflows", "w.json"), "{}")
	writeFile(t, filepath.Join(dir, "workflows", "notes.txt"), "x")
	outside := filepath.Join(filepath.Dir(dir), "outside")
	writeFile(t, filepath.Join(outside, "SKILL.md"), "---\nname: outside\ndescription: d\n---\nx")

	man := &PluginManifest{Name: "p", Skills: PathList{"./extra", "../outside"}}
	c := ResolveContents(dir, nil, man)
	if len(c.SkillDirs) != 2 || c.SkillDirs[0] != dir || filepath.Base(c.SkillDirs[1]) != "one" {
		t.Fatalf("skills (root shorthand + manifest dir, escape ignored): %+v", c.SkillDirs)
	}
	if len(c.AgentFiles) != 1 || len(c.CommandFiles) != 1 || len(c.WorkflowFiles) != 1 {
		t.Fatalf("defaults: %+v", c)
	}
	strict := false
	entry := &Entry{Name: "p", Strict: &strict, Skills: PathList{"./extra/one"}}
	c = ResolveContents(dir, entry, man)
	if len(c.SkillDirs) != 2 {
		t.Fatalf("strict:false uses the entry paths only (plus root shorthand): %+v", c.SkillDirs)
	}
	entry = &Entry{Name: "p", Skills: PathList{"./extra/one"}}
	man = &PluginManifest{Name: "p", Agents: PathList{"./agents/a.md"}}
	c = ResolveContents(dir, entry, man)
	if len(c.SkillDirs) != 2 || len(c.AgentFiles) != 1 {
		t.Fatalf("strict merges both: %+v", c)
	}
	if c := ResolveContents(filepath.Join(dir, "nothing-here"), nil, nil); !c.Empty() {
		t.Fatalf("missing dir: %+v", c)
	}
}

func TestParseRef(t *testing.T) {
	r, err := ParseRef("document-skills@anthropic-agent-skills")
	if err != nil || r.Plugin != "document-skills" || r.Marketplace != "anthropic-agent-skills" || r.String() != "document-skills@anthropic-agent-skills" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "noat", "@mkt", "name@"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
}

func TestRemoveMarketplace_KeepsCloneWhileReferenced(t *testing.T) {
	home, cwd := isolate(t)
	fixture := filepath.Join(home, "fixture")
	writeMarketplaceFixture(t, fixture)
	g := &fakeGit{fixture: fixture}
	orig := RunGit
	RunGit = g.run
	t.Cleanup(func() { RunGit = orig })
	ctx := context.Background()
	m, err := AddMarketplace(ctx, cwd, "o/r", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddMarketplace(ctx, cwd, "o/r", ScopeProject); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMarketplace(cwd, "mkt", ScopeUser); err != nil {
		t.Fatal(err)
	}
	if !dirExists(m.Dir) {
		t.Fatal("clone still referenced by the project must survive")
	}
	if ms := ListMarketplaces(cwd); len(ms) != 1 || ms[0].Scope != ScopeProject {
		t.Fatalf("project registration must remain: %+v", ms)
	}
	if err := RemoveMarketplace(cwd, "mkt", ScopeProject); err != nil {
		t.Fatal(err)
	}
	if dirExists(m.Dir) {
		t.Fatal("clone must go with the last registration")
	}
	if err := RemoveMarketplace(cwd, "mkt", ScopeProject); err == nil {
		t.Fatal("removing an unknown marketplace must error")
	}
}

func TestPublications_StatusAndPull(t *testing.T) {
	home, cwd := isolate(t)
	fixture := filepath.Join(home, "fixture")
	writeMarketplaceFixture(t, fixture)
	ctx := context.Background()
	m, err := AddMarketplace(ctx, cwd, fixture, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(cwd, ".ask", "skills", "deploy")
	writeFile(t, filepath.Join(skill, "SKILL.md"), "---\nname: deploy\ndescription: d\n---\nv1\n")
	res, err := Publish(ctx, m, PublishRequest{PluginName: "deploy", SkillDirs: []string{skill}})
	if err != nil {
		t.Fatal(err)
	}
	pub := Publication{Kind: "skill", Name: "deploy", Scope: "project", Marketplace: "mkt", Plugin: "deploy", File: "deploy", Version: res.Version, Hash: HashPath(skill)}
	if err := RecordPublication(cwd, pub); err != nil {
		t.Fatal(err)
	}
	if pf := ReadProjectFile(cwd); pf.Published["skill:deploy"].Plugin != "deploy" {
		t.Fatalf("project-scope publications live in .ask/plugins.json: %+v", pf)
	}
	got, ok := FindPublication(cwd, "skill", "deploy", "")
	if !ok || got.Hash != pub.Hash {
		t.Fatalf("find: %v %+v", ok, got)
	}
	if _, ok := PublicationForRef(cwd, Ref{"deploy", "mkt"}); !ok {
		t.Fatal("the marketplace plugin must resolve back to the publication")
	}
	if st := Status(cwd, pub, HashPath(skill)); st != SyncInSync {
		t.Fatalf("fresh publish is in sync, got %s", st)
	}

	// Local edit → local changes.
	writeFile(t, filepath.Join(skill, "SKILL.md"), "---\nname: deploy\ndescription: d\n---\nv2\n")
	if st := Status(cwd, pub, HashPath(skill)); st != SyncLocalChanged {
		t.Fatalf("want local changes, got %s", st)
	}
	// Republish → bumped version, back in sync once re-based.
	res, err = Publish(ctx, m, PublishRequest{PluginName: "deploy", SkillDirs: []string{skill}})
	if err != nil || res.Version != "1.0.1" {
		t.Fatalf("update publish: %+v %v", res, err)
	}
	pub.Hash = HashPath(skill)
	pub.Version = res.Version
	if err := RecordPublication(cwd, pub); err != nil {
		t.Fatal(err)
	}
	if st := Status(cwd, pub, HashPath(skill)); st != SyncInSync {
		t.Fatalf("after update: %s", st)
	}

	// Someone edits the marketplace copy → marketplace newer; Pull takes it.
	remote := PublishedCopyPath(m, pub)
	writeFile(t, filepath.Join(remote, "SKILL.md"), "---\nname: deploy\ndescription: d\n---\nv3 from marketplace\n")
	if st := Status(cwd, pub, HashPath(skill)); st != SyncMarketplaceChanged {
		t.Fatalf("want marketplace newer, got %s", st)
	}
	writeFile(t, filepath.Join(skill, "SKILL.md"), "---\nname: deploy\ndescription: d\n---\nv2 local\n")
	if st := Status(cwd, pub, HashPath(skill)); st != SyncDiverged {
		t.Fatalf("want diverged, got %s", st)
	}
	pub, err = Pull(cwd, pub, skill)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(skill, "SKILL.md"))
	if !strings.Contains(string(data), "v3 from marketplace") {
		t.Fatalf("pull must replace the local copy:\n%s", data)
	}
	if st := Status(cwd, pub, HashPath(skill)); st != SyncInSync {
		t.Fatalf("after pull: %s", st)
	}
	if got, _ := FindPublication(cwd, "skill", "deploy", ""); got.Hash != pub.Hash {
		t.Fatal("pull must re-base the recorded hash")
	}

	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	if st := Status(cwd, pub, HashPath(skill)); st != SyncMissing {
		t.Fatalf("want missing, got %s", st)
	}
	if err := ForgetPublication(cwd, "skill", "deploy", "project"); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindPublication(cwd, "skill", "deploy", ""); ok {
		t.Fatal("forget must drop the link")
	}

	// User-scope publications live in the user file.
	upub := Publication{Kind: "agent", Name: "critic", Scope: "user", Marketplace: "mkt", Plugin: "critic", File: "critic.md", Hash: "h"}
	if err := RecordPublication(cwd, upub); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(publishedPath()); err != nil {
		t.Fatal("user publications file missing")
	}
	if list := Publications(cwd); len(list) != 1 || list[0].Name != "critic" {
		t.Fatalf("publications: %+v", list)
	}
}

func TestHashAndBump(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "SKILL.md"), "x")
	writeFile(t, filepath.Join(dir, "a", "scripts", "r.sh"), "y")
	writeFile(t, filepath.Join(dir, "a", ".git", "HEAD"), "ref")
	h1 := HashPath(filepath.Join(dir, "a"))
	writeFile(t, filepath.Join(dir, "b", "SKILL.md"), "x")
	writeFile(t, filepath.Join(dir, "b", "scripts", "r.sh"), "y")
	if h2 := HashPath(filepath.Join(dir, "b")); h1 != h2 {
		t.Fatal("same content (ignoring .git) must hash equal")
	}
	writeFile(t, filepath.Join(dir, "b", "scripts", "r.sh"), "z")
	if HashPath(filepath.Join(dir, "b")) == h1 {
		t.Fatal("content change must change the hash")
	}
	if HashBytes([]byte("x")) != HashPath(filepath.Join(dir, "a", "SKILL.md")) {
		t.Fatal("HashBytes must match HashPath of a file")
	}
	if HashPath(filepath.Join(dir, "nope")) != "" {
		t.Fatal("missing path hashes empty")
	}
	for in, want := range map[string]string{"1.0.0": "1.0.1", "2.3.9": "2.3.10", "1.0": "1.0", "latest": "latest", "1.0.x": "1.0.x"} {
		if got := BumpPatch(in); got != want {
			t.Errorf("BumpPatch(%q) = %q, want %q", in, got, want)
		}
	}
}
