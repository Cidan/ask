package main

import (
	"strings"
	"testing"

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
// seeds a test model pointed at the first one. Neither provider id is
// in providerKeySpecs, so picks apply without the API-key gate.
func modelPickerFixture(t *testing.T) (model, *fakeProvider, *fakeProvider) {
	t.Helper()
	isolateHome(t)
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	p1 := newFakeProvider()
	p1.id = "claude"
	p1.displayName = "Claude"
	p1.modelPicker = ProviderPicker{
		Options: []string{"default", "sonnet", "opus"},
	}
	p2 := newFakeProvider()
	p2.id = "codex"
	p2.displayName = "Codex"
	p2.modelPicker = ProviderPicker{
		Options:     []string{"gpt-5"},
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
	m.providerModel = "opus"
	m = stepKey(t, m, pressKey('m', tea.ModCtrl))
	if m.mode != modeModelPicker {
		t.Fatalf("mode=%v want modeModelPicker", m.mode)
	}
	if m.modelPicker == nil {
		t.Fatal("modelPicker state should be built on open")
	}
	row := selectedRow(t, m)
	if row.kind != modelPickerRowEntry || row.entry.providerID != "claude" || row.entry.modelID != "opus" {
		t.Errorf("cursor should seed on the current provider+model, got %+v", row)
	}
}

func TestModelPicker_CtrlMIgnoredWhileBusy(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m.busy = true
	m = stepKey(t, m, pressKey('m', tea.ModCtrl))
	if m.mode == modeModelPicker {
		t.Errorf("Ctrl+M should be a no-op while busy")
	}
}

func TestModelPicker_EmptyModelSeedsOnProviderHeadRow(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m.providerModel = ""
	m = m.openModelPicker()
	row := selectedRow(t, m)
	if row.entry.providerID != "claude" || row.entry.modelID != "default" {
		t.Errorf("empty model should seed on the provider's first row, got %+v", row)
	}
}

func TestModelPicker_GroupsListProvidersInRegistryOrder(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	rows := m.modelPicker.rows()
	var headers []string
	for _, r := range rows {
		if r.kind == modelPickerRowHeader {
			headers = append(headers, r.title)
		}
	}
	if strings.Join(headers, ",") != "Claude,Codex" {
		t.Errorf("headers=%v want [Claude Codex] (no recents yet)", headers)
	}
	// Codex advertises AllowCustom → its section ends with the custom row.
	last := rows[len(rows)-1]
	if last.kind != modelPickerRowCustom || last.entry.providerID != "codex" {
		t.Errorf("custom affordance missing from codex section, last row %+v", last)
	}
}

func TestModelPicker_NavigationSkipsHeadersAndWraps(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker() // cursor on claude/default (first entry)

	// Walk down through every selectable row: sonnet, opus, gpt-5,
	// custom — then wrap back to the first.
	wantOrder := []string{"sonnet", "opus", "gpt-5", "custom", "default"}
	for _, want := range wantOrder {
		m = stepKey(t, m, pressSpecial(tea.KeyDown))
		row := selectedRow(t, m)
		got := row.entry.modelID
		if row.kind == modelPickerRowCustom {
			got = "custom"
		}
		if got != want {
			t.Fatalf("Down landed on %q want %q", got, want)
		}
	}
	// Up from the first selectable wraps to the last (custom row).
	m = stepKey(t, m, pressSpecial(tea.KeyUp))
	if row := selectedRow(t, m); row.kind != modelPickerRowCustom {
		t.Errorf("Up from first should wrap to the custom row, got %+v", row)
	}
}

func TestModelPicker_EmacsListNav(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	m = stepKey(t, m, pressKey('n', tea.ModCtrl))
	if row := selectedRow(t, m); row.entry.modelID != "sonnet" {
		t.Errorf("Ctrl+N should advance, got %+v", row)
	}
	m = stepKey(t, m, pressKey('p', tea.ModCtrl))
	if row := selectedRow(t, m); row.entry.modelID != "default" {
		t.Errorf("Ctrl+P should retreat, got %+v", row)
	}
}

func TestModelPicker_FilterNarrowsAcrossProviders(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	m = pressText(t, m, "gpt")
	rows := m.modelPicker.rows()
	for _, r := range rows {
		if r.kind == modelPickerRowEntry && r.entry.modelID != "gpt-5" {
			t.Errorf("filter 'gpt' leaked entry %+v", r.entry)
		}
		if r.kind == modelPickerRowHeader && r.title == "Claude" {
			t.Errorf("claude section should vanish under 'gpt' filter")
		}
		if r.kind == modelPickerRowCustom {
			t.Errorf("custom rows should hide while filtering")
		}
	}
	if row := selectedRow(t, m); row.entry.modelID != "gpt-5" {
		t.Errorf("cursor should reset to first match, got %+v", row)
	}
	// Backspace restores the full list.
	for range "gpt" {
		m = stepKey(t, m, pressSpecial(tea.KeyBackspace))
	}
	if got := len(m.modelPicker.rows()); got < 7 {
		t.Errorf("clearing the filter should restore all rows, got %d", got)
	}
}

func TestModelPicker_FilterMatchesProviderName(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	m = pressText(t, m, "codex")
	var entries int
	for _, r := range m.modelPicker.rows() {
		if r.kind == modelPickerRowEntry {
			entries++
			if r.entry.providerID != "codex" {
				t.Errorf("provider-name filter leaked %+v", r.entry)
			}
		}
	}
	if entries == 0 {
		t.Error("filtering by provider name should keep that provider's models")
	}
}

func TestModelPicker_EnterAppliesCrossProviderSwitch(t *testing.T) {
	m, _, p2 := modelPickerFixture(t)
	m.sessionID = "old-session"
	m.resumeCwd = "/somewhere"
	if err := saveConfig(askConfig{Provider: "claude"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m = m.openModelPicker()
	m = pressText(t, m, "gpt")
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	if m.provider == nil || m.provider.ID() != p2.ID() {
		t.Fatalf("provider after pick should be codex, got %v", m.provider)
	}
	if m.providerModel != "gpt-5" {
		t.Errorf("providerModel=%q want gpt-5", m.providerModel)
	}
	if m.sessionID != "" || m.resumeCwd != "" {
		t.Errorf("cross-provider pick should clear session state, got s=%q r=%q", m.sessionID, m.resumeCwd)
	}
	if m.mode != modeInput {
		t.Errorf("mode after apply=%v want modeInput", m.mode)
	}
	if m.modelPicker != nil {
		t.Errorf("picker state should clear on apply")
	}
	// The picked model persists as codex's default model…
	if p2.settings.Model != "gpt-5" {
		t.Errorf("pick should persist provider default model, got %q", p2.settings.Model)
	}
	// …but the persisted default *provider* is untouched.
	cfg, _ := loadConfig()
	if cfg.Provider != "claude" {
		t.Errorf("cfg.Provider=%q want claude (pick must not change the default provider)", cfg.Provider)
	}
}

func TestModelPicker_SameProviderModelKeepsSession(t *testing.T) {
	m, p1, _ := modelPickerFixture(t)
	m.sessionID = "keep-this-id"
	m.resumeCwd = "/work/here"
	m.worktreeName = "feat-x"
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyDown)) // sonnet
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	if m.provider.ID() != p1.ID() {
		t.Fatalf("provider should stay claude, got %s", m.provider.ID())
	}
	if m.providerModel != "sonnet" {
		t.Errorf("providerModel=%q want sonnet", m.providerModel)
	}
	if m.sessionID != "keep-this-id" || m.resumeCwd != "/work/here" || m.worktreeName != "feat-x" {
		t.Errorf("same-provider pick must keep session state; got s=%q r=%q w=%q",
			m.sessionID, m.resumeCwd, m.worktreeName)
	}
	if len(p1.savedState) != 1 || p1.settings.Model != "sonnet" {
		t.Errorf("pick should persist the provider's default model; saves=%d model=%q",
			len(p1.savedState), p1.settings.Model)
	}
}

func TestModelPicker_DefaultRowClearsModel(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m.providerModel = "opus"
	m = m.openModelPicker()
	m.modelPicker.seedCursor("claude", "default")
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.providerModel != "" {
		t.Errorf("picking the 'default' row should clear the model, got %q", m.providerModel)
	}
}

func TestModelPicker_EscAndCtrlCCancelWithoutChange(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{pressSpecial(tea.KeyEsc), pressKey('c', tea.ModCtrl)} {
		m, p1, _ := modelPickerFixture(t)
		m = m.openModelPicker()
		m = stepKey(t, m, pressSpecial(tea.KeyDown))
		m = stepKey(t, m, key)
		if m.mode != modeInput || m.modelPicker != nil {
			t.Errorf("cancel should close the picker; mode=%v state=%v", m.mode, m.modelPicker)
		}
		if m.provider.ID() != p1.ID() || m.providerModel != "" {
			t.Errorf("cancel must not change provider/model: %s %q", m.provider.ID(), m.providerModel)
		}
	}
}

func TestModelPicker_PickRecordsRecentAndResurfaces(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	m = pressText(t, m, "gpt")
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	cfg, _ := loadConfig()
	if len(cfg.RecentModels) != 1 || cfg.RecentModels[0] != (recentModelRef{Provider: "codex", Model: "gpt-5"}) {
		t.Fatalf("pick should record a recent ref, got %+v", cfg.RecentModels)
	}

	// Reopen: "Recently used" leads, carrying the picked entry.
	m = m.openModelPicker()
	rows := m.modelPicker.rows()
	if rows[0].kind != modelPickerRowHeader || rows[0].title != "Recently used" {
		t.Fatalf("first group should be Recently used, got %+v", rows[0])
	}
	if rows[1].kind != modelPickerRowEntry || rows[1].entry.modelID != "gpt-5" || !rows[1].recent {
		t.Errorf("recent entry missing/unmarked: %+v", rows[1])
	}
	// The cursor seeds on the current provider+model — which now
	// matches the recent row first.
	if row := selectedRow(t, m); !row.recent || row.entry.modelID != "gpt-5" {
		t.Errorf("cursor should seed on the recent row, got %+v", row)
	}
}

func TestRecordRecentModel_PushFrontDedupeCap(t *testing.T) {
	isolateHome(t)
	for _, mdl := range []string{"a", "b", "c", "d", "e", "f"} {
		recordRecentModel("claude", mdl)
	}
	recordRecentModel("claude", "d") // dedupe + move to front
	cfg, _ := loadConfig()
	if len(cfg.RecentModels) != maxRecentModels {
		t.Fatalf("recents should cap at %d, got %d", maxRecentModels, len(cfg.RecentModels))
	}
	var got []string
	for _, r := range cfg.RecentModels {
		got = append(got, r.Model)
	}
	if strings.Join(got, ",") != "d,f,e,c,b" {
		t.Errorf("recents order=%v want [d f e c b]", got)
	}
	// Blank ids are ignored.
	recordRecentModel("", "x")
	recordRecentModel("claude", "")
	cfg, _ = loadConfig()
	if len(cfg.RecentModels) != maxRecentModels || cfg.RecentModels[0].Model != "d" {
		t.Errorf("blank ids must not be recorded: %+v", cfg.RecentModels)
	}
}

func TestModelPicker_RecentWithCustomModelIDIsSynthesized(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	if err := saveConfig(askConfig{RecentModels: []recentModelRef{
		{Provider: "codex", Model: "my-custom-id"},
		{Provider: "gone-provider", Model: "x"},
	}}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m = m.openModelPicker()
	rows := m.modelPicker.rows()
	if rows[0].title != "Recently used" {
		t.Fatalf("recents should lead, got %+v", rows[0])
	}
	if rows[1].entry.modelID != "my-custom-id" || rows[1].entry.providerID != "codex" {
		t.Errorf("custom recent should be synthesized, got %+v", rows[1].entry)
	}
	// The unknown provider's ref is dropped, so exactly one recent row.
	if rows[2].kind == modelPickerRowEntry {
		t.Errorf("unknown-provider recent should be dropped, got %+v", rows[2])
	}
}

// ---- API key gate ----

// keyGateFixture registers a fake provider under the real anthropic id
// so providerKeySpecByID engages, with no key in env or config.
func keyGateFixture(t *testing.T) model {
	t.Helper()
	isolateHome(t)
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	t.Setenv(anthropicEnvAPIKey, "")
	p := newFakeProvider()
	p.id = anthropicProviderID
	p.displayName = "Anthropic"
	p.modelPicker = ProviderPicker{Options: []string{"claude-fable-5"}, AllowCustom: true}
	withRegisteredProviders(t, p)
	return newTestModel(t, p)
}

func TestModelPicker_MissingKeyOpensInlinePrompt(t *testing.T) {
	m := keyGateFixture(t)
	m = m.openModelPicker()

	// The section header advertises the missing key.
	var header modelPickerRow
	for _, r := range m.modelPicker.rows() {
		if r.kind == modelPickerRowHeader {
			header = r
			break
		}
	}
	if header.note != "no API key" {
		t.Errorf("header note=%q want 'no API key'", header.note)
	}

	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	ke := m.modelPicker.keyEntry
	if ke == nil {
		t.Fatal("Enter on a keyless provider's model should open the key prompt")
	}
	if ke.spec.id != anthropicProviderID || ke.pending.modelID != "claude-fable-5" {
		t.Errorf("key prompt state wrong: %+v", ke)
	}
	if m.provider.ID() != anthropicProviderID || m.mode != modeModelPicker {
		t.Errorf("pick must not apply before the key lands")
	}
}

func TestModelPicker_KeyPromptSavesAndApplies(t *testing.T) {
	m := keyGateFixture(t)
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	m = pressText(t, m, "sk-ant-test")
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	cfg, _ := loadConfig()
	if cfg.Anthropic.APIKey != "sk-ant-test" {
		t.Fatalf("key should persist to cfg.Anthropic.APIKey, got %q", cfg.Anthropic.APIKey)
	}
	if m.mode != modeInput || m.providerModel != "claude-fable-5" {
		t.Errorf("pick should apply after key save; mode=%v model=%q", m.mode, m.providerModel)
	}
}

func TestModelPicker_KeyPromptEmptyEnterIsNoop(t *testing.T) {
	m := keyGateFixture(t)
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.modelPicker == nil || m.modelPicker.keyEntry == nil {
		t.Errorf("empty key submit should keep the prompt open")
	}
}

func TestModelPicker_KeyPromptEscReturnsToList(t *testing.T) {
	m := keyGateFixture(t)
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	if m.mode != modeModelPicker || m.modelPicker.keyEntry != nil {
		t.Errorf("Esc should pop back to the list, mode=%v keyEntry=%v", m.mode, m.modelPicker.keyEntry)
	}
	cfg, _ := loadConfig()
	if cfg.Anthropic.APIKey != "" {
		t.Errorf("Esc must not persist a key, got %q", cfg.Anthropic.APIKey)
	}
}

func TestModelPicker_ConfiguredKeySkipsPrompt(t *testing.T) {
	m := keyGateFixture(t)
	if err := saveConfig(askConfig{Anthropic: apiProviderConfig{APIKey: "sk-have"}}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.mode != modeInput || m.providerModel != "claude-fable-5" {
		t.Errorf("configured key should apply directly; mode=%v model=%q", m.mode, m.providerModel)
	}
}

func TestModelPicker_EnvKeySkipsPrompt(t *testing.T) {
	m := keyGateFixture(t)
	t.Setenv(anthropicEnvAPIKey, "sk-env")
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.mode != modeInput {
		t.Errorf("env key should satisfy the gate; mode=%v", m.mode)
	}
}

// ---- custom model entry ----

func TestModelPicker_CustomRowOpensEditorAndApplies(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyUp)) // wrap to codex custom row
	if row := selectedRow(t, m); row.kind != modelPickerRowCustom {
		t.Fatalf("setup: expected custom row, got %+v", row)
	}
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.modelPicker.customEntry == nil || m.modelPicker.customEntry.providerID != "codex" {
		t.Fatalf("custom editor should open for codex, got %+v", m.modelPicker.customEntry)
	}
	m = pressText(t, m, "my-model-x")
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.provider.ID() != "codex" || m.providerModel != "my-model-x" {
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

// ---- paste routing ----

func TestModelPicker_PasteRoutesToActiveInput(t *testing.T) {
	m, _, _ := modelPickerFixture(t)
	m = m.openModelPicker()

	mi, _ := m.Update(tea.PasteMsg{Content: "gpt"})
	m = mi.(model)
	if m.modelPicker.query != "gpt" {
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

func TestModelPicker_PasteRoutesToKeyDraft(t *testing.T) {
	m := keyGateFixture(t)
	m = m.openModelPicker()
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	mi, _ := m.Update(tea.PasteMsg{Content: "sk-paste"})
	m = mi.(model)
	if m.modelPicker.keyEntry.draft != "sk-paste" {
		t.Errorf("paste should land in the key draft, got %q", m.modelPicker.keyEntry.draft)
	}
}

// ---- friendly names + fuzzy matching ----

func TestFriendlyModelName_CatalogAndFallback(t *testing.T) {
	cases := []struct{ provider, id, want string }{
		{anthropicProviderID, "claude-fable-5", "Claude Fable 5"},
		{deepseekProviderID, "deepseek-v4-pro", "DeepSeek V4 Pro"}, // catalog dashes → spaces
		{openaiProviderID, "gpt-5.5", "GPT 5.5"},
		{anthropicProviderID, "totally-unknown-model", "Totally Unknown Model"}, // humanized
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
		{"gpt", "OpenAI GPT 5.5 gpt-5.5", true},
		{"g p t", "gpt-5.5", true}, // spaces ignored
		{"sonnet", "Claude Sonnet 4.6", true},
		{"snt", "sonnet", true}, // subsequence
		{"xyz", "sonnet", false},
		{"GPT", "gpt-5.5", true}, // case-insensitive
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
	// Unbound picker action drops the key hint but keeps the pointer.
	km := DefaultKeyMap()
	km[ActionProviderSwitch] = KeyBinding{}
	setKeyMapForTesting(km)
	err = missingAPIKeyError("FOO_API_KEY")
	if strings.Contains(err.Error(), "(") || !strings.Contains(err.Error(), "model picker") {
		t.Errorf("unbound hint should drop the key parenthetical: %v", err)
	}
}

// Design popups prefer a landscape silhouette: comfortably wide,
// capped row count. Dimension math only — no content snapshots.
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

	// The inline key prompt keeps the same silhouette.
	m.modelPicker.keyEntry = &modelPickerKeyEntry{
		pending: modelPickerEntry{display: "X"},
		spec:    providerKeySpecs[0],
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
	// chrome: title + blank + search + blank + blank + help inside the
	// border (2) and vertical padding (2).
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

// newProviderRegistryFixture seeds the model picker with the real
// googleai + vertex providers (alongside the kimi fake from the
// existing fixture) so the picker headers + groups cover the new
// providers end-to-end.
func newProviderRegistryFixture(t *testing.T) model {
	t.Helper()
	isolateHome(t)
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	// Ensure the kimi fake doesn't gate on a key for the picker test
	// (it would surface as "no API key" notes on the header).
	t.Setenv(moonshotEnvAPIKey, "fake-kimi-key")
	t.Setenv(googleaiEnvAPIKey, "fake-googleai-key")
	pKimi := newFakeProvider()
	pKimi.id = kimiProviderID
	pKimi.displayName = "Kimi (Moonshot)"
	pKimi.modelPicker = ProviderPicker{Options: []string{"kimi-k2.7-code"}}
	withRegisteredProviders(t, googleaiAgentProvider(), vertexAgentProvider(), pKimi)
	m := newTestModel(t, pKimi)
	return m
}

func TestModelPicker_GoogleAIAppearsAsSection(t *testing.T) {
	m := newProviderRegistryFixture(t)
	m = m.openModelPicker()
	rows := m.modelPicker.rows()
	var hasHeader, hasEntry bool
	for _, r := range rows {
		if r.kind == modelPickerRowHeader && r.title == "Google AI Studio" {
			hasHeader = true
		}
		if r.kind == modelPickerRowEntry && r.entry.providerID == googleaiProviderID &&
			r.entry.modelID == googleaiDefaultModel {
			hasEntry = true
		}
	}
	if !hasHeader {
		t.Error("model picker must surface a Google AI Studio section header")
	}
	if !hasEntry {
		t.Error("model picker must surface the googleai default model entry")
	}
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

func TestModelPicker_VertexNoKeyPrompt(t *testing.T) {
	// Vertex auth is configured in the /config → Vertex AI submenu,
	// not via the inline API-key prompt. Picking a Vertex model
	// without a configured project should apply directly (and surface
	// a "project is required" error on session start, not on the pick).
	m := newProviderRegistryFixture(t)
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.Vertex.Project = "" // explicit empty
		return saveConfig(cfg)
	}); err != nil {
		t.Fatal(err)
	}
	m = m.openModelPicker()
	// Walk to the Vertex section (Google AI Studio is registered first
	// in the fixture, then Vertex, then Kimi).
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
