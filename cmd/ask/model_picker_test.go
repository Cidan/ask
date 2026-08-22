package main

import (
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/providers"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// pressKey builds a minimal KeyPressMsg — bubbletea's zero-value ModMask
// is ModNone, so callers pass tea.ModCtrl explicitly when they need it.
func pressKey(code rune, mods tea.KeyMod) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: mods}
}

func pressSpecial(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

func pressText(t *testing.T, m model, s string) model {
	t.Helper()
	for _, r := range s {
		m = stepKey(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

func stepKey(t *testing.T, m model, msg tea.KeyPressMsg) model {
	t.Helper()
	mi, _ := m.Update(msg)
	mm, ok := mi.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", mi)
	}
	return mm
}

// modelPickerFixture stands up a registry with two fake providers and
// seeds a test model pointed at the first one.
func modelPickerFixture(t *testing.T) (model, *fakeProvider, *fakeProvider) {
	t.Helper()
	isolateHome(t)
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	p1 := newFakeProvider()
	p1.id = "vertex"
	p1.displayName = "Vertex AI"
	p1.modelPicker = ProviderPicker{
		Options: []string{"default", "gemini-2.5-pro", "gemini-2.5-flash"},
	}
	p2 := newFakeProvider()
	p2.id = "custom"
	p2.displayName = "Custom"
	p2.modelPicker = ProviderPicker{
		Options:     []string{"my-model"},
		AllowCustom: true,
	}
	withRegisteredProviders(t, p1, p2)
	m := newTestModel(t, p1)
	return m, p1, p2
}

// selectedRow returns the row under the picker cursor.
func selectedRow(t *testing.T, m model) modelPickerRow {
	t.Helper()
	if m.modelPicker == nil {
		t.Fatal("model picker not open")
	}
	rows := m.modelPicker.rows()
	if m.modelPicker.cursor < 0 || m.modelPicker.cursor >= len(rows) {
		t.Fatalf("cursor %d out of range (%d rows)", m.modelPicker.cursor, len(rows))
	}
	return rows[m.modelPicker.cursor]
}

func TestModelPicker_CtrlMOpensSeededOnCurrentModel(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m.providerModel = "gemini-2.5-pro"
	m = stepKey(t, m, pressKey('m', tea.ModCtrl))
	if m.mode != modeModelPicker {
		t.Fatalf("mode=%v want modeModelPicker", m.mode)
	}
	if m.modelPicker == nil {
		t.Fatal("modelPicker state should be built on open")
	}
	row := selectedRow(t, m)
	if row.kind != modelPickerRowEntry || row.entry.providerID != "vertex" || row.entry.modelID != "gemini-2.5-pro" {
		t.Errorf("cursor should seed on the current provider+model, got %+v", row)
	}
}

func TestModelPicker_CtrlMIgnoredWhileBusy(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m.testBusy = true
	m = stepKey(t, m, pressKey('m', tea.ModCtrl))
	if m.mode == modeModelPicker {
		t.Errorf("Ctrl+M should be a no-op while busy")
	}
}

func TestModelPicker_HeaderSkippingAndWrap(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()

	// Initial selection: first entry under vertex (cursor 1; cursor 0 is header).
	r0 := selectedRow(t, m)
	if r0.kind != modelPickerRowEntry {
		t.Fatalf("initial cursor on non-entry: %+v", r0)
	}

	// Up wraps to the bottom-most entry.
	m = stepKey(t, m, pressSpecial(tea.KeyUp))
	rEnd := selectedRow(t, m)
	if rEnd.kind == modelPickerRowHeader {
		t.Errorf("Up-wrap should not land on a header: %+v", rEnd)
	}

	// Down wraps back to the first entry.
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	rFirst := selectedRow(t, m)
	if rFirst != r0 {
		t.Errorf("Down-wrap should return to first entry: got %+v, want %+v", rFirst, r0)
	}
}

func TestModelPicker_FuzzyFilter(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()

	// Type "flash" to filter
	m = pressText(t, m, "flash")
	rows := m.modelPicker.rows()
	var entryCount int
	for _, r := range rows {
		if r.kind == modelPickerRowEntry {
			entryCount++
			if !strings.Contains(strings.ToLower(r.entry.modelID), "flash") {
				t.Errorf("unexpected entry in filtered rows: %+v", r)
			}
		}
	}
	if entryCount == 0 {
		t.Error("expected at least 1 matching entry for 'flash'")
	}
}

func TestModelPicker_CustomRowOpensEditorAndApplies(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyUp)) // wrap to custom row
	if row := selectedRow(t, m); row.kind != modelPickerRowCustom {
		t.Fatalf("setup: expected custom row, got %+v", row)
	}
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.modelPicker.customEntry == nil || m.modelPicker.customEntry.providerID != "custom" {
		t.Fatalf("custom editor should open for custom, got %+v", m.modelPicker.customEntry)
	}
	m = pressText(t, m, "my-model-x")
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.provider.ID() != "custom" || m.providerModel != "my-model-x" {
		t.Errorf("custom pick should apply, got %s %q", m.provider.ID(), m.providerModel)
	}
	cfg, _ := loadConfig()
	if len(cfg.RecentModels) != 1 || cfg.RecentModels[0].Model != "my-model-x" {
		t.Errorf("custom pick should land in recents: %+v", cfg.RecentModels)
	}
}

func TestModelPicker_CustomEditorEmptyEnterAndEsc(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyUp))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter)) // empty submit
	if m.modelPicker == nil || m.modelPicker.customEntry == nil {
		t.Fatalf("empty custom submit should keep the editor open")
	}
	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	if m.modelPicker.customEntry != nil || m.mode != modeModelPicker {
		t.Errorf("Esc should pop the editor back to the list")
	}
}

