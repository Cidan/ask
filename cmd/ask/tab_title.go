package main

// Tab titles: a short human-readable description of what each tab is
// doing, surfaced on the sidebar cards and
// persisted on the VirtualSession for /resume.

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// tabTitleMaxLen caps a title at sidebar-card scale.
const tabTitleMaxLen = 50

const tabTitleSystemPrompt = `You generate a short title for a coding-agent session based on the first message the user opens it with.

Rules:
- At most 50 characters. One line. No quotes, no colons, no trailing period.
- Same language as the user's message.
- Describe the task itself ("fix flaky auth test"), never the conversation ("user asks about a test").
- The entire text you return is used verbatim as the title.`

// tabTitleTimeout bounds the background title call so a dead network
// can't leak goroutines per tab.
var tabTitleTimeout = 30 * time.Second

// generateTabTitleText runs the one-shot LLM title call, returning the
// raw title text and the call's token usage.
var generateTabTitleText = func(providerID, modelID, prompt string) (string, TokenUsage, error) {
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
	req := &adkmodel.LLMRequest{
		Model: modelID,
		Contents: []*genai.Content{
			genai.NewContentFromText("Generate a concise title for the following session-opening message:\n\n"+prompt, genai.RoleUser),
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

// generateTabTitleCmd dispatches the background title call.
func generateTabTitleCmd(tabID int, providerID, modelID, prompt string) tea.Cmd {
	return func() tea.Msg {
		raw, usage, err := generateTabTitleText(providerID, modelID, prompt)
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
		return tabTitleMsg{tabID: tabID, title: sanitizeTabTitle(raw), costUSD: cost, costKnown: known}
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
	return generateTabTitleCmd(m.id, m.provider.ID(), m.providerModel, line)
}

// fallbackTabTitle derives the instant title from the user's prompt.
func fallbackTabTitle(prompt string) string {
	return clipText(strings.TrimSpace(flattenNewlines(prompt)), tabTitleMaxLen)
}

// sanitizeTabTitle cleans an LLM response into a card-safe one-liner.
func sanitizeTabTitle(s string) string {
	for {
		start := strings.Index(s, "<think>")
		if start < 0 {
			break
		}
		end := strings.Index(s, "</think>")
		if end < 0 || end < start {
			s = s[:start]
			break
		}
		s = s[:start] + s[end+len("</think>"):]
	}
	s = strings.TrimSpace(flattenNewlines(s))
	s = strings.Trim(s, "\"'`")
	s = strings.TrimSuffix(s, ".")
	s = strings.TrimSpace(s)
	return clipText(s, tabTitleMaxLen)
}

// persistTabTitle writes the title onto the tab's VirtualSession.
func (m *model) persistTabTitle() {
	if m.virtualSessionID == "" || m.tabTitle == "" {
		return
	}
	vsID, title := m.virtualSessionID, m.tabTitle
	if err := mutateVirtualSessions(func(store *virtualSessionStore) error {
		if vs := store.findByID(vsID); vs != nil {
			vs.Title = title
		}
		return nil
	}); err != nil {
		debugLog("persistTabTitle: %v", err)
	}
}
