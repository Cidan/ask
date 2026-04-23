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

// sessionDir pairs a claude project directory (`~/.claude/projects/<encoded>`)
// with the filesystem cwd that produced it. The cwd is what we pass to
// `exec.Cmd.Dir` on `claude --resume` so claude's own cwd→project-dir mapping
// lands on the right sibling.
type sessionDir struct {
	dir string
	cwd string
}

func claudeSessionPath(sessionID string, cwd string) (string, error) {
	dirs, err := claudeCandidateSessionDirs(cwd)
	if err != nil {
		return "", err
	}
	file := sessionID + ".jsonl"
	for _, d := range dirs {
		p := filepath.Join(d.dir, file)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return filepath.Join(dirs[0].dir, file), nil
}

// claudeCandidateSessionDirs returns the `~/.claude/projects/<encoded>`
// directory for cwd plus every sibling directory that corresponds to a
// `.claude/worktrees/<name>` subdir claude spawned with `--worktree`.
// The main dir is always first and is always returned even if it
// doesn't exist on disk (callers may still want its path for error
// reporting).
//
// Claude encodes project cwd as `ReplaceAll(cwd, "/", "-")` for path
// segments without dots, but replaces `.` with `-` too — so
// `/foo/ask/.claude/worktrees/bar` becomes
// `-foo-ask--claude-worktrees-bar`. We rely on that literal prefix to
// find siblings and reconstruct the worktree path from the remainder.
func claudeCandidateSessionDirs(cwd string) ([]sessionDir, error) {
	if cwd == "" {
		c, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cwd = c
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(home, ".claude", "projects")
	mainName := strings.ReplaceAll(cwd, "/", "-")
	dirs := []sessionDir{{dir: filepath.Join(base, mainName), cwd: cwd}}
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
		wtName := strings.TrimPrefix(e.Name(), prefix)
		dirs = append(dirs, sessionDir{
			dir: filepath.Join(base, e.Name()),
			cwd: filepath.Join(cwd, ".claude", "worktrees", wtName),
		})
	}
	return dirs, nil
}

// loadClaudeHistory replays a claude session jsonl into history entries
// the UI can render. Shared between /resume and the silent-reload path
// when config flags change mid-session.
func loadClaudeHistory(sessionID string, opts HistoryOpts) ([]historyEntry, error) {
	path, err := claudeSessionPath(sessionID, "")
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
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
			if opts.RenderDiffs && !opts.QuietMode {
				result, _ := rec["toolUseResult"].(map[string]any)
				if fp, hunks, ok := parseStructuredPatch(result); ok {
					entries = append(entries, historyEntry{
						kind: histPrerendered,
						text: renderDiffBlock(fp, hunks),
					})
					continue
				}
			}
			if opts.RenderToolOutput && !opts.QuietMode {
				if res, ok := userToolResult(rec); ok {
					entries = append(entries, historyEntry{
						kind: histPrerendered,
						text: renderToolResultBlock(res.output, res.isError),
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
			if opts.RenderToolOutput && !opts.QuietMode {
				for _, call := range assistantToolCalls(rec) {
					entries = append(entries, historyEntry{
						kind: histPrerendered,
						text: renderToolCallBlock(call.name, call.input),
					})
				}
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
			if opts.QuietMode && lastAssistantIdx >= 0 {
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
	return entries, nil
}

// loadClaudeSessions enumerates jsonl sessions under every claude
// project dir that claims the given cwd (main + sibling worktrees).
func loadClaudeSessions(cwd string) ([]sessionEntry, error) {
	dirs, err := claudeCandidateSessionDirs(cwd)
	if err != nil {
		return nil, err
	}
	var sessions []sessionEntry
	seen := make(map[string]struct{})
	var firstErr error
	for _, sd := range dirs {
		entries, err := os.ReadDir(sd.dir)
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
			full := filepath.Join(sd.dir, e.Name())
			seen[id] = struct{}{}
			sessions = append(sessions, sessionEntry{
				id:      id,
				cwd:     sd.cwd,
				preview: readSessionPreview(full),
				modTime: info.ModTime(),
			})
		}
	}
	if len(sessions) == 0 && firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].modTime.After(sessions[j].modTime)
	})
	return sessions, nil
}

// loadHistoryCmd wraps the provider's LoadHistory in a tea.Cmd so
// update.go can schedule the replay asynchronously.
func loadHistoryCmd(p Provider, sessionID string, opts HistoryOpts, silent bool) tea.Cmd {
	return func() tea.Msg {
		entries, err := p.LoadHistory(sessionID, opts)
		return historyLoadedMsg{sessionID: sessionID, entries: entries, err: err, silent: silent}
	}
}

// loadSessionsCmd wraps the provider's ListSessions in a tea.Cmd.
func loadSessionsCmd(p Provider, cwd string) tea.Cmd {
	return func() tea.Msg {
		sessions, err := p.ListSessions(cwd)
		return sessionsLoadedMsg{sessions: sessions, err: err}
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
