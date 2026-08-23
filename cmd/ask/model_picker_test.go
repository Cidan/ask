package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/providers"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
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
	resetModelCatalog()
	t.Cleanup(resetModelCatalog)
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

// The overlay is the terminal minus a fixed margin, square-cornered, and
// keeps the same footprint whichever sub-editor (key prompt, custom id) is
// up — the right column swaps content, the box never changes shape.
func TestModelPicker_OverlayGeometry(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()

	cases := []struct{ w, h, boxW, boxH int }{
		{120, 40, 112, 36}, // 4-col / 2-row margins
		{80, 24, 78, 22},   // small terminal: margins collapse to 1
	}
	for _, c := range cases {
		boxW, boxH, listW, detailW, innerH := modelPickerGeometry(c.w, c.h)
		if boxW != c.boxW || boxH != c.boxH {
			t.Errorf("%dx%d: box=%dx%d want %dx%d", c.w, c.h, boxW, boxH, c.boxW, c.boxH)
		}
		if listW+3+detailW != boxW-4 || innerH != boxH-2 {
			t.Errorf("%dx%d: columns %d+3+%d and innerH %d must fill the box %dx%d", c.w, c.h, listW, detailW, innerH, boxW, boxH)
		}
		if listW < modelPickerMinListW || detailW < modelPickerMinDetailW {
			t.Errorf("%dx%d: columns too narrow: list=%d detail=%d", c.w, c.h, listW, detailW)
		}

		ov := m.modelPickerOverlay(c.w, c.h)
		if gw, gh := lipgloss.Width(ov), lipgloss.Height(ov); gw != boxW || gh != boxH {
			t.Errorf("%dx%d: overlay renders %dx%d want %dx%d", c.w, c.h, gw, gh, boxW, boxH)
		}
		plain := xansi.Strip(ov)
		if !strings.HasPrefix(plain, "┌") || !strings.HasSuffix(plain, "┘") {
			t.Errorf("%dx%d: overlay must have square corners, got %q…%q", c.w, c.h, plain[:3], plain[len(plain)-3:])
		}
		if !strings.Contains(plain, "│") {
			t.Errorf("%dx%d: overlay must carry the column divider", c.w, c.h)
		}
	}

	m.modelPicker.keyEntry = &modelPickerKeyEntry{
		pending: modelPickerEntry{display: "X", providerName: "Test"},
		field:   providers.SettingField{Key: "apiKey", Title: "API Key", Secret: true, EnvKey: "TEST_KEY"},
	}
	if kv := m.modelPickerOverlay(120, 40); lipgloss.Width(kv) != 112 || lipgloss.Height(kv) != 36 {
		t.Errorf("key prompt must keep the box footprint, got %dx%d", lipgloss.Width(kv), lipgloss.Height(kv))
	}
	m.modelPicker.keyEntry = nil
	m.modelPicker.customEntry = &modelPickerCustomEntry{providerID: "custom", providerName: "Custom"}
	if cv := m.modelPickerOverlay(120, 40); lipgloss.Width(cv) != 112 || lipgloss.Height(cv) != 36 {
		t.Errorf("custom editor must keep the box footprint, got %dx%d", lipgloss.Width(cv), lipgloss.Height(cv))
	}

	m = m.closeModelPicker()
	if ov := m.modelPickerOverlay(120, 40); ov != "" {
		t.Error("a closed picker renders no overlay")
	}
}

func TestModelPickerWindow(t *testing.T) {
	cases := []struct{ n, cursor, size, start, end int }{
		{5, 0, 10, 0, 5},
		{30, 0, 10, 0, 10},
		{30, 9, 10, 0, 10},
		{30, 10, 10, 1, 11},
		{30, 29, 10, 20, 30},
		{0, 0, 10, 0, 0},
	}
	for _, c := range cases {
		if s, e := modelPickerWindow(c.n, c.cursor, c.size); s != c.start || e != c.end {
			t.Errorf("window(n=%d,cursor=%d,size=%d)=[%d,%d) want [%d,%d)", c.n, c.cursor, c.size, s, e, c.start, c.end)
		}
	}
}

