package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunGit executes git with args in dir and returns its combined output.
// Swappable so tests never spawn a subprocess.
var RunGit = func(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func gitClone(ctx context.Context, url, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	_, err := RunGit(ctx, "", "clone", "--quiet", url, dest)
	return err
}

func gitCheckout(ctx context.Context, dir, ref string) error {
	_, err := RunGit(ctx, dir, "checkout", "--quiet", ref)
	return err
}

func gitPull(ctx context.Context, dir string) error {
	_, err := RunGit(ctx, dir, "pull", "--ff-only", "--quiet")
	return err
}

func gitHeadSHA(ctx context.Context, dir string) string {
	out, err := RunGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func gitIsRepo(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func gitHasRemote(ctx context.Context, dir string) bool {
	out, err := RunGit(ctx, dir, "remote")
	return err == nil && strings.TrimSpace(out) != ""
}
