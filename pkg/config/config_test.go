package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfig_LoadAndSave(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	cfg := Config{
		Provider: "openai",
		OpenAI: APIProviderConfig{
			APIKey: "sk-test",
			Model:  "gpt-4o",
		},
		UI: UIConfig{
			GateTodosBeforeMutate: true,
			Theme:                 "dracula",
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
	if loaded.OpenAI.APIKey != "sk-test" {
		t.Errorf("expected apiKey 'sk-test', got %q", loaded.OpenAI.APIKey)
	}
	if !loaded.UI.GateTodosBeforeMutate {
		t.Errorf("expected GateTodosBeforeMutate true")
	}
}

func TestConfig_ProjectSettings(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(t.TempDir(), "my-project")
	_ = os.MkdirAll(projectDir, 0755)

	pc := ProjectConfig{
		Issues: IssuesConfig{
			Tracker: "github",
			Repo:    "owner/repo",
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

	if loaded.Issues.Tracker != "github" || loaded.Issues.Repo != "owner/repo" {
		t.Errorf("mismatched issues config: %+v", loaded.Issues)
	}
	if loaded.Workflows.ActiveWorkflow != "ship" {
		t.Errorf("mismatched workflow: %+v", loaded.Workflows)
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
