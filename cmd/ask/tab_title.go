package main

// Tab titles: a short human-readable description of what each tab is
// doing, surfaced on the sidebar cards and persisted on the
// VirtualSession. The same call names the conversation's memory topic.

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// tabTitleMaxLen caps a title at sidebar-card scale.
const tabTitleMaxLen = 50

const tabTitleSystemPrompt = `You generate a short title and a topic for a coding-agent session based on the first message the user opens it with.

Rules:
- Line 1: the title. At most 50 characters. No quotes, no colons, no trailing period. Describe the task itself ("fix flaky auth test"), never the conversation ("user asks about a test").
- Line 2: "topic: " followed by one or two words naming what the session is about. Reuse one of the listed topics when it fits; otherwise coin a new one.
- Same language as the user's message. Return exactly those two lines and nothing else.`

// tabTitleTimeout bounds the background title call so a dead network
// can't leak goroutines per tab.
var tabTitleTimeout = 30 * time.Second

// generateTabTitleText runs the one-shot LLM title call, returning the
// raw reply text and the call's token usage. topics are the memory
// topics already known for the project, offered for reuse.
var generateTabTitleText = func(providerID, modelID, prompt string, topics []string) (string, TokenUsage, error) {
	p, ok := providers.Get(providerID)
	if !ok {
		return "", TokenUsage{}, fmt.Errorf("tab title: unknown provider %q", providerID)
	}
	cfg, _ := loadConfig()
	if modelID == "" {
		modelID = p.DefaultModel()
	}

	ctx, cancel := context.WithTimeout(context.Background(), tabTitleTimeout)
	defer cancel()

	llm, err := engine.ModelBuilder(ctx, p, toPkgConfig(cfg), modelID)
	if err != nil {
		return "", TokenUsage{}, err
	}
	// One-shot: a subprocess-backed provider forks a child for this single
	// call, so close it when the title is done.
	defer engine.CloseModel(llm)
	var content strings.Builder
	if len(topics) > 0 {
		content.WriteString("Known topics: ")
		content.WriteString(strings.Join(topics, ", "))
		content.WriteString("\n\n")
	}
	content.WriteString("Generate the title and topic for the following session-opening message:\n\n")
	content.WriteString(prompt)
	req := &adkmodel.LLMRequest{
		Model: modelID,
		Contents: []*genai.Content{
			genai.NewContentFromText(content.String(), genai.RoleUser),
		},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(tabTitleSystemPrompt, genai.RoleUser),
		},
	}

	// One non-streaming call: a streamed run yields deltas and then the
	// aggregated final message, and concatenating both doubles the title.
	var sb strings.Builder
	var usage TokenUsage
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", usage, err
		}
		if resp == nil {
			continue
		}
		if resp.UsageMetadata != nil {
			usage.InputTokens = int(resp.UsageMetadata.PromptTokenCount)
			usage.OutputTokens = int(resp.UsageMetadata.CandidatesTokenCount)
		}
		if resp.Content == nil {
			continue
		}
		for _, part := range resp.Content.Parts {
			if part.Text != "" && !part.Thought {
				sb.WriteString(part.Text)
			}
		}
	}
	return sb.String(), usage, nil
}

// generateTabTitleCmd dispatches the background title call. The
// project's known topics are read inside the command so the update loop
// never touches the store.
func generateTabTitleCmd(tabID int, providerID, modelID, cwd, prompt string) tea.Cmd {
	return func() tea.Msg {
		var topics []string
		if svc := memory.Default(); svc.IsOpen() {
			ctx, cancel := context.WithTimeout(context.Background(), memory.DefaultHookTimeout)
			topics = svc.TopicNames(ctx, cwd, memory.DefaultTopicK)
			cancel()
		}
		raw, usage, err := generateTabTitleText(providerID, modelID, prompt, topics)
		if err != nil {
			debugLog("tab title generation: %v", err)
			return tabTitleMsg{tabID: tabID}
		}
		costModel := modelID
		if costModel == "" {
			if p, ok := providers.Get(providerID); ok {
				costModel = p.DefaultModel()
			}
		}
		cost, known := stepCostUSD(providerID, costModel, usage)
		title, topic := splitTitleAndTopic(raw)
		return tabTitleMsg{tabID: tabID, title: title, topic: topic, costUSD: cost, costKnown: known}
	}
}

// maybeStartTabTitle seeds the fallback title from the first user prompt.
func (m *model) maybeStartTabTitle(line string) tea.Cmd {
	if m.workflowRun != nil || m.tabTitle != "" || m.provider == nil {
		return nil
	}
	if strings.TrimSpace(line) == "" {
		return nil
	}
	if invalid := validateAskCwd(m.cwd); invalid.Msg != "" {
		return nil
	}
	m.tabTitle = fallbackTabTitle(line)
	return generateTabTitleCmd(m.id, m.provider.ID(), m.providerModel, m.cwd, line)
}

// fallbackTabTitle derives the instant title from the user's prompt.
func fallbackTabTitle(prompt string) string {
	return clipText(strings.TrimSpace(flattenNewlines(prompt)), tabTitleMaxLen)
}

// splitTitleAndTopic reads the two-line reply: the first non-empty line
// is the title, a "topic:" line (or the second line) is the topic. A
// reply without a topic line yields an empty topic.
func splitTitleAndTopic(raw string) (string, string) {
	raw = stripThinkTags(raw)
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if len(lines) == 0 {
		return "", ""
	}
	title := sanitizeTabTitle(lines[0])
	topic := ""
	for _, line := range lines[1:] {
		if rest, ok := strings.CutPrefix(strings.ToLower(line), "topic:"); ok {
			topic = rest
			break
		}
	}
	if topic == "" && len(lines) > 1 {
		topic = lines[1]
	}
	topic = strings.Trim(topic, "\"'` ")
	return title, memory.NormalizeTopic(topic)
}

func stripThinkTags(s string) string {
	for {
		start := strings.Index(s, "<think>")
		if start < 0 {
			return s
		}
		end := strings.Index(s, "</think>")
		if end < 0 || end < start {
			return s[:start]
		}
		s = s[:start] + s[end+len("</think>"):]
	}
}

// sanitizeTabTitle cleans an LLM response into a card-safe one-liner.
func sanitizeTabTitle(s string) string {
	s = stripThinkTags(s)
	s = strings.TrimSpace(flattenNewlines(s))
	s = strings.Trim(s, "\"'`")
	s = strings.TrimSuffix(s, ".")
	s = strings.TrimSpace(s)
	return clipText(s, tabTitleMaxLen)
}

// persistTabTitle writes the title and topic onto the tab's VirtualSession.
func (m *model) persistTabTitle() {
	if m.virtualSessionID == "" || m.tabTitle == "" {
		return
	}
	vsID, title, topic := m.virtualSessionID, m.tabTitle, m.tabTopic
	if err := mutateVirtualSessions(func(store *virtualSessionStore) error {
		if vs := store.findByID(vsID); vs != nil {
			vs.Title = title
			vs.Topic = topic
		}
		return nil
	}); err != nil {
		debugLog("persistTabTitle: %v", err)
	}
}

// pushTopicToSession hands the tab's topic to the live agent session so
// the next recall starts from it.
func (m *model) pushTopicToSession() {
	if m.proc == nil || m.tabTopic == "" {
		return
	}
	if s, ok := m.proc.payload.(*agentSession); ok {
		s.setTopic(m.tabTopic)
	}
}