// The picker is drawn by the app over the joined body+sidebar frame, so its
// right border lands inside the sidebar column, not just the tab body.
func TestApp_ModelPickerOverlayCoversSidebar(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	a := app{tabs: []*model{&m}, active: 0, nextID: 2, width: 120, height: 40}
	if !a.sidebarVisible() {
		t.Fatal("fixture must have a visible sidebar")
	}
	mx, my := modelPickerMargins(a.width, a.height)
	lines := strings.Split(a.View().Content, "\n")
	if len(lines) <= my {
		t.Fatalf("frame has %d lines", len(lines))
	}
	row := []rune(xansi.Strip(lines[my]))
	rightX := a.width - mx - 1
	if len(row) <= rightX {
		t.Fatalf("row %d is %d cells wide, want >= %d", my, len(row), rightX+1)
	}
	if row[mx] != '┌' || row[rightX] != '┐' {
		t.Errorf("overlay corners not at columns %d/%d: %q", mx, rightX, string(row))
	}
	if rightX < a.bodyWidth() {
		t.Errorf("right corner at %d must sit inside the sidebar column (body width %d)", rightX, a.bodyWidth())
	}
	if a.View().Cursor != nil {
		t.Error("the frame cursor must be hidden while the picker is up")
	}
}

func TestModelPicker_NaturalSortWithinProvider(t *testing.T) {
	m, p1, _ := modelPickerFixture(t)
	p1.modelPicker = ProviderPicker{Options: []string{"gemini-3-pro", "gemini-10-x", "gemini-2.5-flash", "gemini-3.1-pro"}}
	m = m.openModelPicker()
	var ids []string
	for _, r := range m.modelPicker.rows() {
		if r.kind == modelPickerRowEntry && r.entry.providerID == "vertex" {
			ids = append(ids, r.entry.modelID)
		}
	}
	want := []string{"gemini-2.5-flash", "gemini-3-pro", "gemini-3.1-pro", "gemini-10-x"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("provider entries must be natural-sorted: got %v want %v", ids, want)
	}
}

func TestModelPicker_RecentlyUsedFirstInRecencyOrder(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	recordRecentModel("vertex", "gemini-2.5-flash")
	recordRecentModel("vertex", "gemini-2.5-pro")
	m = m.openModelPicker()
	rows := m.modelPicker.rows()
	if len(rows) < 3 || rows[0].kind != modelPickerRowHeader || rows[0].title != "Recently used" {
		t.Fatalf("first section must be Recently used, got %+v", rows[:min(len(rows), 3)])
	}
	if rows[1].entry.modelID != "gemini-2.5-pro" || rows[2].entry.modelID != "gemini-2.5-flash" || !rows[1].recent {
		t.Errorf("recents keep recency order (newest first), got %q then %q", rows[1].entry.modelID, rows[2].entry.modelID)
	}
}

func TestModelPicker_CatalogLoadRebuildsRowsAndKeepsCursor(t *testing.T) {
	m := newProviderRegistryFixture(t)
	resetModelCatalog()
	t.Cleanup(resetModelCatalog)
	m = m.openModelPicker()
	m.modelPicker.seedCursor(vertexProviderID, "gemini-2.5-flash")
	m.modelPicker.loading = true

	hasID := func(id string) bool {
		for _, r := range m.modelPicker.rows() {
			if r.kind == modelPickerRowEntry && r.entry.modelID == id {
				return true
			}
		}
		return false
	}
	if hasID("gemini-9-new") {
		t.Fatal("setup: live-only id must be absent before the load")
	}

	cacheModelOptions(map[string][]string{vertexProviderID: {"gemini-2.5-pro", "gemini-2.5-flash", "gemini-9-new"}})
	mi, _ := m.Update(modelCatalogLoadedMsg{})
	m = mi.(model)

	if !hasID("gemini-9-new") {
		t.Error("rows must rebuild from the live listing after the load lands")
	}
	if row := selectedRow(t, m); row.entry.modelID != "gemini-2.5-flash" {
		t.Errorf("cursor must stay on the selected model across a rebuild, got %+v", row)
	}
	if m.modelPicker.loading {
		t.Error("loading must clear when the catalog message lands")
	}
}

func TestModelPicker_OpenDispatchesCatalogLoadOnce(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	prev := loadModelsDev
	loadModelsDev = func(context.Context) error { return nil }
	t.Cleanup(func() { loadModelsDev = prev })

	mi, cmd := m.Update(pressKey('m', tea.ModCtrl))
	m = mi.(model)
	if cmd == nil {
		t.Fatal("first Ctrl+M must dispatch the catalog load")
	}
	if !m.modelPicker.loading {
		t.Error("picker must show loading while the load is in flight")
	}
	msg, ok := cmd().(modelCatalogLoadedMsg)
	if !ok {
		t.Fatalf("load cmd must yield modelCatalogLoadedMsg, got %T", msg)
	}
	mi, _ = m.Update(msg)
	m = mi.(model)
	if m.modelPicker.loading {
		t.Error("loading must clear once the load lands")
	}

	m = m.closeModelPicker()
	mi, cmd = m.Update(pressKey('m', tea.ModCtrl))
	m = mi.(model)
	if cmd != nil {
		t.Error("a loaded catalog must not be refetched on the next open")
	}
	if _, cmd := m.Update(pressKey('r', tea.ModCtrl)); cmd == nil {
		t.Error("Ctrl+R must force a refresh")
	}
}