func TestModelPicker_PasteRoutesToActiveInput(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()

	mi, _ := m.Update(tea.PasteMsg{Content: "flash"})
	m = mi.(model)
	if m.modelPicker.query != "flash" {
		t.Errorf("paste should land in the filter query, got %q", m.modelPicker.query)
	}
	m.modelPicker.query = ""
	m = stepKey(t, m, pressSpecial(tea.KeyUp)) // custom row
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	mi, _ = m.Update(tea.PasteMsg{Content: "pasted-id"})
	m = mi.(model)
	if m.modelPicker.customEntry.draft != "pasted-id" {
		t.Errorf("paste should land in the custom draft, got %q", m.modelPicker.customEntry.draft)
	}
}

func TestFriendlyModelName_CatalogAndFallback(t *testing.T) {
	cases := []struct{ provider, id, want string }{
		{vertexProviderID, "gemini-2.5-pro", "Gemini 2.5 Pro"},
		{vertexProviderID, "totally-unknown-model", "Totally Unknown Model"},
		{"no-such-provider", "some_model_id", "Some Model Id"},
	}
	for _, c := range cases {
		if got := friendlyModelName(c.provider, c.id); got != c.want {
			t.Errorf("friendlyModelName(%s,%s)=%q want %q", c.provider, c.id, got, c.want)
		}
	}
}

func TestModelPickerFuzzyMatch(t *testing.T) {
	cases := []struct {
		query, target string
		want          bool
	}{
		{"", "anything", true},
		{"gemini", "Google Gemini 2.5 Pro gemini-2.5-pro", true},
		{"g e m", "gemini-2.5-pro", true},
		{"flash", "Gemini Flash", true},
		{"flsh", "flash", true},
		{"xyz", "gemini", false},
		{"GEMINI", "gemini-2.5-pro", true},
	}
	for _, c := range cases {
		if got := modelPickerFuzzyMatch(c.query, c.target); got != c.want {
			t.Errorf("fuzzy(%q,%q)=%v want %v", c.query, c.target, got, c.want)
		}
	}
}

