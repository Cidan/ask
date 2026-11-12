package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// worktreeLinksChangedMsg is broadcast after the link key moves a link,
// so every tab re-checks whether a link points at its worktree.
type worktreeLinksChangedMsg struct{}

// linkTargetPaths returns the project's link targets as absolute paths.
func linkTargetPaths(cfg askConfig, root string) []string {
	var out []string
	for _, p := range loadProjectConfig(cfg, root).Links {
		out = append(out, resolveLinkTarget(p, root))
	}
	return out
}

// resolveLinkTarget is a configured target as an absolute path: ~ is the
// home directory and a relative target is taken from the project root.
func resolveLinkTarget(p, root string) string {
	p, _ = expandTilde(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return filepath.Clean(p)
}

// acceptLinkTarget is the /config check on a target being added: it cleans
// the path and refuses one the link key could never manage — something
// real (not a symlink) already at it, or a folder that does not exist.
func acceptLinkTarget(v, root string) (string, error) {
	v = filepath.Clean(v)
	p := resolveLinkTarget(v, root)
	if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink == 0 {
		what := "file"
		if info.IsDir() {
			what = "directory"
		}
		return "", fmt.Errorf("%s is a real %s, not a symlink; move it aside first", filepath.Base(p), what)
	}
	if info, err := os.Stat(filepath.Dir(p)); err != nil || !info.IsDir() {
		return "", fmt.Errorf("folder %s does not exist", filepath.Base(filepath.Dir(p)))
	}
	return v, nil
}

// linkTargetMatches completes a target being typed in /config: the
// directories and symlinks in its folder, each symlink with where it points.
func linkTargetMatches(query, root string) []pathMatch {
	found, _ := completeDirEntries(query, root, func(e fs.DirEntry) bool {
		return e.IsDir() || e.Type()&fs.ModeSymlink != 0
	})
	out := make([]pathMatch, 0, len(found))
	for _, c := range found {
		mt := pathMatch{path: c.path, dir: c.entry.IsDir()}
		if dest, ok := linkDest(c.onDisk); ok {
			mt.dest = tildePath(dest)
		}
		out = append(out, mt)
	}
	return out
}

// linkTargetsSummary is the Link targets row's value: the targets' names.
func linkTargetsSummary(links []string) string {
	if len(links) == 0 {
		return "(none)"
	}
	return baseNames(links)
}

// linkDest returns target's symlink destination as an absolute path; ok is
// false when target is not a symlink.
func linkDest(target string) (string, bool) {
	dest, err := os.Readlink(target)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(target), dest)
	}
	return filepath.Clean(dest), true
}

func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(os.PathSeparator))
}

// linkedWorktreeOf names the worktree of the project at root that dest
// lies in, or "" when dest is not inside one. dest need not exist.
func linkedWorktreeOf(dest, root string) string {
	name := worktreeNameFromCwd(dest)
	if name == "" {
		return ""
	}
	roots := []string{root}
	if r, err := filepath.EvalSymlinks(root); err == nil && r != root {
		roots = append(roots, r)
	}
	for _, r := range roots {
		if pathWithin(dest, worktreePath(r, name)) {
			return name
		}
	}
	return ""
}

// swapLink points target at dest by renaming a fresh symlink over it, so
// nothing reading target ever finds it missing. rename(2) replaces a
// symlink in place and refuses to replace a directory.
func swapLink(target, dest string) error {
	tmp := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".ask-link-"+randomAlphanum(8))
	if err := os.Symlink(dest, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// linkToggle is what toggleLinks did: where the links point now, the
// targets it moved, and the targets it left alone, each with the reason.
type linkToggle struct {
	dest    string
	moved   []string
	skipped []string
}

// toggleLinks points every target at dir, or back at root when all of
// them already point at dir. It only replaces symlinks and creates
// missing paths; anything else at a target is reported and left alone.
func toggleLinks(targets []string, dir, root string) linkToggle {
	var res linkToggle
	var usable []string
	for _, t := range targets {
		info, err := os.Lstat(t)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			usable = append(usable, t)
		case err != nil:
			res.skipped = append(res.skipped, filepath.Base(t)+": "+err.Error())
		case info.Mode()&os.ModeSymlink == 0:
			res.skipped = append(res.skipped, filepath.Base(t)+" is not a symlink, left alone")
		default:
			usable = append(usable, t)
		}
	}
	res.dest = dir
	if len(usable) > 0 && allLinkedTo(usable, dir) {
		res.dest = root
	}
	for _, t := range usable {
		if d, ok := linkDest(t); ok && samePathOrResolved(d, res.dest) {
			continue
		}
		if err := swapLink(t, res.dest); err != nil {
			res.skipped = append(res.skipped, filepath.Base(t)+": "+err.Error())
			continue
		}
		res.moved = append(res.moved, t)
	}
	return res
}

