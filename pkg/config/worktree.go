package config

import (
	"os"
	"path/filepath"
	"strings"
)

const AskLockPrefix = "ask:"

type WorkspaceBackend int

const (
	WorkspaceBackendNone WorkspaceBackend = iota
	WorkspaceBackendGit
	WorkspaceBackendJJ
)

func InGitCheckoutAt(cwd string) bool {
	if cwd == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(cwd, ".git"))
	return err == nil
}

func InJujutsuCheckoutAt(cwd string) bool {
	if cwd == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(cwd, ".jj"))
	return err == nil && info.IsDir()
}

func WorktreeBackendAt(cwd string) WorkspaceBackend {
	if InJujutsuCheckoutAt(cwd) {
		return WorkspaceBackendJJ
	}
	if InGitCheckoutAt(cwd) {
		return WorkspaceBackendGit
	}
	return WorkspaceBackendNone
}

func InGitCheckout() bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	return InGitCheckoutAt(cwd)
}

func WorktreeNameFromCwd(cwd string) string {
	sep := string(os.PathSeparator)
	marker := sep + ".claude" + sep + "worktrees" + sep
	_, rest, ok := strings.Cut(cwd, marker)
	if !ok {
		return ""
	}
	if name, _, ok := strings.Cut(rest, sep); ok {
		return name
	}
	return rest
}

type AskCwdInvalidErr struct {
	Msg          string
	WorktreeName string
}

func ValidateAskCwd(cwd string) AskCwdInvalidErr {
	if cwd == "" {
		return AskCwdInvalidErr{}
	}
	if name := WorktreeNameFromCwd(cwd); name != "" {
		return AskCwdInvalidErr{
			Msg: "Ask's current working directory can not be a subdirectory of a git checkout and must be the checkout's root.\n\n" +
				"To resume a worktree session, you must type /resume and pick the session that previously held the worktree " + name + ".",
			WorktreeName: name,
		}
	}
	if WorktreeBackendAt(cwd) != WorkspaceBackendNone {
		return AskCwdInvalidErr{}
	}
	if findEnclosingCheckout(cwd) != "" {
		return AskCwdInvalidErr{
			Msg: "Ask's current working directory can not be a subdirectory of a git checkout and must be the checkout's root.",
		}
	}
	return AskCwdInvalidErr{}
}

func findEnclosingCheckout(cwd string) string {
	cur := cwd
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
		if WorktreeBackendAt(cur) != WorkspaceBackendNone {
			return cur
		}
	}
}

func EnsureWorktreeGitignore(cwd string) {
	if WorktreeBackendAt(cwd) == WorkspaceBackendNone {
		return
	}
	path := filepath.Join(cwd, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	if GitignoreCoversWorktrees(string(existing)) {
		return
	}
	next := string(existing)
	if len(next) > 0 && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	next += ".claude/worktrees/\n"
	_ = os.WriteFile(path, []byte(next), 0o644)
}

// GitignoreCoversWorktrees reports whether a .gitignore body already ignores
// the .claude/worktrees/ tree (directly or via a broader .claude rule).
func GitignoreCoversWorktrees(contents string) bool {
	for _, raw := range strings.Split(contents, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "!") {
			continue
		}
		l = strings.TrimPrefix(l, "/")
		for changed := true; changed; {
			changed = false
			for _, suf := range []string{"/**", "/*", "/"} {
				if strings.HasSuffix(l, suf) {
					l = strings.TrimSuffix(l, suf)
					changed = true
				}
			}
		}
		if l == ".claude" || l == ".claude/worktrees" {
			return true
		}
	}
	return false
}
