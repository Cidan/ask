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
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(cwd, "/", "-"))
	return filepath.Join(dir, sessionID+".jsonl"), nil
}

func loadHistoryCmd(sessionID string, renderDiffs bool) tea.Cmd {
	return func() tea.Msg {
		path, err := sessionPath(sessionID)
		if err != nil {
			return historyLoadedMsg{sessionID: sessionID, err: err}
		}
		f, err := os.Open(path)
		if err != nil {
			return historyLoadedMsg{sessionID: sessionID, err: err}
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<22)
		var entries []historyEntry
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
					continue
				}
				if renderDiffs {
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
				entries = append(entries, historyEntry{
					kind: histResponse,
					text: b.String(),
				})
			}
		}
		return historyLoadedMsg{sessionID: sessionID, entries: entries}
	}
}

func loadSessionsCmd() tea.Cmd {
	return func() tea.Msg {
		cwd, err := os.Getwd()
		if err != nil {
			return sessionsLoadedMsg{err: err}
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return sessionsLoadedMsg{err: err}
		}
		dir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(cwd, "/", "-"))
		entries, err := os.ReadDir(dir)
		if err != nil {
			return sessionsLoadedMsg{err: err}
		}
		var sessions []sessionEntry
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			full := filepath.Join(dir, e.Name())
			sessions = append(sessions, sessionEntry{
				id:      strings.TrimSuffix(e.Name(), ".jsonl"),
				preview: readSessionPreview(full),
				modTime: info.ModTime(),
			})
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
