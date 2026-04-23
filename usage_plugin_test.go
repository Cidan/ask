package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractUsagePlugin_HonorsXDGCacheHome(t *testing.T) {
	isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	parent, err := extractUsagePlugin()
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	wantParent := filepath.Join(xdg, "ask", "plugins")
	if parent != wantParent {
		t.Errorf("parent=%q want %q", parent, wantParent)
	}
	if _, err := os.Stat(filepath.Join(parent, "ask-usage")); err != nil {
		t.Errorf("ask-usage dir missing under parent: %v", err)
	}
}

func TestExtractUsagePlugin_Idempotent(t *testing.T) {
	isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	p1, err := extractUsagePlugin()
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	p2, err := extractUsagePlugin()
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if p1 != p2 {
		t.Errorf("paths differ: %q vs %q", p1, p2)
	}
}

func TestExtractUsagePlugin_WritesExpectedFiles(t *testing.T) {
	isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	parent, err := extractUsagePlugin()
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	pluginRoot := filepath.Join(parent, "ask-usage")
	cases := []string{
		filepath.Join(".claude-plugin", "plugin.json"),
		filepath.Join("hooks", "hooks.json"),
		filepath.Join("scripts", "fetch-usage.mjs"),
	}
	for _, rel := range cases {
		full := filepath.Join(pluginRoot, rel)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
}

func TestUsagePluginCacheFile_ReflectsXDGCacheHome(t *testing.T) {
	isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)
	got := usagePluginCacheFile()
	want := filepath.Join(xdg, "ask", "plugins", "ask-usage", ".cache.json")
	if got != want {
		t.Errorf("usagePluginCacheFile()=%q want %q", got, want)
	}
}

func TestFetchUsageMjs_HasNoRefreshEndpoint(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("plugins", "ask-usage", "scripts", "fetch-usage.mjs"))
	if err != nil {
		t.Fatalf("read mjs: %v", err)
	}
	for _, bad := range []string{"platform.claude.com", "/v1/oauth/token", "refresh_token", "grant_type"} {
		if strings.Contains(string(data), bad) {
			t.Errorf("fetch-usage.mjs contains forbidden token %q — plugin must never refresh credentials", bad)
		}
	}
}

// TestFetchUsageMjs_ThrottleSkipsWhenFresh verifies that the plugin's
// parent process exits without spawning a child when the cache file is
// within the 30s throttle window. We assert this indirectly: a fresh
// cache's mtime must be unchanged after a run.
func TestFetchUsageMjs_ThrottleSkipsWhenFresh(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	parent, err := extractUsagePlugin()
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	pluginRoot := filepath.Join(parent, "ask-usage")
	cachePath := filepath.Join(pluginRoot, ".cache.json")
	if err := os.WriteFile(cachePath, []byte(`{"timestamp":0,"fiveHourPercent":0,"weeklyPercent":0}`), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	before, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	script := filepath.Join(pluginRoot, "scripts", "fetch-usage.mjs")
	cmd := exec.Command("node", script)
	cmd.Env = append(os.Environ(),
		"CLAUDE_PLUGIN_ROOT="+pluginRoot,
		// Point at an unroutable host so that if the throttle is broken
		// and a fetch actually runs, we don't leak a real request — it
		// will time out against the loopback.
		"ASK_USAGE_API_HOST=127.0.0.1",
		"HOME="+t.TempDir(),
	)
	if err := cmd.Run(); err != nil {
		t.Fatalf("plugin parent exited nonzero: %v", err)
	}

	// Give any (incorrectly spawned) child a moment to misbehave.
	time.Sleep(200 * time.Millisecond)

	after, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("cache mtime changed, throttle not respected: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

// TestFetchUsageMjs_MissingCredsSilent verifies that running the plugin
// with no credentials anywhere exits cleanly and writes no cache.
func TestFetchUsageMjs_MissingCredsSilent(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	parent, err := extractUsagePlugin()
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	pluginRoot := filepath.Join(parent, "ask-usage")
	cachePath := filepath.Join(pluginRoot, ".cache.json")

	script := filepath.Join(pluginRoot, "scripts", "fetch-usage.mjs")
	homeDir := t.TempDir() // empty, no .credentials.json under .claude/
	cmd := exec.Command("node", script)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"CLAUDE_PLUGIN_ROOT=" + pluginRoot,
		"HOME=" + homeDir,
		// Point at loopback so any accidental fetch times out locally.
		"ASK_USAGE_API_HOST=127.0.0.1",
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("plugin parent exited nonzero: %v", err)
	}

	// Wait for any detached child to finish whatever it's doing.
	time.Sleep(1 * time.Second)

	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf(".cache.json should not exist when creds are missing, stat err=%v", err)
	}
}
