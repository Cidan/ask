package main

// Tab titles: a short human-readable description of what each tab is
// doing, surfaced on the sidebar cards and
// persisted on the VirtualSession for /resume.
//
// Two layers, cheapest first: the moment the first user turn is sent
// the title is seeded from the prompt itself (fallbackTabTitle), then
// a one-shot LLM call — the crush session-title pattern — refines it
// asynchronously (generateTabTitleCmd → tabTitleMsg).

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
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

// tabTitleMaxOutputTokens leaves room for reasoning models that burn
// tokens before emitting text; the sanitizer clamps the result anyway.
const tabTitleMaxOutputTokens = int64(512)

// generateTabTitleText runs the one-shot LLM title call, returning the
// raw title text and the call's token usage (so the dispatcher can
// price it onto the session cost meter). Swappable package var so
// tests script the result with zero network.
var generateTabTitleText = func(providerID, modelID, prompt string) (string, fantasy.Usage, error) {
	spec, ok := agentSpecByID(providerID)
	if !ok {
		return "", fantasy.Usage{}, fmt.Errorf("tab title: provider %q has no agent spec", providerID)
	}
	cfg, _ := loadConfig()
	if modelID == "" {
		modelID = spec.defaultModel
	}
	lm, err := spec.buildModel(cfg, modelID)
	if err != nil {
		return "", fantasy.Usage{}, err
	}
	agent := fantasy.NewAgent(lm, fantasy.WithSystemPrompt(tabTitleSystemPrompt))
	ctx, cancel := context.WithTimeout(context.Background(), tabTitleTimeout)
	defer cancel()
	retryMaxRetries, _, _ := agentRetryOptions(cfg)
	res, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:          "Generate a concise title for the following session-opening message:\n\n" + prompt,
		MaxOutputTokens: maxOutputTokensPtr(tabTitleMaxOutputTokens),
		MaxRetries:      retryMaxRetriesPtr(retryMaxRetries),
	})
	if err != nil {
		return "", fantasy.Usage{}, err
	}
	if res == nil {
		return "", fantasy.Usage{}, fmt.Errorf("tab title: empty response")
	}
	return res.Response.Content.Text(), res.TotalUsage, nil
}

// generateTabTitleCmd dispatches the background title call and lands
// the sanitized result as a tabTitleMsg, priced via the catwalk
// catalog. Failures return an empty title — the handler keeps the
// first-prompt fallback, never errors at the user.
func generateTabTitleCmd(tabID int, providerID, modelID, prompt string) tea.Cmd {
	return func() tea.Msg {
		raw, usage, err := generateTabTitleText(providerID, modelID, prompt)
		if err != nil {
			debugLog("tab title generation: %v", err)
			return tabTitleMsg{tabID: tabID}
		}
		// Resolve the same default the generator used so the catalog
		// lookup prices the model actually called.
		costModel := modelID
		if costModel == "" {
			if spec, ok := agentSpecByID(providerID); ok {
				costModel = spec.defaultModel
			}
		}
		cost, known := stepCostUSD(providerID, costModel, usage)
		return tabTitleMsg{tabID: tabID, title: sanitizeTabTitle(raw), costUSD: cost, costKnown: known}
	}
}

// maybeStartTabTitle seeds the fallback title from the first user
// prompt and returns the cmd that refines it via the LLM. Nil (and a
// no-op) for workflow tabs, already-titled tabs, blank prompts, and
// invalid cwds (where the turn itself will be refused).
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

// fallbackTabTitle derives the instant title from the user's prompt:
// newlines collapsed, clipped to card scale.
func fallbackTabTitle(prompt string) string {
	return clipText(strings.TrimSpace(flattenNewlines(prompt)), tabTitleMaxLen)
}

// sanitizeTabTitle cleans an LLM response into a card-safe one-liner:
// reasoning tags stripped, newlines collapsed, surrounding quotes and
// trailing period dropped, clipped to tabTitleMaxLen.
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
// No-op until the tab is paired with a VS — recordVirtualSession
// backfills the title on the next turn completion for that case.
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
