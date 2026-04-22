package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

func sessionPath(sessionID string) (string, error) {
	dirs, err := candidateSessionDirs()
	if err != nil {
		return "", err
	}
	file := sessionID + ".jsonl"
	for _, d := range dirs {
		p := filepath.Join(d, file)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return filepath.Join(dirs[0], file), nil
}

// candidateSessionDirs returns the `~/.claude/projects/<encoded>` directory
// for the current cwd plus every sibling directory that corresponds to a
// `.claude/worktrees/<name>` subdir claude spawned with `--worktree`. The
// main dir is always first and is always returned even if it doesn't exist
// on disk (callers may still want its path for error reporting).
//
// Claude encodes project cwd as `ReplaceAll(cwd, "/", "-")` for path
// segments without dots, but replaces `.` with `-` too — so
// `/foo/ask/.claude/worktrees/bar` becomes `-foo-ask--claude-worktrees-bar`.
// We rely on that literal prefix to find siblings.
func candidateSessionDirs() ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(home, ".claude", "projects")
	mainName := strings.ReplaceAll(cwd, "/", "-")
	dirs := []string{filepath.Join(base, mainName)}
	prefix := mainName + "--claude-worktrees-"
	entries, err := os.ReadDir(base)
	if err != nil {
		return dirs, nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		dirs = append(dirs, filepath.Join(base, e.Name()))
	}
	return dirs, nil
}

func loadHistoryCmd(sessionID string, renderDiffs, quietMode, silent bool) tea.Cmd {
	return func() tea.Msg {
		path, err := sessionPath(sessionID)
		if err != nil {
			return historyLoadedMsg{sessionID: sessionID, err: err, silent: silent}
		}
		f, err := os.Open(path)
		if err != nil {
			return historyLoadedMsg{sessionID: sessionID, err: err, silent: silent}
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<22)
		var entries []historyEntry
		lastAssistantIdx := -1
		for sc.Scan() {
			var rec map[string]any
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			if isMeta, _ := rec["isMeta"].(bool); isMeta {
				continue
			}
			if isSide, _ := rec["isSidechain"].(bool); isSide {
				continue
			}
			t, _ := rec["type"].(string)
			msg, _ := rec["message"].(map[string]any)
			switch t {
			case "user":
				if msg == nil {
					continue
				}
				if s, ok := msg["content"].(string); ok && strings.TrimSpace(s) != "" {
					entries = append(entries, historyEntry{
						kind: histUser,
						text: s,
					})
					lastAssistantIdx = -1
					continue
				}
				if renderDiffs && !quietMode {
					result, _ := rec["toolUseResult"].(map[string]any)
					if fp, hunks, ok := parseStructuredPatch(result); ok {
						entries = append(entries, historyEntry{
							kind: histPrerendered,
							text: renderDiffBlock(fp, hunks),
						})
					}
				}
			case "assistant":
				if msg == nil {
					continue
				}
				arr, ok := msg["content"].([]any)
				if !ok {
					continue
				}
				var b strings.Builder
				for _, item := range arr {
					im, _ := item.(map[string]any)
					if im["type"] != "text" {
						continue
					}
					if txt, _ := im["text"].(string); txt != "" {
						if b.Len() > 0 {
							b.WriteString("\n\n")
						}
						b.WriteString(txt)
					}
				}
				if b.Len() == 0 {
					continue
				}
				if quietMode && lastAssistantIdx >= 0 {
					entries[lastAssistantIdx].text = b.String()
					entries[lastAssistantIdx].rendered = ""
					continue
				}
				entries = append(entries, historyEntry{
					kind: histResponse,
					text: b.String(),
				})
				lastAssistantIdx = len(entries) - 1
			}
		}
		return historyLoadedMsg{sessionID: sessionID, entries: entries, silent: silent}
	}
}

func loadSessionsCmd() tea.Cmd {
	return func() tea.Msg {
		dirs, err := candidateSessionDirs()
		if err != nil {
			return sessionsLoadedMsg{err: err}
		}
		var sessions []sessionEntry
		seen := make(map[string]struct{})
		var firstErr error
		for _, dir := range dirs {
			entries, err := os.ReadDir(dir)
			if err != nil {
				if firstErr == nil && !os.IsNotExist(err) {
					firstErr = err
				}
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
					continue
				}
				id := strings.TrimSuffix(e.Name(), ".jsonl")
				if _, dup := seen[id]; dup {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				full := filepath.Join(dir, e.Name())
				seen[id] = struct{}{}
				sessions = append(sessions, sessionEntry{
					id:      id,
					preview: readSessionPreview(full),
					modTime: info.ModTime(),
				})
			}
		}
		if len(sessions) == 0 && firstErr != nil {
			return sessionsLoadedMsg{err: firstErr}
		}
		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].modTime.After(sessions[j].modTime)
		})
		return sessionsLoadedMsg{sessions: sessions}
	}
}

func readSessionPreview(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if isMeta, _ := rec["isMeta"].(bool); isMeta {
			continue
		}
		if isSide, _ := rec["isSidechain"].(bool); isSide {
			continue
		}
		if t, _ := rec["type"].(string); t == "queue-operation" {
			if op, _ := rec["operation"].(string); op == "enqueue" {
				if c, _ := rec["content"].(string); c != "" {
					return strings.ReplaceAll(c, "\n", " ")
				}
			}
		}
		if msg, ok := rec["message"].(map[string]any); ok {
			if role, _ := msg["role"].(string); role == "user" {
				if s, ok := msg["content"].(string); ok && s != "" {
					return strings.ReplaceAll(s, "\n", " ")
				}
				if arr, ok := msg["content"].([]any); ok {
					for _, item := range arr {
						if im, ok := item.(map[string]any); ok {
							if txt, _ := im["text"].(string); txt != "" {
								return strings.ReplaceAll(txt, "\n", " ")
							}
						}
					}
				}
			}
		}
	}
	return ""
}