func TestModelPicker_DetailPane(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	s := m.modelPicker

	known := xansi.Strip(strings.Join(s.modelDetailLines(modelPickerEntry{
		providerID: vertexProviderID, providerName: "Vertex AI", modelID: "gemini-2.5-pro", display: "Gemini 2.5 Pro",
	}, 60), "\n"))
	for _, want := range []string{"Gemini 2.5 Pro", "Vertex AI · gemini-2.5-pro", "Context", "1,048,576 tokens", "Max output"} {
		if !strings.Contains(known, want) {
			t.Errorf("catalog model detail must mention %q:\n%s", want, known)
		}
	}
	if strings.Contains(known, "no information available") {
		t.Error("a catalog hit must not claim no information")
	}

	unknown := xansi.Strip(strings.Join(s.modelDetailLines(modelPickerEntry{
		providerID: "custom", providerName: "Custom", modelID: "my-model", display: "My Model",
	}, 60), "\n"))
	if !strings.Contains(unknown, "no information available") || !strings.Contains(unknown, "Custom · my-model") {
		t.Errorf("unknown model must show its identity plus the no-information line:\n%s", unknown)
	}

	// Scrolling clamps: PgDn past the end never loses the hint row.
	s.detailScroll = 1000
	lines := m.renderModelPickerDetail(s, 60, 10)
	if len(lines) != 10 {
		t.Errorf("detail pane must fill its height exactly, got %d lines", len(lines))
	}
	if !strings.Contains(xansi.Strip(lines[len(lines)-1]), "refresh") {
		t.Errorf("last detail line must be the hint row, got %q", xansi.Strip(lines[len(lines)-1]))
	}
}

func TestModelDetailFacts(t *testing.T) {
	full := providers.ModelMeta{
		ContextWindow:   1_048_576,
		MaxOutputTokens: 65_536,
		Pricing:         &providers.ModelPricing{InputPer1M: 0.75, OutputPer1M: 3.75, CachedInputPer1M: 0.075},
		InputModalities: []string{"text", "image"},
		ReasoningLevels: []string{"low", "high"},
		KnowledgeCutoff: "2026-03",
		ReleaseDate:     "2026-08-13",
		Status:          "beta",
	}
	got := modelDetailFacts(full)
	want := []modelFact{
		{"Context", "1,048,576 tokens"},
		{"Max output", "65,536 tokens"},
		{"Input", "$0.75 / 1M"},
		{"Output", "$3.75 / 1M"},
		{"Cached input", "$0.075 / 1M"},
		{"Modalities", "text · image"},
		{"Reasoning", "low · high"},
		{"Knowledge", "2026-03"},
		{"Released", "2026-08-13"},
		{"Status", "beta"},
	}
	if len(got) != len(want) {
		t.Fatalf("facts: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fact %d: got %v want %v", i, got[i], want[i])
		}
	}

	free := modelDetailFacts(providers.ModelMeta{Pricing: &providers.ModelPricing{}, Reasoning: true})
	if len(free) != 2 || free[0] != (modelFact{"Pricing", "free"}) || free[1] != (modelFact{"Reasoning", "yes"}) {
		t.Errorf("free pricing + bare reasoning flag: got %v", free)
	}
	if facts := modelDetailFacts(providers.ModelMeta{}); len(facts) != 0 {
		t.Errorf("empty meta yields no facts, got %v", facts)
	}
}

