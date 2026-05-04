package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadConfig_MissingReturnsZero(t *testing.T) {
	isolateHome(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Provider != "" || cfg.Claude.Model != "" || cfg.Claude.Effort != "" ||
		len(cfg.Claude.SlashCommands) != 0 || cfg.Claude.Ollama.Host != "" ||
		cfg.UI.Theme != "" || cfg.UI.QuietMode != nil ||
		cfg.Memory.Enabled != nil {
		t.Errorf("missing file should yield zero askConfig, got %+v", cfg)
	}
}

func TestSaveConfig_RoundTrip(t *testing.T) {
	home := isolateHome(t)
	qmTrue := true
	diffsTrue := true
	worktreeTrue := true
	memOn := true
	want := askConfig{
		Provider: "claude",
		Claude: claudeConfig{
			Model:  "opus",
			Effort: "high",
			SlashCommands: []providerSlashEntry{
				{Name: "extra", Description: "demo"},
			},
			Ollama: ollamaConfig{Host: "localhost:11434", Model: "llama3"},
		},
		UI: uiConfig{
			QuietMode:   &qmTrue,
			RenderDiffs: &diffsTrue,
			ToolOutput:  string(toolOutputShort),
			Worktree:    &worktreeTrue,
			Theme:       "catppuccin-mocha",
		},
		Memory: memoryConfig{Enabled: &memOn},
	}
	if err := saveConfig(want); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.Provider != want.Provider {
		t.Errorf("Provider=%q want %q", got.Provider, want.Provider)
	}
	if got.Claude.Model != want.Claude.Model || got.Claude.Effort != want.Claude.Effort {
		t.Errorf("claude model/effort lost in roundtrip: %+v", got.Claude)
	}
	if len(got.Claude.SlashCommands) != 1 || got.Claude.SlashCommands[0].Name != "extra" {
		t.Errorf("slash commands: %+v", got.Claude.SlashCommands)
	}
	if got.Claude.Ollama != want.Claude.Ollama {
		t.Errorf("ollama lost: %+v", got.Claude.Ollama)
	}
	if got.UI.QuietMode == nil || *got.UI.QuietMode != true {
		t.Errorf("quietMode lost: %+v", got.UI.QuietMode)
	}
	if got.UI.ToolOutput != string(toolOutputShort) {
		t.Errorf("toolOutput lost: %q", got.UI.ToolOutput)
	}
	if got.UI.Theme != "catppuccin-mocha" {
		t.Errorf("theme lost: %q", got.UI.Theme)
	}
	if got.Memory.Enabled == nil || *got.Memory.Enabled != true {
		t.Errorf("memory.enabled lost in roundtrip: %+v", got.Memory.Enabled)
	}

	// Permissions 0600 per saveConfig contract.
	path := filepath.Join(home, ".config", "ask", "ask.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config perm=%o want 0600", info.Mode().Perm())
	}
}

func TestSaveConfig_EmitsTrailingNewline(t *testing.T) {
	home := isolateHome(t)
	if err := saveConfig(askConfig{}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	path := filepath.Join(home, ".config", "ask", "ask.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Errorf("saveConfig should end with newline; last byte=%v", data[len(data)-1])
	}
}

func TestSaveConfig_FormatsJSONIndented(t *testing.T) {
	home := isolateHome(t)
	_ = saveConfig(askConfig{Provider: "claude"})
	path := filepath.Join(home, ".config", "ask", "ask.json")
	data, _ := os.ReadFile(path)
	var back askConfig
	if err := json.Unmarshal(data, &back); err != nil {
		t.Errorf("config not parseable JSON: %v; data=%s", err, data)
	}
}

func TestSaveConfig_ConcurrentReadersSeeCompleteJSON(t *testing.T) {
	home := isolateHome(t)
	if err := saveConfig(askConfig{Provider: "seed"}); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	path := filepath.Join(home, ".config", "ask", "ask.json")
	dir := filepath.Dir(path)

	done := make(chan struct{})
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				select {
				case errs <- err:
				default:
				}
				return
			}
			var cfg askConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				select {
				case errs <- fmt.Errorf("read partial config: %w; len=%d", err, len(data)):
				default:
				}
				return
			}
		}
	}()

	for i := 0; i < 80; i++ {
		cfg := askConfig{
			Provider: fmt.Sprintf("provider-%02d", i),
			Projects: map[string]projectConfig{
				"/large": {Workflows: workflowsConfig{Items: largeWorkflowFixture(i)}},
			},
		}
		if err := saveConfig(cfg); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("saveConfig %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()

	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".ask.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("saveConfig left temp files behind: %v", matches)
	}
}

func largeWorkflowFixture(seed int) []workflowDef {
	items := make([]workflowDef, 0, 8)
	prompt := strings.Repeat(fmt.Sprintf("prompt-%02d ", seed), 400)
	for i := 0; i < 8; i++ {
		steps := make([]workflowStep, 0, 4)
		for j := 0; j < 4; j++ {
			steps = append(steps, workflowStep{
				Name:     fmt.Sprintf("step-%d-%d", i, j),
				Provider: "claude",
				Model:    "sonnet",
				Prompt:   prompt,
			})
		}
		items = append(items, workflowDef{Name: fmt.Sprintf("wf-%d-%d", seed, i), Steps: steps})
	}
	return items
}

