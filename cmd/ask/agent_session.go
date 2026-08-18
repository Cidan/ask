package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Cidan/ask/pkg/engine"
)

// agentSessionStore persists in-process agent transcripts as message arrays.
type agentSessionStore struct {
	provider string
}

type agentSessionFile struct {
	Version   int              `json:"version"`
	Cwd       string           `json:"cwd"`
	CreatedAt time.Time        `json:"createdAt"`
	UpdatedAt time.Time        `json:"updatedAt"`
	Messages  []engine.Message `json:"messages"`
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

func (st *agentSessionStore) save(id, cwd string, messages []engine.Message) error {
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

func agentSessionPreview(messages []engine.Message) string {
	for _, m := range messages {
		if m.Role != engine.RoleUser {
			continue
		}
		if m.Text != "" {
			line := strings.TrimSpace(strings.SplitN(m.Text, "\n", 2)[0])
			if len(line) > 80 {
				line = line[:80] + "…"
			}
			if line != "" {
				return line
			}
		}
	}
	return "(empty session)"
}

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
		case engine.RoleUser:
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
			entries = append(entries, historyEntry{kind: histUser, text: m.Text})
			lastAssistantIdx = -1
		case engine.RoleAssistant, engine.RoleModel:
			if showTools {
				for _, tc := range m.ToolCalls {
					name := tc.Name
					input := tc.Args
					if name == "invoke_tool" {
						name, input = unwrapInvokeToolCall(input)
					}
					entries = append(entries, historyEntry{
						kind: histPrerendered,
						text: renderToolCallBlock(name, input, mode),
					})
				}
			}
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
			if opts.QuietMode && lastAssistantIdx >= 0 {
				entries[lastAssistantIdx].text = m.Text
				invalidateEntryRender(&entries[lastAssistantIdx])
				continue
			}
			entries = append(entries, historyEntry{kind: histResponse, text: m.Text})
			lastAssistantIdx = len(entries) - 1
		case engine.RoleTool:
			if !showTools {
				continue
			}
			for _, tr := range m.ToolResults {
				entries = append(entries, historyEntry{
					kind: histPrerendered,
					text: renderToolResultBlock(tr.Content, tr.IsError),
				})
			}
		}
	}
	return entries, nil
}

func (st *agentSessionStore) materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	if workspace == "" {
		return "", "", errors.New("materialize: workspace is required")
	}
	messages := make([]engine.Message, 0, len(turns))
	for _, turn := range turns {
		switch turn.Role {
		case "user":
			messages = append(messages, engine.NewUserMessage(turn.Text))
		case "assistant":
			messages = append(messages, engine.NewAssistantMessage(turn.Text, nil, nil))
		}
	}
	id := newUUIDv4()
	if err := st.save(id, workspace, messages); err != nil {
		return "", "", err
	}
	return id, workspace, nil
}

var claudeProjectDirNonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

const claudeProjectDirMaxLen = 200

func encodeClaudeProjectDir(path string) string {
	enc := claudeProjectDirNonAlnum.ReplaceAllString(path, "-")
	if len(enc) <= claudeProjectDirMaxLen {
		return enc
	}
	return enc[:claudeProjectDirMaxLen] + "-" + claudeProjectDirHash(path)
}

func claudeProjectDirHash(path string) string {
	var h int32
	for _, r := range path {
		h = h*31 + r
	}
	n := int64(h)
	if n < 0 {
		n = -n
	}
	return strconv.FormatInt(n, 36)
}
