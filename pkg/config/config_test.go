package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfig_LoadAndSave(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	skipPermsTrue := true
	quietTrue := true
	blinkFalse := false
	diffsTrue := true
	maxRetries := 5
	initialDelay := 1500
	backoffFactor := 2.5

	cfg := Config{
		Provider: "openai",
		Effort:   "medium",
		Providers: map[string]ProviderConfig{
			"openai": {Model: "gpt-4o", Fields: map[string]string{"apiKey": "sk-test"}},
		},
		UI: UIConfig{
			SkipAllPermissions: &skipPermsTrue,
			QuietMode:          &quietTrue,
			CursorBlink:        &blinkFalse,
			RenderDiffs:        &diffsTrue,
			ToolOutput:         "short",
			Theme:              "dracula",
			Retry: &RetryUIConfig{
				MaxRetries:     &maxRetries,
				InitialDelayMs: &initialDelay,
				BackoffFactor:  &backoffFactor,
			},
		},
	}

	if err := Save(cfg); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if loaded.Provider != "openai" {
		t.Errorf("expected provider 'openai', got %q", loaded.Provider)
	}
	if loaded.Effort != "medium" {
		t.Errorf("expected effort 'medium', got %q", loaded.Effort)
	}
	if pc := loaded.ProviderConfig("openai"); pc.Field("apiKey") != "sk-test" || pc.Model != "gpt-4o" {
		t.Errorf("provider block lost in round trip: %+v", pc)
	}
	if loaded.UI.SkipAllPermissions == nil || !*loaded.UI.SkipAllPermissions {
		t.Errorf("expected SkipAllPermissions true")
	}
	if loaded.UI.QuietMode == nil || !*loaded.UI.QuietMode {
		t.Errorf("expected QuietMode true")
	}
	if loaded.UI.CursorBlink == nil || *loaded.UI.CursorBlink {
		t.Errorf("expected CursorBlink false")
	}
	if loaded.UI.RenderDiffs == nil || !*loaded.UI.RenderDiffs {
		t.Errorf("expected RenderDiffs true")
	}
	if loaded.UI.ToolOutput != "short" {
		t.Errorf("expected ToolOutput 'short', got %q", loaded.UI.ToolOutput)
	}
	if loaded.UI.Theme != "dracula" {
		t.Errorf("expected Theme 'dracula', got %q", loaded.UI.Theme)
	}
	if loaded.UI.Retry == nil || loaded.UI.Retry.MaxRetries == nil || *loaded.UI.Retry.MaxRetries != 5 {
		t.Errorf("expected Retry.MaxRetries 5, got %+v", loaded.UI.Retry)
	}
}

func TestConfig_ProjectSettings(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(t.TempDir(), "my-project")
	_ = os.MkdirAll(projectDir, 0755)

	pc := ProjectConfig{
		Issues: IssuesConfig{
			Provider: "github",
			Tracker:  "github",
			Repo:     "owner/repo",
		},
		MCP: ProjectMCPConfig{
			GitHub: GitHubMCPConfig{
				Token: "ghp_secret_token",
			},
			Linear: LinearMCPConfig{
				Token:   "lin_api_key",
				TeamKey: "ENG",
			},
		},
		Workflows: WorkflowsConfig{
			ActiveWorkflow: "ship",
		},
	}

	if err := SaveProject(projectDir, pc); err != nil {
		t.Fatalf("failed to save project config: %v", err)
	}

	loaded, err := LoadProject(projectDir)
	if err != nil {
		t.Fatalf("failed to load project config: %v", err)
	}

	if loaded.Issues.Provider != "github" || loaded.Issues.Repo != "owner/repo" {
		t.Errorf("mismatched issues config: %+v", loaded.Issues)
	}
	if loaded.MCP.GitHub.Token != "ghp_secret_token" {
		t.Errorf("mismatched github token: %+v", loaded.MCP.GitHub)
	}
	if loaded.MCP.Linear.Token != "lin_api_key" || loaded.MCP.Linear.TeamKey != "ENG" {
		t.Errorf("mismatched linear config: %+v", loaded.MCP.Linear)
	}
	if loaded.Workflows.ActiveWorkflow != "ship" {
		t.Errorf("mismatched workflow: %+v", loaded.Workflows)
	}
}

func TestWorktreeEnabled_ProjectOverridesGlobal(t *testing.T) {
	on, off := true, false
	projectDir := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755)

	withProject := func(global, project *bool) Config {
		cfg := Config{UI: UIConfig{Worktree: global}}
		if project != nil {
			cfg = UpsertProjectConfig(cfg, projectDir, ProjectConfig{Worktree: project})
		}
		return cfg
	}
	cases := []struct {
		name            string
		global, project *bool
		want            bool
	}{
		{"both unset", nil, nil, false},
		{"global on, no override", &on, nil, true},
		{"global off, no override", &off, nil, false},
		{"global on, project off", &on, &off, false},
		{"global off, project on", &off, &on, true},
		{"global unset, project on", nil, &on, true},
		{"global unset, project off", nil, &off, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WorktreeEnabled(withProject(tc.global, tc.project), projectDir); got != tc.want {
				t.Fatalf("WorktreeEnabled=%v want %v", got, tc.want)
			}
		})
	}

	// A worktree cwd resolves to the main checkout's project key, so
	// the override is honoured from inside .claude/worktrees/<name>.
	wt := filepath.Join(projectDir, ".claude", "worktrees", "swift-fox")
	_ = os.MkdirAll(wt, 0o755)
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: ../../../.git/worktrees/swift-fox\n"), 0o644)
	if !WorktreeEnabled(withProject(&off, &on), wt) {
		t.Fatal("project override should apply from inside a worktree cwd")
	}

	// Empty cwd has no project block: global wins.
	if !WorktreeEnabled(withProject(&on, &off), "") {
		t.Fatal("empty cwd should fall back to the global flag")
	}
}