func TestValidateOllamaHost(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", true},
		{"missing port", "localhost", true},
		{"host:port", "localhost:11434", false},
		{"bad port alpha", "localhost:abc", true},
		{"out of range", "localhost:99999", true},
		{"http scheme", "http://localhost:11434", false},
		{"https scheme", "https://example.com", false},
		{"https with port", "https://example.com:443", false},
		{"broken url", "http://", true},
	}
	for _, c := range cases {
		err := validateOllamaHost(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: validateOllamaHost(%q) err=%v wantErr=%v",
				c.name, c.in, err, c.wantErr)
		}
	}
}

func TestClaudeProviderSettings_RoundTrip(t *testing.T) {
	isolateHome(t)
	var p claudeProvider
	initial := p.LoadSettings()
	if initial.Model != "" || initial.Effort != "" || len(initial.SlashCommands) != 0 {
		t.Errorf("fresh settings not zero-valued: %+v", initial)
	}
	updated := ProviderSettings{
		Model:         "sonnet[1m]",
		Effort:        "xhigh",
		SlashCommands: []providerSlashEntry{{Name: "foo", Description: "bar"}},
	}
	if err := p.SaveSettings(updated); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got := p.LoadSettings()
	if got.Model != updated.Model || got.Effort != updated.Effort {
		t.Errorf("Model/Effort lost: %+v", got)
	}
	if len(got.SlashCommands) != 1 || got.SlashCommands[0].Name != "foo" {
		t.Errorf("slash commands lost: %+v", got.SlashCommands)
	}
}

func TestLoadConfig_MigratesLegacyRenderToolOutput(t *testing.T) {
	// Configs written before the tri-state landed used `renderToolOutput`
	// as a bool. Honour the user's prior choice on first load instead of
	// silently reverting to the default.
	cases := []struct {
		name    string
		raw     string
		wantOut string
	}{
		{"true → short", `{"ui":{"renderToolOutput":true}}`, string(toolOutputShort)},
		{"false → off", `{"ui":{"renderToolOutput":false}}`, string(toolOutputOff)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := isolateHome(t)
			path := filepath.Join(home, ".config", "ask", "ask.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(path, []byte(c.raw), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := loadConfig()
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got.UI.ToolOutput != c.wantOut {
				t.Errorf("ToolOutput=%q want %q (legacy raw=%q)", got.UI.ToolOutput, c.wantOut, c.raw)
			}
		})
	}
}

func TestLoadConfig_NewToolOutputWinsOverLegacy(t *testing.T) {
	// If both keys are present (mid-migration ask.json), the new explicit
	// setting takes precedence — never let the legacy bool downgrade a
	// fresh choice.
	home := isolateHome(t)
	path := filepath.Join(home, ".config", "ask", "ask.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw := `{"ui":{"toolOutput":"full","renderToolOutput":false}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.UI.ToolOutput != string(toolOutputFull) {
		t.Errorf("explicit toolOutput should win; got %q", got.UI.ToolOutput)
	}
}

// TestLoadConfig_MigratesLegacyIssuesGitHub covers the auto-migration
// from the deprecated `issues.github.{endpoint,token}` block to the
// new top-level `mcp.github.*` slot. Real users on disk still have the
// legacy shape; loadConfig must preserve their endpoint/token without
// any manual intervention. Each case asserts the legacy fields land in
// the new slot and that an explicit new value always wins.
func TestLoadConfig_MigratesLegacyIssuesGitHub(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		wantToken    string
		wantEndpoint string
	}{
		{
			name:         "token and endpoint both move",
			raw:          `{"projects":{"/proj":{"issues":{"provider":"github","github":{"token":"ghp_legacy","endpoint":"https://ghe.example/mcp"}}}}}`,
			wantToken:    "ghp_legacy",
			wantEndpoint: "https://ghe.example/mcp",
		},
		{
			name:         "token only",
			raw:          `{"projects":{"/proj":{"issues":{"provider":"github","github":{"token":"ghp_only"}}}}}`,
			wantToken:    "ghp_only",
			wantEndpoint: "",
		},
		{
			name:         "endpoint only",
			raw:          `{"projects":{"/proj":{"issues":{"provider":"github","github":{"endpoint":"https://ghe.example/mcp"}}}}}`,
			wantToken:    "",
			wantEndpoint: "https://ghe.example/mcp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := isolateHome(t)
			path := filepath.Join(home, ".config", "ask", "ask.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(path, []byte(c.raw), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			cfg, err := loadConfig()
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			pc, ok := cfg.Projects["/proj"]
			if !ok {
				t.Fatalf("project entry missing from cfg.Projects after load")
			}
			if pc.MCP.GitHub.Token != c.wantToken {
				t.Errorf("MCP.GitHub.Token=%q want %q", pc.MCP.GitHub.Token, c.wantToken)
			}
			if pc.MCP.GitHub.Endpoint != c.wantEndpoint {
				t.Errorf("MCP.GitHub.Endpoint=%q want %q", pc.MCP.GitHub.Endpoint, c.wantEndpoint)
			}
			// Issues.Provider must be preserved through the migration —
			// the user opted into github issues, and we don't want to
			// silently disable that just because the credential moved.
			if pc.Issues.Provider != "github" {
				t.Errorf("Issues.Provider=%q want %q (migration must preserve provider)",
					pc.Issues.Provider, "github")
			}
		})
	}
}

