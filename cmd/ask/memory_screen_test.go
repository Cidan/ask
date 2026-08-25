package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
)

type browserClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *browserClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *browserClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// memoryBrowserFixture opens a fake-embedded store with three concepts
// for the model's cwd (two project, one global) and one for another
// project, and returns the model plus the clock the store reads.
func memoryBrowserFixture(t *testing.T) (model, *browserClock, []int64) {
	t.Helper()
	m := newTestModel(t, newFakeProvider())
	clock := &browserClock{now: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)}
	_ = memory.Close()
	if err := memory.Open(memory.Options{DBPath: filepath.Join(t.TempDir(), "b.db"), Embedder: memory.NewFakeEmbedder(512), Now: clock.Now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memory.Close() })
	ctx := context.Background()
	svc := memory.Default()
	scope := memory.ScopeFor(m.cwd)
	a, _ := svc.Upsert(ctx, memory.Concept{Scope: scope, Kind: memory.KindProject, Topic: "build", Title: "make test builds llama", Body: "Line one.\nLine two."})
	b, _ := svc.Upsert(ctx, memory.Concept{Scope: scope, Kind: memory.KindFeedback, Topic: "style", Title: "short answers"})
	g, _ := svc.Upsert(ctx, memory.Concept{Scope: memory.ScopeGlobal, Kind: memory.KindUser, Topic: "profile", Title: "writes Go daily"})
	_, _ = svc.Upsert(ctx, memory.Concept{Scope: "/elsewhere", Kind: memory.KindProject, Title: "another project's fact"})
	clock.Advance(time.Minute)
	_, _ = svc.Reinforce(ctx, b)
	clock.Advance(time.Minute)
	return m, clock, []int64{a, b, g}
}

func browserTitles(s *memoryBrowserState) []string {
	var out []string
	for _, idx := range s.visibleRows() {
		out = append(out, s.concepts[idx].Title)
	}
	return out
}

func TestMemoryBrowser_ClosedService(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	_ = memory.Close()
	next, cmd := m.openMemoryBrowser()
	if next.(model).mode != modeInput || next.(model).memoryBrowser != nil || cmd == nil {
		t.Fatal("closed memory must keep the input mode and toast")
	}
	viaCmd, toast := m.handleCommand("/memory")
	if viaCmd.(model).mode != modeInput || toast == nil {
		t.Fatal("/memory with memory closed must toast and stay put")
	}
}

func TestMemoryBrowser_ListSortFilterExpand(t *testing.T) {
	m, _, ids := memoryBrowserFixture(t)
	next, _ := m.handleCommand("/memory")
	m = next.(model)
	s := m.memoryBrowser
	if m.mode != modeMemory || s == nil {
		t.Fatal("/memory must open the browser")
	}
	if got := strings.Join(browserTitles(s), "|"); got != "short answers|make test builds llama|writes Go daily" {
		t.Fatalf("weight order = %v", browserTitles(s))
	}
	for _, c := range s.concepts {
		if c.Scope == "/elsewhere" {
			t.Fatal("other projects must not appear")
		}
	}

	// Tab → recent (the reinforced one was touched last), then kind.
	mi, _ := m.updateMemoryBrowser(tea.KeyPressMsg{Code: tea.KeyTab})
	m = mi.(model)
	if s.sort != memorySortRecent || browserTitles(s)[0] != "short answers" {
		t.Fatalf("recent sort: %v", browserTitles(s))
	}
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: tea.KeyTab})
	m = mi.(model)
	if s.sort != memorySortKind || browserTitles(s)[0] != "short answers" || browserTitles(s)[2] != "writes Go daily" {
		t.Fatalf("kind sort (feedback, project, user): %v", browserTitles(s))
	}

	// Filter matches topic and title.
	for _, r := range "build" {
		mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = mi.(model)
	}
	if got := browserTitles(s); len(got) != 1 || got[0] != "make test builds llama" {
		t.Fatalf("filtered = %v", got)
	}
	// Enter expands the body of the current row.
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if !s.expanded[ids[0]] {
		t.Fatal("enter must expand the body")
	}
	rows := s.renderRows()
	if len(rows) != 3 || !rows[1].isBody || rows[1].bodyLine != "Line one." || rows[2].bodyLine != "Line two." {
		t.Fatalf("render rows = %+v", rows)
	}
	lines := m.renderMemoryLines(s, 100, 12)
	if len(lines) != 12 || !strings.Contains(lines[0], "concepts") || !strings.Contains(lines[3], "builds llama") || !strings.Contains(lines[4], "Line one.") {
		t.Fatalf("rendered lines:\n%s", strings.Join(lines, "\n"))
	}
	// Backspace edits the filter; Esc closes.
	for i := 0; i < 5; i++ {
		mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: tea.KeyBackspace})
		m = mi.(model)
	}
	if len(browserTitles(s)) != 3 {
		t.Fatal("clearing the filter restores every row")
	}
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mi.(model)
	if m.mode != modeInput || m.memoryBrowser != nil {
		t.Fatal("esc must close the browser")
	}
}

