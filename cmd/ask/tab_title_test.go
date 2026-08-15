package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func swapTitleGenerator(t *testing.T, fn func(providerID, modelID, prompt string) (string, fantasy.Usage, error)) {
	t.Helper()
	prev := generateTabTitleText
	generateTabTitleText = fn
	t.Cleanup(func() { generateTabTitleText = prev })
}

func TestFallbackTabTitle(t *testing.T) {
	if got := fallbackTabTitle("  fix the\nflaky test  "); got != "fix the flaky test" {
		t.Errorf("fallback = %q", got)
	}
	long := strings.Repeat("x", 80)
	got := fallbackTabTitle(long)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > tabTitleMaxLen {
		t.Errorf("long fallback not clipped: %q (%d runes)", got, len([]rune(got)))
	}
}

func TestSanitizeTabTitle(t *testing.T) {
	cases := map[string]string{
		"\"Fix auth test\"":                          "Fix auth test",
		"Fix auth test.":                             "Fix auth test",
		"<think>hmm reasoning</think>Fix auth test":  "Fix auth test",
		"Fix\nauth\ntest":                            "Fix auth test",
		"  '`Fix auth test`'  ":                      "Fix auth test",
		"<think>dangling tail without close tag Fix": "",
	}
	for in, want := range cases {
		if got := sanitizeTabTitle(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("word ", 30)
	if got := sanitizeTabTitle(long); len([]rune(got)) > tabTitleMaxLen {
		t.Errorf("sanitize did not clip: %d runes", len([]rune(got)))
	}
}

func TestMaybeStartTabTitleGating(t *testing.T) {
	swapTitleGenerator(t, func(_, _, _ string) (string, fantasy.Usage, error) {
		return "Generated title", fantasy.Usage{}, nil
	})

	// Seeds the fallback and returns the async cmd.
	m := newTestModel(t, newFakeProvider())
	cmd := (&m).maybeStartTabTitle("do a thing")
	if cmd == nil {
		t.Fatal("returned no title cmd")
	}
	if m.tabTitle != "do a thing" {
		t.Fatalf("fallback title = %q", m.tabTitle)
	}
	msg, ok := cmd().(tabTitleMsg)
	if !ok {
		t.Fatalf("cmd produced %T", cmd())
	}
	if msg.tabID != m.id || msg.title != "Generated title" {
		t.Fatalf("title msg = %+v", msg)
	}

	// Already titled: no second call.
	if cmd := (&m).maybeStartTabTitle("another"); cmd != nil {
		t.Fatal("re-generated title for a titled tab")
	}

	// Workflow tabs never title themselves.
	w := newTestModel(t, newFakeProvider())
	w.workflowRun = &workflowRunState{}
	if cmd := (&w).maybeStartTabTitle("step prompt"); cmd != nil || w.tabTitle != "" {
		t.Fatal("workflow tab generated a title")
	}

	// Blank prompts don't seed.
	b := newTestModel(t, newFakeProvider())
	if cmd := (&b).maybeStartTabTitle("   "); cmd != nil || b.tabTitle != "" {
		t.Fatal("blank prompt seeded a title")
	}
}

func TestGenerateTabTitleCmdSwallowsErrors(t *testing.T) {
	swapTitleGenerator(t, func(_, _, _ string) (string, fantasy.Usage, error) {
		return "", fantasy.Usage{}, errors.New("network down")
	})
	msg := generateTabTitleCmd(7, "fake", "", "prompt")().(tabTitleMsg)
	if msg.tabID != 7 || msg.title != "" {
		t.Fatalf("error path msg = %+v", msg)
	}
}

func TestTabTitleMsgHandler(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.tabTitle = "fallback seed"

	// Wrong tab: ignored.
	m2, _ := runUpdate(t, m, tabTitleMsg{tabID: 99, title: "Other"})
	if m2.tabTitle != "fallback seed" {
		t.Fatal("foreign tabTitleMsg applied")
	}

	// Empty title (generation failed): fallback kept.
	m3, _ := runUpdate(t, m, tabTitleMsg{tabID: m.id})
	if m3.tabTitle != "fallback seed" {
		t.Fatal("empty title clobbered the fallback")
	}

	// Real title applies.
	m4, _ := runUpdate(t, m, tabTitleMsg{tabID: m.id, title: "Refined title"})
	if m4.tabTitle != "Refined title" {
		t.Fatalf("title = %q", m4.tabTitle)
	}

	// A /new between dispatch and arrival cleared tabTitle — the
	// stale generated title must not resurrect.
	m.tabTitle = ""
	m5, _ := runUpdate(t, m, tabTitleMsg{tabID: m.id, title: "Stale"})
	if m5.tabTitle != "" {
		t.Fatal("stale title resurrected after /new")
	}
}

func TestTabTitlePersistsToVirtualSession(t *testing.T) {
	isolateHome(t)
	now := time.Now().UTC()
	if err := mutateVirtualSessions(func(store *virtualSessionStore) error {
		upsertVirtualSession(store, "", "/ws", "fake", "native-1", "/ws", "preview", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store, _ := loadVirtualSessions()
	vsID := store.Sessions[0].ID

	m := newTestModel(t, newFakeProvider())
	m.virtualSessionID = vsID
	m2, _ := runUpdate(t, m, tabTitleMsg{tabID: m.id, title: "Refined"})
	_ = m2
	// Pre-existing fallback required for the handler to apply — redo
	// with a seeded title.
	m.tabTitle = "seed"
	m3, _ := runUpdate(t, m, tabTitleMsg{tabID: m.id, title: "Refined"})
	if m3.tabTitle != "Refined" {
		t.Fatalf("title = %q", m3.tabTitle)
	}
	store2, err := loadVirtualSessions()
	if err != nil {
		t.Fatal(err)
	}
	if got := store2.findByID(vsID).Title; got != "Refined" {
		t.Fatalf("persisted title = %q", got)
	}
}

func TestRecordVirtualSessionBackfillsTitle(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.tabTitle = "Backfilled"
	m.history = []historyEntry{{kind: histUser, text: "first prompt"}}
	(&m).recordVirtualSession("native-1")
	store, err := loadVirtualSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Sessions) != 1 {
		t.Fatalf("%d sessions", len(store.Sessions))
	}
	if got := store.Sessions[0].Title; got != "Backfilled" {
		t.Fatalf("title = %q", got)
	}
}

func TestResumeRehydratesTitle(t *testing.T) {
	isolateHome(t)
	now := time.Now().UTC()
	var vsID string
	if err := mutateVirtualSessions(func(store *virtualSessionStore) error {
		vsID = upsertVirtualSession(store, "", "/ws", "fake", "native-1", "/ws", "the preview text", now)
		store.findByID(vsID).Title = "Saved title"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, newFakeProvider())
	newM, _ := m.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	if got := newM.(model).tabTitle; got != "Saved title" {
		t.Fatalf("rehydrated title = %q", got)
	}

	// No title on the VS → preview fallback.
	if err := mutateVirtualSessions(func(store *virtualSessionStore) error {
		store.findByID(vsID).Title = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m2 := newTestModel(t, newFakeProvider())
	newM2, _ := m2.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	if got := newM2.(model).tabTitle; got != "the preview text" {
		t.Fatalf("preview fallback title = %q", got)
	}
}

// TestGenerateTabTitle_RetriesOn5xx verifies the title call's
// AgentStreamCall carries MaxRetries, so a 5xx on the first attempt
// gets retried and a non-empty title still lands. Drives the real
// generateTabTitleText (not the swap) by swapping the deepseek model
// factory to return a fakeLM that fails once with a 5xx.
func TestGenerateTabTitle_RetriesOn5xx(t *testing.T) {
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		{providerErrPart(500, "internal server error", "boom")},
		textTurn("Recovered Title", fantasy.Usage{InputTokens: 5, OutputTokens: 3}),
	}}
	swapDeepseekLM(t, lm)

	title, usage, err := generateTabTitleText("deepseek", "deepseek-v4-pro", "fix the flaky test")
	if err != nil {
		t.Fatalf("expected success after retry, got error: %v", err)
	}
	if title != "Recovered Title" {
		t.Errorf("title = %q want %q", title, "Recovered Title")
	}
	if usage.OutputTokens != 3 {
		t.Errorf("usage.OutputTokens = %d want 3 (from the successful retry)", usage.OutputTokens)
	}
	if n := len(lm.streamCalls()); n != 2 {
		t.Errorf("stream calls = %d want 2 (1 fail + 1 success)", n)
	}
}
