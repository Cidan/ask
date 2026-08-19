package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Cidan/ask/pkg/engine"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// agentSessionStore persists in-process agent transcripts using ADK session persistence.
type agentSessionStore struct {
	provider string
}

func (st *agentSessionStore) svc(cwd string) *engine.FileSessionService {
	return engine.NewFileSessionService(st.provider, cwd)
}

func (st *agentSessionStore) root() (string, error) {
	return st.svc("").Root()
}

func (st *agentSessionStore) dirFor(cwd string) (string, error) {
	return st.svc(cwd).DirFor(cwd)
}

func (st *agentSessionStore) pathFor(id string) (string, error) {
	return st.svc("").PathFor(id)
}

func (st *agentSessionStore) save(id, cwd string, messages []engine.Message) error {
	store := engine.NewSessionStore(st.provider)
	return store.Save(id, cwd, messages)
}

func (st *agentSessionStore) saveEvents(id, cwd string, events []*session.Event) error {
	store := engine.NewSessionStore(st.provider)
	return store.SaveEvents(id, cwd, events)
}

func (st *agentSessionStore) load(id string) (engine.StoredSessionFile, error) {
	store := engine.NewSessionStore(st.provider)
	return store.Load(id)
}

func (st *agentSessionStore) list(cwd string) ([]sessionEntry, error) {
	store := engine.NewSessionStore(st.provider)
	summaries, err := store.List(cwd)
	if err != nil {
		return nil, err
	}
	entries := make([]sessionEntry, len(summaries))
	for i, s := range summaries {
		entries[i] = sessionEntry{
			id:      s.ID,
			cwd:     s.Cwd,
			preview: s.Preview,
			modTime: s.ModTime,
		}
	}
	return entries, nil
}

func (st *agentSessionStore) loadHistory(id string, opts HistoryOpts) ([]historyEntry, error) {
	file, err := st.load(id)
	if err != nil {
		return nil, err
	}
	return LoadHistoryEntriesFromEvents(file.Events, opts)
}

// LoadHistoryEntriesFromEvents maps a slice of session.Event directly to Bubble Tea historyEntry view models.
func LoadHistoryEntriesFromEvents(events []*session.Event, opts HistoryOpts) ([]historyEntry, error) {
	mode := opts.ToolOutput
	showTools := !opts.QuietMode && mode != toolOutputOff
	var entries []historyEntry
	lastAssistantIdx := -1

	for _, e := range events {
		if e == nil || e.LLMResponse.Content == nil {
			continue
		}
		content := e.LLMResponse.Content
		isUser := e.Author == "user" || content.Role == genai.RoleUser

		if isUser {
			if showTools {
				for _, p := range content.Parts {
					if p == nil {
						continue
					}
					if p.FunctionResponse != nil {
						resStr := ""
						isErr := false
						if res, ok := p.FunctionResponse.Response["result"].(string); ok {
							resStr = res
						}
						if errFlag, ok := p.FunctionResponse.Response["is_error"].(bool); ok {
							isErr = errFlag
						}
						entries = append(entries, historyEntry{
							kind: histPrerendered,
							text: renderToolResultBlock(resStr, isErr),
						})
					}
				}
			}

			var textParts []string
			for _, p := range content.Parts {
				if p != nil && p.Text != "" && !p.Thought && p.FunctionResponse == nil && p.FunctionCall == nil {
					textParts = append(textParts, p.Text)
				}
			}
			fullText := strings.TrimSpace(strings.Join(textParts, ""))
			if fullText != "" {
				entries = append(entries, historyEntry{kind: histUser, text: fullText})
				lastAssistantIdx = -1
			}
		} else {
			// Model / Assistant event
			if showTools {
				for _, p := range content.Parts {
					if p == nil {
						continue
					}
					if p.FunctionCall != nil {
						name := p.FunctionCall.Name
						input := p.FunctionCall.Args
						if name == "invoke_tool" {
							name, input = unwrapInvokeToolCall(input)
						}
						entries = append(entries, historyEntry{
							kind: histPrerendered,
							text: renderToolCallBlock(name, input, mode),
						})
					}
				}
			}

			var nonThoughtTexts []string
			for _, p := range content.Parts {
				if p != nil && !p.Thought && p.Text != "" {
					nonThoughtTexts = append(nonThoughtTexts, p.Text)
				}
			}
			msgText := strings.TrimSpace(strings.Join(nonThoughtTexts, ""))
			if msgText != "" {
				if opts.QuietMode && lastAssistantIdx >= 0 {
					entries[lastAssistantIdx].text = msgText
					invalidateEntryRender(&entries[lastAssistantIdx])
				} else {
					entries = append(entries, historyEntry{kind: histResponse, text: msgText})
					lastAssistantIdx = len(entries) - 1
				}
			}

			if showTools {
				for _, p := range content.Parts {
					if p == nil {
						continue
					}
					if p.FunctionResponse != nil {
						resStr := ""
						isErr := false
						if res, ok := p.FunctionResponse.Response["result"].(string); ok {
							resStr = res
						}
						if errFlag, ok := p.FunctionResponse.Response["is_error"].(bool); ok {
							isErr = errFlag
						}
						entries = append(entries, historyEntry{
							kind: histPrerendered,
							text: renderToolResultBlock(resStr, isErr),
						})
					}
				}
			}
		}
	}
	return entries, nil
}

func (st *agentSessionStore) materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	if workspace == "" {
		return "", "", errors.New("materialize: workspace is required")
	}
	var events []*session.Event
	now := time.Now()
	for _, turn := range turns {
		switch turn.Role {
		case "user":
			events = append(events, &session.Event{
				Author: "user",
				LLMResponse: adkmodel.LLMResponse{
					Content: genai.NewContentFromText(turn.Text, genai.RoleUser),
				},
				Timestamp: now,
			})
		case "assistant":
			events = append(events, &session.Event{
				Author: "ask_coder",
				LLMResponse: adkmodel.LLMResponse{
					Content: genai.NewContentFromText(turn.Text, genai.RoleModel),
				},
				Timestamp: now,
			})
		}
	}
	id := newUUIDv4()
	if err := st.saveEvents(id, workspace, events); err != nil {
		return "", "", err
	}
	return id, workspace, nil
}

var claudeProjectDirNonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

const claudeProjectDirMaxLen = 200

func encodeClaudeProjectDir(path string) string {
	return engine.EncodeProjectDir(path)
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
