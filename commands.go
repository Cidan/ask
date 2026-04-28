package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

func (m model) runPathCommand(cmd, target string) (tea.Model, tea.Cmd) {
	switch cmd {
	case "cd":
		return m.doCd(target)
	case "ls":
		return m.doLs(target)
	case "/add-dir":
		return m.doAddDir(target)
	}
	return m, nil
}

func (m model) doCd(target string) (tea.Model, tea.Cmd) {
	abs, err := resolveDir(target)
	if err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("cd: " + err.Error())))
		return m, nil
	}
	if m.cwd == abs {
		return m, nil
	}
	if err := os.Chdir(abs); err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("cd: " + err.Error())))
		return m, nil
	}
	m.cwd = abs
	m.killProc()
	m.sessionID = ""
	m.sessionMinted = false
	m.history = nil
	m.refreshPrompt()
	m.appendHistory(outputStyle.Render(
		promptStyle.Render("✓ cd "+abs) + "  " + dimStyle.Render("(session cleared)"),
	))
	return m, nil
}

// doAddDir registers an additional directory with the active provider.
// Kills the running proc so the next user turn relaunches with the
// new dir in argv (claude --add-dir, codex writable_roots). The
// session id and worktree are intentionally preserved so the relaunch
// resumes — not restarts — the conversation.
func (m model) doAddDir(target string) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(target) == "" {
		m.appendHistory(outputStyle.Render(errStyle.Render(
			"/add-dir: missing directory argument")))
		return m, nil
	}
	abs, err := resolveDir(target)
	if err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render(
			"/add-dir: " + err.Error())))
		return m, nil
	}
	if slices.Contains(m.addedDirs, abs) {
		m.appendHistory(outputStyle.Render(dimStyle.Render(
			"/add-dir: " + abs + " already registered")))
		return m, nil
	}
	m.addedDirs = append(m.addedDirs, abs)
	m.killProc()
	m.persistAddedDirs()
	m.appendHistory(outputStyle.Render(
		promptStyle.Render("✓ /add-dir " + abs) + "  " +
			dimStyle.Render("(applies on next message)"),
	))
	return m, nil
}

func (m model) doLs(target string) (tea.Model, tea.Cmd) {
	resolved := resolvePath(target)

	var paths []string
	if hasGlob(resolved) {
		matches, err := filepath.Glob(resolved)
		if err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render("ls: " + err.Error())))
			return m, nil
		}
		if len(matches) == 0 {
			m.appendHistory(outputStyle.Render(dimStyle.Render("ls: no matches for " + target)))
			return m, nil
		}
		paths = matches
	} else {
		info, err := os.Lstat(resolved)
		if err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render("ls: " + err.Error())))
			return m, nil
		}
		if info.IsDir() {
			entries, err := os.ReadDir(resolved)
			if err != nil {
				m.appendHistory(outputStyle.Render(errStyle.Render("ls: " + err.Error())))
				return m, nil
			}
			for _, e := range entries {
				paths = append(paths, filepath.Join(resolved, e.Name()))
			}
		} else {
			paths = []string{resolved}
		}
	}

	out := renderLsOutput(target, paths)
	m.appendHistory(outputStyle.Render(out))
	return m, nil
}

type lsRow struct {
	name  string
	info  os.FileInfo
	isDir bool
	isExe bool
	isLnk bool
	link  string
}

func renderLsOutput(target string, paths []string) string {
	rows := make([]lsRow, 0, len(paths))
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		mode := info.Mode()
		row := lsRow{
			name:  filepath.Base(p),
			info:  info,
			isDir: info.IsDir(),
			isExe: !info.IsDir() && mode&0o111 != 0,
			isLnk: mode&os.ModeSymlink != 0,
		}
		if row.isLnk {
			if t, err := os.Readlink(p); err == nil {
				row.link = t
			}
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].isDir != rows[j].isDir {
			return rows[i].isDir
		}
		return rows[i].name < rows[j].name
	})

	var b strings.Builder
	header := promptStyle.Render(target) + "  " + dimStyle.Render(fmt.Sprintf("(%d items)", len(rows)))
	b.WriteString(header)
	b.WriteString("\n")

	if len(rows) == 0 {
		b.WriteString(dimStyle.Render("  (empty)"))
		return b.String()
	}

	sizeW, timeW := 0, 0
	for _, r := range rows {
		if w := lipgloss.Width(formatLsSize(r)); w > sizeW {
			sizeW = w
		}
		if w := lipgloss.Width(formatLsTime(r.info.ModTime())); w > timeW {
			timeW = w
		}
	}

	for _, r := range rows {
		line := fmt.Sprintf("%s  %s  %s  %s",
			dimStyle.Render(r.info.Mode().String()),
			padRight(formatLsSize(r), sizeW),
			padRight(dimStyle.Render(formatLsTime(r.info.ModTime())), timeW),
			formatLsName(r),
		)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatLsName(r lsRow) string {
	switch {
	case r.isLnk:
		return fileSymlinkStyle.Render(r.name) +
			dimStyle.Render(" → "+r.link)
	case r.isDir:
		return cwdStyle.Render(r.name + "/")
	case r.isExe:
		return fileExeStyle.Render(r.name + "*")
	default:
		return r.name
	}
}

func formatLsSize(r lsRow) string {
	if r.isDir {
		return "-"
	}
	return humanBytes(r.info.Size())
}

func formatLsTime(t time.Time) string {
	return humanDuration(time.Since(t)) + " ago"
}

func resolveDir(p string) (string, error) {
	if p == "" {
		return os.UserHomeDir()
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			p = home
		} else {
			p = filepath.Join(home, p[2:])
		}
	}
	return filepath.Abs(filepath.Clean(p))
}