func TestFormatUSDPer1M(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.75, "$0.75 / 1M"}, {3.75, "$3.75 / 1M"}, {0.075, "$0.075 / 1M"}, {15, "$15.00 / 1M"}, {0.3, "$0.30 / 1M"}, {0.0000833, "$0.0001 / 1M"},
	}
	for _, c := range cases {
		if got := formatUSDPer1M(c.in); got != c.want {
			t.Errorf("formatUSDPer1M(%v)=%q want %q", c.in, got, c.want)
		}
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
	withRegisteredProviders(t, vertexAgentProvider(), agentAPIProvider{prov: providers.OpenRouter{}})
	t.Setenv(providers.OpenRouterEnvAPIKey, "")

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
	if ke := m.modelPicker.keyEntry; ke.field.Key != providers.OpenRouterFieldAPIKey || ke.title() != "OpenRouter API Key" {
		t.Errorf("key prompt must ask for the provider's Secret setting: %+v", ke.field)
	}

	m = pressText(t, m, "test-or-key")
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	if m.mode != modeInput {
		t.Errorf("expected to return to input mode, got %v", m.mode)
	}

	cfg, _ := loadConfig()
	if got := cfg.ProviderConfig(providers.OpenRouterProviderID).Field(providers.OpenRouterFieldAPIKey); got != "test-or-key" {
		t.Errorf("expected OpenRouter API key saved, got %q", got)
	}
	if providerNeedsAPIKey(cfg, providers.OpenRouterProviderID) {
		t.Error("a saved key must satisfy the gate")
	}
}

func TestProviderNeedsAPIKey_EnvAndUnknown(t *testing.T) {
	isolateHome(t)
	t.Setenv(providers.OpenRouterEnvAPIKey, "")
	if !providerNeedsAPIKey(askConfig{}, providers.OpenRouterProviderID) {
		t.Error("no key anywhere → needs one")
	}
	t.Setenv(providers.OpenRouterEnvAPIKey, "env")
	if providerNeedsAPIKey(askConfig{}, providers.OpenRouterProviderID) {
		t.Error("the env fallback satisfies the gate")
	}
	if providerNeedsAPIKey(askConfig{}, providers.VertexProviderID) {
		t.Error("a provider without a Secret setting never prompts")
	}
	if providerNeedsAPIKey(askConfig{}, "nosuch") {
		t.Error("an unknown provider never prompts")
	}
}

func TestModelPicker_VertexNoKeyPrompt(t *testing.T) {
	m := newProviderRegistryFixture(t)
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.SetProviderConfig(vertexProviderID, providerConfig{})
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

// A Ctrl+M pick of another provider's model must survive a restart: the picked
// provider becomes the default for new tabs (cfg.Provider) AND its model is
// persisted. Regression for "picked OpenRouter, relaunched, back on Vertex".
func TestModelPicker_OpenRouterPickPersistsProviderAndModel(t *testing.T) {
	m := newProviderRegistryFixture(t)
	withRegisteredProviders(t, vertexAgentProvider(), agentAPIProvider{prov: providers.OpenRouter{}})

	// Pre-seed the key so the pick applies directly instead of prompting.
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.SetProviderConfig(providers.OpenRouterProviderID, providerConfig{}.WithField(providers.OpenRouterFieldAPIKey, "k"))
		return saveConfig(cfg)
	}); err != nil {
		t.Fatal(err)
	}

	m = m.openModelPicker()
	mi, _ := m.dispatchModelPick(modelPickerEntry{
		providerID:   providers.OpenRouterProviderID,
		providerName: "OpenRouter",
		modelID:      "anthropic/claude-3.7-sonnet",
	})
	if _, ok := mi.(model); !ok {
		t.Fatalf("dispatchModelPick returned %T, want model", mi)
	}

	cfg, _ := loadConfig()
	if cfg.Provider != providers.OpenRouterProviderID {
		t.Errorf("cfg.Provider = %q, want %q (Ctrl+M must set the default for new tabs)",
			cfg.Provider, providers.OpenRouterProviderID)
	}
	if got := cfg.ProviderConfig(providers.OpenRouterProviderID); got.Model != "anthropic/claude-3.7-sonnet" || got.Field(providers.OpenRouterFieldAPIKey) != "k" {
		t.Errorf("openrouter block = %+v, want the model persisted next to the key", got)
	}
}

// SaveSettings must persist every provider's block, not just Vertex — the
// original code copied back only cfg.Vertex, silently dropping the OpenRouter
// model so it never reached disk.
func TestAgentProvider_SaveSettingsPersistsNonVertexModel(t *testing.T) {
	isolateHome(t)
	p := agentAPIProvider{prov: providers.OpenRouter{}}
	if err := p.SaveSettings(ProviderSettings{Model: "openai/o3-mini", Effort: "high"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	cfg, _ := loadConfig()
	if got := cfg.ProviderConfig(providers.OpenRouterProviderID).Model; got != "openai/o3-mini" {
		t.Errorf("openrouter model = %q, want openai/o3-mini", got)
	}
	if cfg.Effort != "high" {
		t.Errorf("cfg.Effort = %q, want high", cfg.Effort)
	}
}
