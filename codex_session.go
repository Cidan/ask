package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Codex session storage lives at:
//   ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread-id>.jsonl
// Each file starts with a {"type":"session_meta","payload":{...}}
// line carrying the thread id and the cwd the session ran in.
// Subsequent lines are the items the wire protocol emits — the ones
// we care about for /resume are {"type":"response_item","payload":
// {"type":"message","role":"user|assistant|developer","content":[...]}}.

// codexRolloutItem mirrors a minimal slice of a codex response_item
// payload — just what history replay and preview lookup need.
type codexRolloutItem struct {
	Type    string             `json:"type"`
	Role    string             `json:"role"`
	Content []codexContentItem `json:"content"`
}

type codexContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// firstText returns the first human-visible text chunk of a response
// item. Codex uses `input_text` for user-authored content and
// `output_text` for the assistant's reply.
func (it codexRolloutItem) firstText() string {
	for _, c := range it.Content {
		switch c.Type {
		case "input_text", "output_text":
			if c.Text != "" {
				return c.Text
			}
		}
	}
	return ""
}

// isCodexEnvironmentText skips the XML-tagged preludes codex injects
// at the start of every thread: `<environment_context>`, permission
// policy blobs, and similar hook-inserted wrappers. They aren't
// actual user turns.
func isCodexEnvironmentText(s string) bool {
	trimmed := strings.TrimLeft(s, " \t\n")
	return strings.HasPrefix(trimmed, "<environment_context>") ||
		strings.HasPrefix(trimmed, "<permissions") ||
		strings.HasPrefix(trimmed, "<hook")
}

// codexAcceptedCwds returns the set of working directories whose
// sessions count as "belonging to this tab": cwd itself plus every
// `.claude/worktrees/<name>` sibling. Mirrors how claude resume
// enumerates main + worktree dirs.
func codexAcceptedCwds(cwd string) map[string]struct{} {
	accept := map[string]struct{}{cwd: {}}
	entries, err := os.ReadDir(filepath.Join(cwd, ".claude", "worktrees"))
	if err != nil {
		return accept
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		accept[filepath.Join(cwd, ".claude", "worktrees", e.Name())] = struct{}{}
	}
	return accept
}

// loadCodexSessions scans ~/.codex/sessions for rollout jsonl files
// whose session_meta cwd matches the tab's cwd (or a worktree
// sibling). Results are sorted newest first so /resume opens on the
// most recently-touched session. Parsing only the first line of each
// rollout keeps the scan cheap even when there are hundreds of files.
func loadCodexSessions(cwd string) ([]sessionEntry, error) {
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
	root := filepath.Join(home, ".codex", "sessions")
	accept := codexAcceptedCwds(cwd)
	var sessions []sessionEntry
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// Skip unreadable dirs rather than fail the whole scan —
			// a stale permission on one day shouldn't hide the rest.
			if d != nil && d.IsDir() {
				return nil
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		entry, ok := readCodexSessionEntry(p)
		if !ok {
			return nil
		}
		if _, ok := accept[entry.cwd]; !ok {
			return nil
		}
		sessions = append(sessions, entry)
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return sessions, walkErr
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].modTime.After(sessions[j].modTime)
	})
	return sessions, nil
}

// readCodexSessionEntry pulls id+cwd from session_meta and the first
// real user message from the rollout as a preview. Returns ok=false
// when the file is empty or missing the meta line.
func readCodexSessionEntry(path string) (sessionEntry, bool) {
	f, err := os.Open(path)
	if err != nil {
		return sessionEntry{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sessionEntry{}, false
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	var entry sessionEntry
	var metaSeen bool
	for sc.Scan() {
		var rec struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		switch rec.Type {
		case "session_meta":
			var meta struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(rec.Payload, &meta) != nil || meta.ID == "" {
				continue
			}
			entry.id = meta.ID
			entry.cwd = meta.Cwd
			entry.modTime = info.ModTime()
			metaSeen = true
		case "response_item":
			if !metaSeen {
				continue
			}
			var item codexRolloutItem
			if json.Unmarshal(rec.Payload, &item) != nil {
				continue
			}
			if item.Type != "message" || item.Role != "user" {
				continue
			}
			txt := item.firstText()
			if txt == "" || isCodexEnvironmentText(txt) {
				continue
			}
			entry.preview = strings.ReplaceAll(txt, "\n", " ")
			return entry, true
		}
	}
	return entry, metaSeen
}

// loadCodexHistory parses a rollout into the same historyEntry shape
// claude uses. User preludes and developer-role items are skipped so
// the replay looks like a conversation.
func loadCodexHistory(sessionID string, _ HistoryOpts) ([]historyEntry, error) {
	path, err := findCodexRollout(sessionID)
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
	for sc.Scan() {
		var rec struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Type != "response_item" {
			continue
		}
		var item codexRolloutItem
		if json.Unmarshal(rec.Payload, &item) != nil {
			continue
		}
		if item.Type != "message" {
			continue
		}
		txt := item.firstText()
		if txt == "" {
			continue
		}
		switch item.Role {
		case "user":
			if isCodexEnvironmentText(txt) {
				continue
			}
			entries = append(entries, historyEntry{kind: histUser, text: txt})
		case "assistant":
			entries = append(entries, historyEntry{kind: histResponse, text: txt})
		}
	}
	return entries, nil
}

// findCodexRollout locates the jsonl for a given thread id. Codex
// names files `rollout-<ts>-<thread-id>.jsonl`, so a suffix match is
// both exact and cheap.
func findCodexRollout(sessionID string) (string, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, "/\\\x00") {
		return "", fmt.Errorf("codex session id rejected: %q", sessionID)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, ".codex", "sessions")
	suffix := "-" + sessionID + ".jsonl"
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), suffix) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("codex session %s not found under %s", sessionID, root)
	}
	return found, nil
}