func TestProjectConfig_WorktreeOverrideRoundTripsAndPrunes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(projectDir, 0o755)

	off := false
	if IsProjectConfigEmpty(ProjectConfig{Worktree: &off}) {
		t.Fatal("a worktree override must keep the project block alive")
	}
	if err := SaveProject(projectDir, ProjectConfig{Worktree: &off}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	loaded, err := LoadProject(projectDir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if loaded.Worktree == nil || *loaded.Worktree {
		t.Fatalf("override did not round-trip: %+v", loaded.Worktree)
	}

	if err := SaveProject(projectDir, ProjectConfig{}); err != nil {
		t.Fatalf("SaveProject clear: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Projects[ProjectKey(projectDir)]; ok {
		t.Fatal("clearing the override should prune the empty project block")
	}
}

func TestWorkflowSession_UnmarshalBothCasings(t *testing.T) {
	camelJSON := []byte(`{
		"workflow": "ship",
		"stepIndex": 2,
		"status": "done",
		"startedAt": "2026-08-15T12:00:00Z",
		"updatedAt": "2026-08-15T12:05:00Z"
	}`)

	var s1 WorkflowSession
	if err := json.Unmarshal(camelJSON, &s1); err != nil {
		t.Fatalf("unmarshal camelCase: %v", err)
	}
	if s1.Workflow != "ship" || s1.StepIndex != 2 || s1.Status != "done" {
		t.Errorf("unexpected camelCase parsed: %+v", s1)
	}

	snakeJSON := []byte(`{
		"workflow": "review",
		"step_index": 3,
		"status": "failed",
		"started_at": "2026-08-15T12:00:00Z",
		"updated_at": "2026-08-15T12:05:00Z"
	}`)

	var s2 WorkflowSession
	if err := json.Unmarshal(snakeJSON, &s2); err != nil {
		t.Fatalf("unmarshal snake_case: %v", err)
	}
	if s2.Workflow != "review" || s2.StepIndex != 3 || s2.Status != "failed" {
		t.Errorf("unexpected snake_case parsed: %+v", s2)
	}
}

func TestConfig_MigrateLegacyEffort(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Write a config with provider-specific effort but no global effort
	legacyJSON := `{
		"provider": "openrouter",
		"openrouter": {
			"api_key": "sk-test",
			"effort": "high"
		}
	}`

	configDir := filepath.Join(tmpHome, ".config", "ask")
	_ = os.MkdirAll(configDir, 0755)
	err := os.WriteFile(filepath.Join(configDir, "ask.json"), []byte(legacyJSON), 0644)
	if err != nil {
		t.Fatalf("failed to write legacy config: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if loaded.Effort != "high" {
		t.Errorf("expected global effort to be migrated to 'high', got %q", loaded.Effort)
	}
}

func TestConfig_AgentRetryOptions(t *testing.T) {
	// Defaults
	mr, id, bf := AgentRetryOptions(Config{})
	if mr != AgentDefaultMaxRetries || id != time.Duration(AgentDefaultInitialDelayMs)*time.Millisecond || bf != AgentDefaultBackoffFactor {
		t.Errorf("expected defaults, got mr=%d id=%v bf=%f", mr, id, bf)
	}

	// Overridden
	maxRetries := 2
	initialDelay := 500
	backoffFactor := 3.0
	mr, id, bf = AgentRetryOptions(Config{
		UI: UIConfig{
			Retry: &RetryUIConfig{
				MaxRetries:     &maxRetries,
				InitialDelayMs: &initialDelay,
				BackoffFactor:  &backoffFactor,
			},
		},
	})
	if mr != 2 || id != 500*time.Millisecond || bf != 3.0 {
		t.Errorf("expected overridden values, got mr=%d id=%v bf=%f", mr, id, bf)
	}
}

func TestWorktree_ValidateAskCwd(t *testing.T) {
	// Empty directory outside git
	err := ValidateAskCwd(t.TempDir())
	if err.Msg != "" {
		t.Errorf("expected empty message for non-git directory, got: %s", err.Msg)
	}

	// Inside worktrees
	worktreeCwd := filepath.Join(t.TempDir(), ".claude", "worktrees", "swift-running-fox")
	err = ValidateAskCwd(worktreeCwd)
	if err.WorktreeName != "swift-running-fox" {
		t.Errorf("expected worktree name 'swift-running-fox', got %q", err.WorktreeName)
	}
}
