package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
)

// agentSessionStore persists in-process agent transcripts as fantasy
// message arrays so a session can be resumed wire-true (including
// reasoning_content and tool call/result pairing). One store per
// provider id; files live under
// ~/.config/ask/agent-sessions/<provider>/<encoded-cwd>/<id>.json
// with the same cwd encoding claude uses for its project dirs.
type agentSessionStore struct {
	provider string
}

// agentSessionFile is the on-disk shape. Messages round-trip through
// fantasy's typed part (de)serialization.
type agentSessionFile struct {
	Version   int               `json:"version"`
	Cwd       string            `json:"cwd"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
	Messages  []fantasy.Message `json:"messages"`
}

func (st *agentSessionStore) root() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ask", "agent-sessions", st.provider), nil
}

func (st *agentSessionStore) dirFor(cwd string) (string, error) {
	root, err := st.root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, encodeClaudeProjectDir(cwd)), nil
}

// pathFor locates an existing session file by id across every project
// dir (LoadHistory and resume get only the id, not the cwd).
func (st *agentSessionStore) pathFor(id string) (string, error) {
	root, err := st.root()
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", id+".json"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no stored agent session %s", id)
	}
	return matches[0], nil
}

// save writes the full transcript atomically (0600, temp+rename).
// CreatedAt is preserved across saves.
func (st *agentSessionStore) save(id, cwd string, messages []fantasy.Message) error {
	dir, err := st.dirFor(cwd)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, id+".json")
	now := time.Now()
	file := agentSessionFile{
		Version:   1,
		Cwd:       cwd,
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  messages,
	}
	if prev, err := readAgentSessionFile(path); err == nil && !prev.CreatedAt.IsZero() {
		file.CreatedAt = prev.CreatedAt
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+id+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// load returns the stored transcript for a session id.
func (st *agentSessionStore) load(id string) (agentSessionFile, error) {
	path, err := st.pathFor(id)
	if err != nil {
		return agentSessionFile{}, err
	}
	return readAgentSessionFile(path)
}

func readAgentSessionFile(path string) (agentSessionFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agentSessionFile{}, err
	}
	var file agentSessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return agentSessionFile{}, err
	}
	if file.Version != 1 {
		return agentSessionFile{}, fmt.Errorf("unsupported agent session version %d", file.Version)
	}
	return file, nil
}

// list enumerates sessions stored for cwd, newest first.
func (st *agentSessionStore) list(cwd string) ([]sessionEntry, error) {
	dir, err := st.dirFor(cwd)
	if err != nil {
		return nil, err
	}
	dirents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []sessionEntry
	for _, e := range dirents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		file, err := readAgentSessionFile(path)
		if err != nil {
			continue
		}
		sessions = append(sessions, sessionEntry{
			id:      strings.TrimSuffix(e.Name(), ".json"),
			cwd:     file.Cwd,
			preview: agentSessionPreview(file.Messages),
			modTime: info.ModTime(),
		})
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].modTime.After(sessions[j].modTime)
	})
	return sessions, nil
}

// agentSessionPreview pulls the first user text as the picker line.
func agentSessionPreview(messages []fantasy.Message) string {
	for _, m := range messages {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				line := strings.TrimSpace(strings.SplitN(tp.Text, "\n", 2)[0])
				if len(line) > 80 {
					line = line[:80] + "…"
				}
				if line != "" {
					return line
				}
			}
		}
	}
	return "(empty session)"
}

// loadHistory replays a stored transcript as UI history entries,
// honouring the same tool-output/diff/quiet gates claude's replay
// uses so resumed agent sessions look like resumed claude sessions.
func (st *agentSessionStore) loadHistory(id string, opts HistoryOpts) ([]historyEntry, error) {
	file, err := st.load(id)
	if err != nil {
		return nil, err
	}
	mode := opts.ToolOutput
	showTools := !opts.QuietMode && mode != toolOutputOff
	var entries []historyEntry
	lastAssistantIdx := -1
	for _, m := range file.Messages {
		switch m.Role {
		case fantasy.MessageRoleUser:
			text := messageText(m)
			if strings.TrimSpace(text) == "" {
				continue
			}
			entries = append(entries, historyEntry{kind: histUser, text: text})
			lastAssistantIdx = -1
		case fantasy.MessageRoleAssistant:
			if showTools {
				for _, part := range m.Content {
					if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
						input := map[string]any{}
						_ = json.Unmarshal([]byte(tc.Input), &input)
						entries = append(entries, historyEntry{
							kind: histPrerendered,
							text: renderToolCallBlock(tc.ToolName, input, mode),
						})
					}
				}
			}
			text := messageText(m)
			if strings.TrimSpace(text) == "" {
				continue
			}
			if opts.QuietMode && lastAssistantIdx >= 0 {
				entries[lastAssistantIdx].text = text
				invalidateEntryRender(&entries[lastAssistantIdx])
				continue
			}
			entries = append(entries, historyEntry{kind: histResponse, text: text})
			lastAssistantIdx = len(entries) - 1
		case fantasy.MessageRoleTool:
			if !showTools {
				continue
			}
			for _, part := range m.Content {
				if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
					entries = append(entries, historyEntry{
						kind: histPrerendered,
						text: renderToolResultBlock(toolResultText(tr.Output), toolResultIsError(tr.Output)),
					})
				}
			}
		}
	}
	return entries, nil
}

// messageText joins a message's text parts (reasoning parts are
// deliberately not replayed — they are wire context, not transcript).
func messageText(m fantasy.Message) string {
	var b strings.Builder
	for _, part := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && tp.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}

// materialize writes a fresh session seeded from a provider-neutral
// transcript and returns its new id — the cross-provider /provider
// swap path into this provider.
func (st *agentSessionStore) materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	if workspace == "" {
		return "", "", errors.New("materialize: workspace is required")
	}
	messages := make([]fantasy.Message, 0, len(turns))
	for _, turn := range turns {
		switch turn.Role {
		case "user":
			messages = append(messages, fantasy.NewUserMessage(turn.Text))
		case "assistant":
			messages = append(messages, fantasy.Message{
				Role:    fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: turn.Text}},
			})
		}
	}
	id := newUUIDv4()
	if err := st.save(id, workspace, messages); err != nil {
		return "", "", err
	}
	return id, workspace, nil
}