func TestMemoryBrowser_ReinforceDemoteForget(t *testing.T) {
	m, clock, ids := memoryBrowserFixture(t)
	next, _ := m.handleCommand("/memory")
	m = next.(model)
	s := m.memoryBrowser
	// Cursor on the second row: the untouched project concept.
	mi, _ := m.updateMemoryBrowser(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mi.(model)
	cur, _ := s.current()
	if cur.ID != ids[0] {
		t.Fatalf("cursor on #%d, want #%d", cur.ID, ids[0])
	}
	before := cur.Weight

	mi, cmd := m.updateMemoryBrowser(tea.KeyPressMsg{Code: '+', Text: "+"})
	m = mi.(model)
	if cmd == nil {
		t.Fatal("reinforce must toast")
	}
	after, _ := s.current()
	if after.ID != ids[0] || after.Weight <= before {
		t.Fatalf("reinforce: %v -> %v", before, after.Weight)
	}
	// Inside the refractory window a demote is a no-op.
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: '-', Text: "-"})
	m = mi.(model)
	if again, _ := s.current(); again.Weight != after.Weight {
		t.Fatal("refractory demote changed the weight")
	}
	clock.Advance(memory.RefractoryPeriod + time.Second)
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: '-', Text: "-"})
	m = mi.(model)
	if demoted, _ := s.current(); demoted.Weight >= after.Weight {
		t.Fatalf("demote: %v -> %v", after.Weight, demoted.Weight)
	}

	// Ctrl+X arms, a stray key disarms, Ctrl+X twice forgets.
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	m = mi.(model)
	if s.confirmForget != ids[0] {
		t.Fatal("first ctrl+x must arm")
	}
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mi.(model)
	if s.confirmForget != 0 {
		t.Fatal("any other key must disarm")
	}
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: tea.KeyUp})
	m = mi.(model)
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	m = mi.(model)
	mi, _ = m.updateMemoryBrowser(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	m = mi.(model)
	if _, err := memory.Default().Get(context.Background(), ids[0]); err == nil {
		t.Fatal("concept must be forgotten")
	}
	if len(s.concepts) != 2 {
		t.Fatalf("browser must reload after forget: %d", len(s.concepts))
	}
}

func TestGlobalConfig_MemoryRow(t *testing.T) {
	m := fieldsFixture(t)
	var row configItem
	for _, it := range m.globalConfigItems() {
		if it.id == "memory" {
			row = it
		}
	}
	if row.name != "Memory..." || row.key != "session provider (cheapest)" {
		t.Fatalf("memory row = %+v", row)
	}
	mi, _ := m.handleGlobalConfigEnter("memory")
	m = mi.(model)
	if !m.configFields.active || m.configFields.title != "Memory" {
		t.Fatalf("memory row opens the fields picker: %+v", m.configFields)
	}
	// An unknown provider id is rejected and keeps the editor open.
	m = m.openFieldsPickerEditor("provider")
	m.configFields.draft = "nosuch"
	mi, cmd := m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if cmd == nil || m.configFields.editing != "provider" {
		t.Fatal("invalid provider must toast and stay in the editor")
	}
	m.configFields.draft = providers.OpenRouterProviderID
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	m = m.openFieldsPickerEditor("model")
	m.configFields.draft = "openai/gpt-4o-mini"
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	cfg, _ := loadConfig()
	if cfg.Memory.Provider != providers.OpenRouterProviderID || cfg.Memory.Model != "openai/gpt-4o-mini" {
		t.Fatalf("persisted memory block = %+v", cfg.Memory)
	}
	if memoryConfigSummary(cfg) != "openrouter/openai/gpt-4o-mini" {
		t.Fatalf("summary = %q", memoryConfigSummary(cfg))
	}
	if rows := m.fieldsPickerItems(); rows[0].key != providers.OpenRouterProviderID || rows[1].key != "openai/gpt-4o-mini" {
		t.Fatalf("picker rows = %+v", rows)
	}
	cfg.Memory.Model = ""
	if memoryConfigSummary(cfg) != "openrouter (cheapest)" {
		t.Fatalf("provider-only summary = %q", memoryConfigSummary(cfg))
	}
}