func allLinkedTo(targets []string, dir string) bool {
	for _, t := range targets {
		d, ok := linkDest(t)
		if !ok || !samePathOrResolved(d, dir) {
			return false
		}
	}
	return true
}

func baseNames(paths []string) string {
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	return strings.Join(names, ", ")
}

// relinkOrphanedLinks points every link target of the project at root that
// names one of its worktrees no longer on disk back at root. Links into a
// worktree that still exists — another tab's, another ask's, one prune
// kept because it is dirty — are left alone.
func relinkOrphanedLinks(root string) {
	cfg, err := loadConfig()
	if err != nil {
		debugLog("worktree link relink: load config: %v", err)
		return
	}
	for _, t := range linkTargetPaths(cfg, root) {
		dest, ok := linkDest(t)
		if !ok {
			continue
		}
		name := linkedWorktreeOf(dest, root)
		if name == "" {
			continue
		}
		if _, err := os.Lstat(worktreePath(root, name)); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := swapLink(t, root); err != nil {
			debugLog("worktree link relink %s: %v", t, err)
			continue
		}
		debugLog("worktree link %s → %s (worktree %s is gone)", t, root, name)
	}
}

// toggleWorktreeLink is the link key: it points the project's link targets
// at the tab's worktree (the project root when the tab has none), or back
// at the project root when they all point there already.
func (m model) toggleWorktreeLink() (model, tea.Cmd) {
	cfg, _ := loadConfig()
	targets := linkTargetPaths(cfg, m.cwd)
	if len(targets) == 0 {
		return m, m.toast.show("no link targets for this project · set them in /config → Project")
	}
	dir := m.cwd
	if m.worktreeName != "" {
		dir = worktreePath(m.cwd, m.worktreeName)
		if _, err := os.Stat(dir); err != nil {
			return m, m.toast.show("worktree " + m.worktreeName + " no longer exists")
		}
	}
	res := toggleLinks(targets, dir, m.cwd)
	label := "project root"
	if res.dest != m.cwd {
		label = m.worktreeName
	}
	var parts []string
	switch {
	case len(res.moved) > 0:
		parts = append(parts, "linked "+baseNames(res.moved)+" → "+label)
	case len(res.skipped) == 0:
		parts = append(parts, baseNames(targets)+" already → "+label)
	}
	for _, s := range res.skipped {
		parts = append(parts, "✗ "+s)
	}
	m.refreshLinked()
	cmds := []tea.Cmd{m.toast.show(strings.Join(parts, " · "))}
	if len(res.moved) > 0 {
		cmds = append(cmds, func() tea.Msg { return worktreeLinksChangedMsg{} })
	}
	return m, tea.Batch(cmds...)
}

// refreshLinked re-checks whether one of the project's link targets points
// at the tab's worktree.
func (m *model) refreshLinked() {
	linked := ""
	if m.worktreeName != "" {
		cfg, _ := loadConfig()
		for _, t := range linkTargetPaths(cfg, m.cwd) {
			if d, ok := linkDest(t); ok && linkedWorktreeOf(d, m.cwd) == m.worktreeName {
				linked = m.worktreeName
				break
			}
		}
	}
	if linked != m.linkedWorktree {
		m.linkedWorktree = linked
		m.lastContentFP = ""
	}
}