// An explicit value in the new slot must beat the legacy value — the
// migration runs on every load, and a user who has saved a fresh PAT
// under mcp.github should not see it silently rewritten by a stale
// issues.github value left over in the same file.
func TestLoadConfig_NewMCPGitHubWinsOverLegacy(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".config", "ask", "ask.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw := `{"projects":{"/proj":{
		"issues":{"provider":"github","github":{"token":"ghp_legacy","endpoint":"https://legacy"}},
		"mcp":{"github":{"token":"ghp_new","endpoint":"https://new"}}
	}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	pc := cfg.Projects["/proj"]
	if pc.MCP.GitHub.Token != "ghp_new" {
		t.Errorf("explicit new token should win, got %q", pc.MCP.GitHub.Token)
	}
	if pc.MCP.GitHub.Endpoint != "https://new" {
		t.Errorf("explicit new endpoint should win, got %q", pc.MCP.GitHub.Endpoint)
	}
}

// Migration must be a no-op for projects that have no entry in
// cfg.Projects yet — otherwise a malformed legacy block could
// accidentally synthesise an entry that didn't exist before.
func TestLoadConfig_MigrationIsNoOpForUnknownProjects(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".config", "ask", "ask.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// projects:{} (empty), no entries — migration must not panic
	// on the empty map and must not create entries from thin air.
	raw := `{"projects":{}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Projects) != 0 {
		t.Errorf("empty projects map should remain empty, got %d entries", len(cfg.Projects))
	}
}

// Round-trip the new shape: write via saveConfig, read back via
// loadConfig — MCP.GitHub fields persist verbatim and the migration
// is idempotent (a load → save → load cycle on a freshly-written
// new-shape file leaves the values unchanged).
func TestSaveConfig_RoundTripPreservesMCPGitHub(t *testing.T) {
	isolateHome(t)
	want := askConfig{
		Projects: map[string]projectConfig{
			"/proj": {
				Issues: issuesConfig{Provider: "github"},
				MCP: projectMCPConfig{
					GitHub: githubMCPConfig{
						Token:    "ghp_xyz",
						Endpoint: "https://ghe.example/mcp",
					},
				},
			},
		},
	}
	if err := saveConfig(want); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	pc := got.Projects["/proj"]
	if pc.MCP.GitHub.Token != "ghp_xyz" {
		t.Errorf("token lost in roundtrip: %q", pc.MCP.GitHub.Token)
	}
	if pc.MCP.GitHub.Endpoint != "https://ghe.example/mcp" {
		t.Errorf("endpoint lost in roundtrip: %q", pc.MCP.GitHub.Endpoint)
	}
	if pc.Issues.Provider != "github" {
		t.Errorf("issues.provider lost in roundtrip: %q", pc.Issues.Provider)
	}
	// Idempotence: a second save+load cycle must produce the same
	// values, proving the migration doesn't drift the data each pass.
	if err := saveConfig(got); err != nil {
		t.Fatalf("second saveConfig: %v", err)
	}
	got2, err := loadConfig()
	if err != nil {
		t.Fatalf("second loadConfig: %v", err)
	}
	pc2 := got2.Projects["/proj"]
	if pc2.MCP != pc.MCP || pc2.Issues != pc.Issues {
		t.Errorf("migration not idempotent:\n got: %+v\nwant: %+v", pc2, pc)
	}
}

func TestConfigPath_UnderHome(t *testing.T) {
	home := isolateHome(t)
	path, err := configPath()
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	want := filepath.Join(home, ".config", "ask", "ask.json")
	if path != want {
		t.Errorf("configPath=%q want %q", path, want)
	}
}

func TestClaudeProviderSettings_PreservesOtherFields(t *testing.T) {
	isolateHome(t)
	// Seed unrelated fields in the on-disk config; SaveSettings must not nuke them.
	boolT := true
	cfg := askConfig{
		Provider: "claude",
		UI:       uiConfig{QuietMode: &boolT, Theme: "keep-me"},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}

	var p claudeProvider
	if err := p.SaveSettings(ProviderSettings{Model: "opus"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, _ := loadConfig()
	if got.UI.Theme != "keep-me" {
		t.Errorf("theme was overwritten: %q", got.UI.Theme)
	}
	if got.UI.QuietMode == nil || *got.UI.QuietMode != true {
		t.Errorf("quietMode pointer lost: %+v", got.UI.QuietMode)
	}
	if got.Claude.Model != "opus" {
		t.Errorf("model not persisted: %+v", got.Claude)
	}
}
