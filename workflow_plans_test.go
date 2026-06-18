package main

import (
	"path/filepath"
	"testing"
)

func TestWorkflowPlansDir(t *testing.T) {
	root := initGitRepo(t)

	t.Run("main_checkout", func(t *testing.T) {
		got := workflowPlansDir(root)
		want := filepath.Join(root, "ask", "plans")
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("worktree", func(t *testing.T) {
		path, _, err := createWorktreeAt(root)
		if err != nil {
			t.Fatal(err)
		}
		
		got := workflowPlansDir(path)
		want := filepath.Join(path, "ask", "plans")
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})
}
