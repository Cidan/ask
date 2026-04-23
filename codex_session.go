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
//
// Quiet mode collapses consecutive assistant messages into a single
// entry that keeps the most recent text, matching claude's behavior.
// Each user message resets the collapse so separate turns stay
// separate. Diff rendering from the rollout jsonl isn't implemented
// yet — codex records file-change events under a `fileChange`
// response_item (and related event_msg subtypes) that we don't
// surface here. Real-time diffs still render during a new turn via
// the wire-stream handler; this is a history-replay limitation.
func loadCodexHistory(sessionID string, opts HistoryOpts) ([]historyEntry, error) {
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
	lastAssistantIdx := -1
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
		// Peek the payload type once so we can branch: messages are the
		// conversation turns, function_call / function_call_output /
		// custom_tool_call carry tool activity the new Render Tool
		// Output toggle surfaces.
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rec.Payload, &head) != nil {
			continue
		}
		switch head.Type {
		case "message":
			var item codexRolloutItem
			if json.Unmarshal(rec.Payload, &item) != nil {
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
				lastAssistantIdx = -1
			case "assistant":
				if opts.QuietMode && lastAssistantIdx >= 0 {
					entries[lastAssistantIdx].text = txt
					entries[lastAssistantIdx].rendered = ""
					continue
				}
				entries = append(entries, historyEntry{kind: histResponse, text: txt})
				lastAssistantIdx = len(entries) - 1
			}
		case "function_call", "custom_tool_call":
			if !opts.RenderToolOutput || opts.QuietMode {
				continue
			}
			name, input := codexRolloutCallSummary(rec.Payload)
			entries = append(entries, historyEntry{
				kind: histPrerendered,
				text: renderToolCallBlock(name, input),
			})
		case "function_call_output":
			if !opts.RenderToolOutput || opts.QuietMode {
				continue
			}
			output, isError := codexRolloutOutputSummary(rec.Payload)
			entries = append(entries, historyEntry{
				kind: histPrerendered,
				text: renderToolResultBlock(output, isError),
			})
		}
	}
	return entries, nil
}

// codexRolloutCallSummary extracts the tool name and input map from a
// function_call / custom_tool_call response_item payload. function_call
// carries its input as a JSON-encoded string in `arguments`; we parse
// that into a map so renderToolCallBlock can list the keys inline.
// custom_tool_call uses `input` instead (a raw string), which we surface
// as a single-key map to keep the renderer's format uniform.
func codexRolloutCallSummary(payload []byte) (string, map[string]any) {
	var rec struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Input     string `json:"input"`
	}
	if json.Unmarshal(payload, &rec) != nil {
		return "", nil
	}
	name := rec.Name
	if name == "" {
		name = rec.Type
	}
	if rec.Arguments != "" {
		var m map[string]any
		if json.Unmarshal([]byte(rec.Arguments), &m) == nil {
			return name, m
		}
		return name, map[string]any{"arguments": rec.Arguments}
	}
	if rec.Input != "" {
		return name, map[string]any{"input": rec.Input}
	}
	return name, nil
}

// codexRolloutOutputSummary extracts the output string from a
// function_call_output response_item payload. Codex marks failed calls
// with status != "completed"; we map anything else to isError=true so
// the renderer can style the block accordingly.
func codexRolloutOutputSummary(payload []byte) (string, bool) {
	var rec struct {
		Output string `json:"output"`
		Status string `json:"status"`
	}
	if json.Unmarshal(payload, &rec) != nil {
		return "", false
	}
	return rec.Output, rec.Status != "" && rec.Status != "completed"
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
