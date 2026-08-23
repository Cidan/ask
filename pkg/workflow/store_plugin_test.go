package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/plugin"
)

func installWorkflowPlugin(t *testing.T, home, cwd string) {
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
	write(".claude-plugin/marketplace.json", `{"name":"mkt","owner":{"name":"t"},"plugins":[{"name":"tools","source":"./plugins/tools"}]}`)
	write("plugins/tools/workflows/release.json", `{"name":"release","description":"Cut a release","steps":[{"name":"plan","provider":"vertex","prompt":"p"}]}`)
	write("plugins/tools/workflows/broken.json", `not json`)
	ctx := context.Background()
	if _, err := plugin.AddMarketplace(ctx, cwd, mkt, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.InstallPlugin(ctx, cwd, plugin.Ref{Plugin: "tools", Marketplace: "mkt"}, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}
}

func TestPluginWorkflows_ReadOnlyScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	installWorkflowPlugin(t, home, cwd)

	all := ListAll(cwd)
	if len(all) != 1 || all[0].Name != "release" || all[0].Scope != ScopePlugin || all[0].Plugin != "tools@mkt" || !all[0].ReadOnly() {
		t.Fatalf("plugin workflow in ListAll: %+v", all)
	}
	w, err := ResolveByName(cwd, "release", ScopePlugin)
	if err != nil || w.Plugin != "tools@mkt" {
		t.Fatalf("resolve by plugin scope: %v %+v", err, w)
	}
	if w, err := ResolveByName(cwd, "release", ""); err != nil || w.Scope != ScopePlugin {
		t.Fatalf("resolve without scope: %v %+v", err, w)
	}
	if _, err := NormalizeScope(ScopePlugin); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("plugin scope is not a mutation target: %v", err)
	}

	// Saving the merged list never writes plugin workflows anywhere.
	if err := SaveAll(cwd, all); err != nil {
		t.Fatal(err)
	}
	if defs, _ := LoadUserWorkflows(cwd); len(defs) != 0 {
		t.Fatalf("plugin workflow must not leak into ask.json: %+v", defs)
	}
	if _, err := os.Stat(RepoDir(cwd)); !os.IsNotExist(err) {
		t.Fatal("plugin workflow must not leak into the repo dir")
	}

	// Copying a plugin workflow into another scope makes an editable copy.
	err = MutateWorkflows(cwd, func(items []Def) ([]Def, error) {
		src, err := ResolveByName(cwd, "release", ScopePlugin)
		if err != nil {
			return nil, err
		}
		src.Scope = ScopeRepo
		src.Plugin = ""
		return append(items, src), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	all = ListAll(cwd)
	if len(all) != 2 || all[0].Scope != ScopeRepo || all[1].Scope != ScopePlugin {
		t.Fatalf("repo copy + plugin original: %+v", all)
	}
	if _, err := os.Stat(filepath.Join(RepoDir(cwd), "release.json")); err != nil {
		t.Fatal("repo copy must be on disk")
	}
}

func TestExportFile(t *testing.T) {
	path, err := ExportFile(Def{Name: "ship it", Scope: ScopeRepo, Plugin: "x@y", Steps: []Step{{Name: "a", Provider: "vertex"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })
	if filepath.Base(path) != "ship-it.json" {
		t.Fatalf("file name: %s", path)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "x@y") || strings.Contains(string(data), `"scope"`) || !strings.Contains(string(data), `"name": "ship it"`) {
		t.Fatalf("exported shape must be scope-free:\n%s", data)
	}
}