func TestMissingAPIKeyError_NamesPickerAndEnv(t *testing.T) {
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	err := missingAPIKeyError("FOO_API_KEY")
	for _, want := range []string{"model picker", "ctrl+m", "FOO_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	km := DefaultKeyMap()
	km[ActionProviderSwitch] = KeyBinding{}
	setKeyMapForTesting(km)
	err = missingAPIKeyError("FOO_API_KEY")
	if strings.Contains(err.Error(), "(") || !strings.Contains(err.Error(), "model picker") {
		t.Errorf("unbound hint should drop the key parenthetical: %v", err)
	}
}

func TestModelPicker_ViewIsWiderThanTall(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m.width, m.height = 120, 40
	m = m.openModelPicker()

	v := m.viewModelPicker()
	w, h := lipgloss.Width(v), lipgloss.Height(v)
	if w < modelPickerMinWidth {
		t.Errorf("picker width=%d want >= %d", w, modelPickerMinWidth)
	}
	if w <= h {
		t.Errorf("picker should be wider than tall, got %dx%d", w, h)
	}

	m.modelPicker.keyEntry = &modelPickerKeyEntry{
		pending: modelPickerEntry{display: "X"},
		spec:    providerKeySpec{id: "test", title: "Test"},
	}
	kv := m.viewModelPicker()
	kw, kh := lipgloss.Width(kv), lipgloss.Height(kv)
	if kw < modelPickerMinWidth || kw <= kh {
		t.Errorf("key prompt should stay wide, got %dx%d", kw, kh)
	}
}

func TestModelPicker_VisibleRowsCapped(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m.width, m.height = 120, 60
	m = m.openModelPicker()
	v := m.viewModelPicker()
	maxH := modelPickerMaxRows + 12
	if h := lipgloss.Height(v); h > maxH {
		t.Errorf("picker height=%d should stay flat (<= %d)", h, maxH)
	}
}

func TestModelPicker_CtrlDClosesTab(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	mi, cmd := m.Update(pressKey('d', tea.ModCtrl))
	if cmd == nil {
		t.Fatal("Ctrl+D should dispatch closeTabCmd")
	}
	if msg, ok := cmd().(closeTabMsg); !ok || msg.tabID != m.id {
		t.Errorf("Ctrl+D should close this tab, got %T", cmd())
	}
	_ = mi
}

func newProviderRegistryFixture(t *testing.T) model {
	t.Helper()
	isolateHome(t)
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	withRegisteredProviders(t, vertexAgentProvider())
	m := newTestModel(t, vertexAgentProvider())
	return m
}

func TestModelPicker_VertexAppearsAsSection(t *testing.T) {
	m := newProviderRegistryFixture(t)
	m = m.openModelPicker()
	rows := m.modelPicker.rows()
	var hasHeader, hasEntry bool
	for _, r := range rows {
		if r.kind == modelPickerRowHeader && r.title == "Vertex AI" {
			hasHeader = true
		}
		if r.kind == modelPickerRowEntry && r.entry.providerID == vertexProviderID &&
			r.entry.modelID == vertexDefaultModel {
			hasEntry = true
		}
	}
	if !hasHeader {
		t.Error("model picker must surface a Vertex AI section header")
	}
	if !hasEntry {
		t.Error("model picker must surface the vertex default model entry")
	}
}

func TestModelPicker_OpenRouterKeyPrompt(t *testing.T) {
	m := newProviderRegistryFixture(t)
	// We need to inject openrouter provider into the test registry
	withRegisteredProviders(t, vertexAgentProvider(), agentAPIProvider{spec: &providers.OpenRouterSpec})

	m = m.openModelPicker()
	// Filter for openrouter
	m = pressText(t, m, "openrouter")
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	for {
		row := selectedRow(t, m)
		if row.kind == modelPickerRowEntry && row.entry.providerID == providers.OpenRouterProviderID {
			break
		}
		m = stepKey(t, m, pressSpecial(tea.KeyDown))
	}
	
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.modelPicker.keyEntry == nil {
		t.Fatal("expected OpenRouter key prompt to open")
	}

	m = pressText(t, m, "test-or-key")
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	if m.mode != modeInput {
		t.Errorf("expected to return to input mode, got %v", m.mode)
	}

	cfg, _ := loadConfig()
	if cfg.OpenRouter.APIKey != "test-or-key" {
		t.Errorf("expected OpenRouter API key saved, got %q", cfg.OpenRouter.APIKey)
	}
}

func TestModelPicker_VertexNoKeyPrompt(t *testing.T) {
	m := newProviderRegistryFixture(t)
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.Vertex.Project = ""
		return saveConfig(cfg)
	}); err != nil {
		t.Fatal(err)
	}
	m = m.openModelPicker()
	for {
		row := selectedRow(t, m)
		if row.kind == modelPickerRowEntry && row.entry.providerID == vertexProviderID {
			break
		}
		m = stepKey(t, m, pressSpecial(tea.KeyDown))
	}
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.modelPicker != nil && m.modelPicker.keyEntry != nil {
		t.Error("Vertex must not open an inline API-key prompt")
	}
	if m.mode != modeInput {
		t.Errorf("Vertex pick should apply directly; mode=%v", m.mode)
	}
}
