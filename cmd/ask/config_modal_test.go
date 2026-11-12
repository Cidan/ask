package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

// configFixture is a test tab with /config open and no credentials in the
// environment.
func configFixture(t *testing.T) model {
	t.Helper()
	isolateHome(t)
	for _, k := range []string{providers.VertexEnvCloudProject, providers.VertexEnvApplicationCredentials,
		providers.OpenRouterEnvAPIKey, braveEnvAPIKey, providers.GeminiEnvAPIKey} {
		t.Setenv(k, "")
	}
	m := newTestModel(t, newFakeProvider())
	m.width, m.height = 140, 44
	return m.startConfigModal()
}

// focusConfigRow puts the cursor on the row with id, opening /config when
// it is not up.
func focusConfigRow(t *testing.T, m model, id string) model {
	t.Helper()
	if m.config == nil {
		m = m.startConfigModal()
	}
	for si, sec := range m.configSections() {
		for ri, r := range sec.rows {
			if r.id == id {
				m.config.section, m.config.cursor, m.config.query = si, ri, ""
				return m
			}
		}
	}
	t.Fatalf("no /config row %q", id)
	return m
}

func configRowByID(t *testing.T, m model, id string) configRow {
	t.Helper()
	r, ok := rowByID(m.configSections(), id)
	if !ok {
		t.Fatalf("no /config row %q", id)
	}
	return r
}

func pressConfig(t *testing.T, m model, msgs ...tea.Msg) model {
	t.Helper()
	for _, msg := range msgs {
		m, _ = runUpdate(t, m, msg)
	}
	return m
}

func typeKeys(s string) []tea.Msg {
	out := make([]tea.Msg, 0, len(s))
	for _, r := range s {
		out = append(out, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return out
}

// configEnter focuses id and presses Enter on it.
func configEnter(t *testing.T, m model, id string) model {
	t.Helper()
	return pressConfig(t, focusConfigRow(t, m, id), tea.KeyPressMsg{Code: tea.KeyEnter})
}

// toggleGlobalConfigRow presses Enter on a /config row by id.
func toggleGlobalConfigRow(t *testing.T, m model, rowID string) model {
	t.Helper()
	return configEnter(t, m, rowID)
}

func TestConfigScreen_OpensFullFrameAndCloses(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	mi, _ := m.handleCommand("/config")
	m = mi.(model)
	if m.mode != modeConfig || m.config == nil {
		t.Fatalf("/config must open the screen: mode=%v state=%v", m.mode, m.config)
	}
	if m.configOverlay(140, 44) == "" {
		t.Fatal("the screen draws as a full-frame overlay")
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.mode != modeInput || m.config != nil {
		t.Fatalf("Esc closes the screen: mode=%v", m.mode)
	}
	if m.configOverlay(140, 44) != "" {
		t.Fatal("a closed screen draws nothing")
	}

	m = pressConfig(t, m.startConfigModal(), tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'c'})
	if m.mode != modeInput {
		t.Errorf("Ctrl+C closes the screen: mode=%v", m.mode)
	}
	if _, cmd := runUpdate(t, m.startConfigModal(), tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'd'}); cmd == nil {
		t.Error("Ctrl+D still closes the tab from /config")
	}
}

func TestConfigScreen_EverySettingHasADescribedRow(t *testing.T) {
	m := configFixture(t)
	ids := map[string]configRow{}
	groups := map[string]bool{}
	for _, sec := range m.configSections() {
		if sec.title == "" || sec.blurb == "" || len(sec.rows) == 0 {
			t.Errorf("section %q needs a title, a blurb, and rows", sec.id)
		}
		for _, r := range sec.rows {
			if _, dup := ids[r.id]; dup {
				t.Errorf("row id %q appears twice", r.id)
			}
			ids[r.id] = r
			groups[r.group] = true
			if strings.TrimSpace(r.desc) == "" {
				t.Errorf("row %q has no description", r.id)
			}
		}
	}
	want := []string{"theme", "quiet", "toolOutput", "renderDiffs", "cursorBlink",
		"provider", "skipAllPermissions", "worktree", "autoCompact", "deslop", "deslopModel",
		"decisionModel", "decision:" + providers.GeminiAPIProviderID + ":" + providers.GeminiFieldAPIKey,
		"memoryModel", "memoryGate", "memoryFilter", "memoryBrowse", "braveApiKey",
		"githubAuth", "project:githubMCPEndpoint", "project:githubMCPToken", "project:linearGraphQLEndpoint",
		"project:linearAPIKey", "project:linearTeamKey", "issueProvider", "projectWorktree", "projectLinks", "workflows"}
	for _, p := range providers.All() {
		for _, f := range p.Settings() {
			want = append(want, "provider:"+p.ID()+":"+f.Key)
		}
	}
	for _, a := range actionMeta {
		want = append(want, "key:"+string(a.Action))
	}
	for _, id := range want {
		if _, ok := ids[id]; !ok {
			t.Errorf("missing /config row %q", id)
		}
	}
	for _, g := range actionGroups {
		if !groups[g.Heading] {
			t.Errorf("keybindings are grouped under %q", g.Heading)
		}
	}
}

func TestConfigScreen_NavigationFlowsAcrossSections(t *testing.T) {
	m := configFixture(t)
	secs := m.configSections()
	first := len(secs[0].rows)
	for i := 0; i < first-1; i++ {
		m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.config.section != 0 || m.config.cursor != first-1 {
		t.Fatalf("down walks the section: %d/%d", m.config.section, m.config.cursor)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.config.section != 1 || m.config.cursor != 0 {
		t.Fatalf("down past the last row flows into the next section: %d/%d", m.config.section, m.config.cursor)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	if m.config.section != 0 || m.config.cursor != first-1 {
		t.Fatalf("Ctrl+P back past the first row lands on the previous section's last: %d/%d", m.config.section, m.config.cursor)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	if m.config.section != 1 {
		t.Fatalf("Ctrl+N moves down: %d", m.config.section)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m.config.section != 2 || m.config.cursor != 0 {
		t.Fatalf("Tab jumps to the next section's top: %d/%d", m.config.section, m.config.cursor)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.config.section != len(secs)-1 {
		t.Fatalf("Shift+Tab wraps backwards: %d", m.config.section)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.config.cursor != len(secs[len(secs)-1].rows)-1 {
		t.Errorf("End lands on the last row: %d", m.config.cursor)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.config.section != 0 || m.config.cursor != 0 {
		t.Errorf("down past the very last row wraps to the top: %d/%d", m.config.section, m.config.cursor)
	}
}

func TestConfigScreen_FilterFindsSettingsInEverySection(t *testing.T) {
	m := configFixture(t)
	m = pressConfig(t, m, typeKeys("api key")...)
	page := m.config.page(m.configSections())
	found := map[string]int{}
	for i, pr := range page {
		found[pr.row.id] = i
	}
	for _, id := range []string{"provider:" + providers.OpenRouterProviderID + ":" + providers.OpenRouterFieldAPIKey,
		"decision:" + providers.GeminiAPIProviderID + ":" + providers.GeminiFieldAPIKey, "braveApiKey", "project:linearAPIKey"} {
		if _, ok := found[id]; !ok {
			t.Errorf("filter %q misses %q", m.config.query, id)
		}
	}
	if _, ok := found["theme"]; ok {
		t.Error("an unrelated row must not match")
	}

	m = pressConfig(t, m, tea.KeyPressMsg{Code: 'x', Text: "x", Mod: tea.ModAlt})
	if m.config.query != "api key" {
		t.Errorf("a modified key is not typed into the filter: %q", m.config.query)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.config.query != "api ke" {
		t.Errorf("Backspace removes one rune: %q", m.config.query)
	}

	m.config.cursor = found["braveApiKey"]
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.config.editing != "braveApiKey" {
		t.Fatalf("Enter acts on the filtered row: %q", m.config.editing)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.config.editing != "" || m.config.query != "api ke" {
		t.Fatalf("Esc closes the editor first: editing=%q query=%q", m.config.editing, m.config.query)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.mode != modeConfig || m.config.query != "" {
		t.Fatalf("Esc then clears the filter and keeps the screen: mode=%v query=%q", m.mode, m.config.query)
	}

	m = pressConfig(t, m, typeKeys("brave")...)
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	secs := m.configSections()
	if m.config.query != "" || secs[m.config.section-1].id != "websearch" {
		t.Errorf("Tab leaves the filter for the section after the match: %q %d", m.config.query, m.config.section)
	}
}

func TestConfigScreen_TogglesPersistAndApply(t *testing.T) {
	m := configFixture(t)
	m.quietMode = false
	m.toolOutputMode = toolOutputFull
	m.transcript = []transcriptItem{
		{kind: trAssistant, text: "a"},
		{kind: trToolCall, toolName: "read"},
		{kind: trAssistant, text: "b"},
	}
	(&m).projectHistory()
	m = configEnter(t, m, "quiet")
	if !m.quietMode || len(m.history) != 2 {
		t.Fatalf("quiet reprojects the history at once: quiet=%v entries=%d", m.quietMode, len(m.history))
	}
	if cfg, _ := loadConfig(); cfg.UI.QuietMode == nil || !*cfg.UI.QuietMode {
		t.Errorf("quiet persisted: %+v", cfg.UI)
	}
	if !strings.Contains(m.config.notice, "Quiet mode on") || m.config.noticeErr {
		t.Errorf("the change is acknowledged on screen: %q", m.config.notice)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.quietMode {
		t.Error("←/→ flips a toggle too")
	}

	m = configEnter(t, m, "autoCompact")
	if cfg, _ := loadConfig(); cfg.AutoCompact == nil || *cfg.AutoCompact {
		t.Fatalf("auto-compact is on unless turned off, so the first flip is off: %v", cfg.AutoCompact)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cfg, _ := loadConfig(); cfg.AutoCompact == nil || !*cfg.AutoCompact {
		t.Fatalf("second flip is explicit on: %v", cfg.AutoCompact)
	}

	for _, id := range []string{"memoryGate", "memoryFilter"} {
		m = configEnter(t, m, id)
	}
	cfg, _ := loadConfig()
	if config.MemoryExtractGateEnabled(cfg) || config.MemoryRecallFilterEnabled(cfg) {
		t.Fatalf("the memory decision gates default on and flip off: %+v", cfg.Memory)
	}
	if r := configRowByID(t, m, "memoryGate"); r.options[r.current] != "off" {
		t.Errorf("the row reads the saved value: %+v", r)
	}

	m.proc = &providerProc{}
	m = configEnter(t, m, "skipAllPermissions")
	if !m.skipAllPermissions || m.proc != nil {
		t.Errorf("skip-all-permissions applies and restarts the session: skip=%v proc=%v", m.skipAllPermissions, m.proc)
	}

	m.cursorBlink = false
	m = focusConfigRow(t, m, "cursorBlink")
	m2, cmd := runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m2.cursorBlink || cmd == nil {
		t.Errorf("turning blink on starts it: blink=%v cmd=%v", m2.cursorBlink, cmd)
	}
}

func TestConfigScreen_ChoiceRowsStepAndPersist(t *testing.T) {
	isolateHome(t)
	p1 := newFakeProvider()
	p1.id, p1.displayName = "claude", "Claude"
	p2 := newFakeProvider()
	p2.id, p2.displayName = "codex", "Codex"
	withRegisteredProviders(t, p1, p2)
	m := newTestModel(t, p1).startConfigModal()

	if r := configRowByID(t, m, "provider"); r.options[r.current] != "Claude" {
		t.Errorf("with nothing saved the first provider is the default: %+v", r)
	}
	if err := saveConfig(askConfig{Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	if r := configRowByID(t, m, "provider"); r.options[r.current] != "Codex" {
		t.Errorf("the row reads the saved default, not the tab's provider: %+v", r)
	}
	m = pressConfig(t, focusConfigRow(t, m, "provider"), tea.KeyPressMsg{Code: tea.KeyRight})
	if cfg, _ := loadConfig(); cfg.Provider != "claude" {
		t.Errorf("→ steps and saves the default: %q", cfg.Provider)
	}
	if m.provider.ID() != "claude" || m.mode != modeConfig {
		t.Errorf("the default only applies to new tabs; the screen stays up: %s %v", m.provider.ID(), m.mode)
	}

	m.toolOutputMode = toolOutputFull
	m = pressConfig(t, focusConfigRow(t, m, "toolOutput"), tea.KeyPressMsg{Code: tea.KeyRight})
	if m.toolOutputMode != toolOutputShort {
		t.Fatalf("→ moves full to short: %q", m.toolOutputMode)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyLeft}, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.toolOutputMode != toolOutputOff {
		t.Fatalf("← wraps from full to off: %q", m.toolOutputMode)
	}
	if cfg, _ := loadConfig(); cfg.UI.ToolOutput != string(toolOutputOff) {
		t.Errorf("tool output persisted: %q", cfg.UI.ToolOutput)
	}
}

func TestConfigScreen_ThemeListPreviewsAndRevertsOnEsc(t *testing.T) {
	m := configFixture(t)
	m = m.previewTheme(0)
	start := m.themeName
	m = configEnter(t, m, "theme")
	if m.config.choosing != "theme" {
		t.Fatalf("Enter opens the theme list: %q", m.config.choosing)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.themeName == start {
		t.Fatal("moving in the list previews the theme")
	}
	if cfg, _ := loadConfig(); cfg.UI.Theme != "" {
		t.Errorf("a preview is not saved: %q", cfg.UI.Theme)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.themeName != start || m.config.choosing != "" {
		t.Fatalf("Esc restores the theme: %q", m.themeName)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	cfg, _ := loadConfig()
	if m.config.choosing != "" || cfg.UI.Theme != m.themeName || m.themeName == start {
		t.Fatalf("Enter in the list saves the theme: %q saved %q", m.themeName, cfg.UI.Theme)
	}
	before := m.themeName
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	if cfg, _ := loadConfig(); m.themeName == before || cfg.UI.Theme != m.themeName {
		t.Errorf("→ on the row steps and saves: %q saved %q", m.themeName, cfg.UI.Theme)
	}
	applyTheme(themeRegistry[themeIndexByName(start)])
}

func TestConfigScreen_TextRowsEditValidateAndPersist(t *testing.T) {
	m := configFixture(t)
	project := "provider:" + vertexProviderID + ":" + providers.VertexFieldProject
	location := "provider:" + vertexProviderID + ":" + providers.VertexFieldLocation
	saKey := "provider:" + vertexProviderID + ":" + providers.VertexFieldServiceAccountKey
	if r := configRowByID(t, m, location); r.value != "global (default)" {
		t.Errorf("an unset field shows its default: %q", r.value)
	}

	m = configEnter(t, m, project)
	if m.config.editing != project || m.config.input.Value() != "" {
		t.Fatalf("Enter opens the editor on the stored value: %q %q", m.config.editing, m.config.input.Value())
	}
	m = pressConfig(t, m, typeKeys("abc")...)
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.config.editing != project || !m.config.noticeErr {
		t.Fatal("a validation failure keeps the editor open and says why")
	}
	if cfg, _ := loadConfig(); len(cfg.Providers) != 0 {
		t.Fatalf("an invalid draft is not saved: %+v", cfg.Providers)
	}
	m.config.input.SetValue("my")
	m = pressConfig(t, m, tea.PasteMsg{Content: "-proj"}, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.config.editing != "" {
		t.Fatal("a valid draft closes the editor")
	}
	cfg, _ := loadConfig()
	if got := cfg.ProviderConfig(vertexProviderID).Field(providers.VertexFieldProject); got != "my-proj" {
		t.Fatalf("persisted project = %q", got)
	}
	if r := configRowByID(t, m, project); r.value != "my-proj" {
		t.Errorf("the row shows the saved value: %q", r.value)
	}

	m = configEnter(t, m, location)
	if m.config.input.Value() != "" {
		t.Errorf("the editor opens on what is stored, not the default: %q", m.config.input.Value())
	}
	m.config.input.SetValue("blah")
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.config.editing != location {
		t.Fatal("an invalid location keeps the editor open")
	}
	m.config.input.SetValue("us-central1")
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	cfg, _ = loadConfig()
	if v := cfg.ProviderConfig(vertexProviderID); v.Field(providers.VertexFieldLocation) != "us-central1" || v.Field(providers.VertexFieldProject) != "my-proj" {
		t.Errorf("the location saves beside the project: %+v", v)
	}

	sa := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(sa, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m = configEnter(t, m, saKey)
	m.config.input.SetValue(sa)
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	cfg, _ = loadConfig()
	if got := cfg.ProviderConfig(vertexProviderID).Field(providers.VertexFieldServiceAccountKey); got != sa {
		t.Fatalf("SA key = %q", got)
	}
	m = pressConfig(t, focusConfigRow(t, m, saKey), tea.KeyPressMsg{Code: tea.KeyDelete})
	cfg, _ = loadConfig()
	if _, ok := cfg.ProviderConfig(vertexProviderID).Fields[providers.VertexFieldServiceAccountKey]; ok {
		t.Error("Delete clears the stored value")
	}
}

func TestConfigScreen_SecretsAreMaskedAndEnvFallbackShows(t *testing.T) {
	m := configFixture(t)
	id := "provider:" + providers.OpenRouterProviderID + ":" + providers.OpenRouterFieldAPIKey
	m = configEnter(t, m, id)
	if m.config.input.EchoMode != textinput.EchoPassword {
		t.Fatal("a secret is edited masked")
	}
	m.config.input.SetValue("sk-secret")
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	r := configRowByID(t, m, id)
	plain, styled := configRowValue(r, 40)
	if r.value != "configured" || strings.Contains(plain, "sk-secret") || strings.Contains(styled, "sk-secret") {
		t.Errorf("a stored secret reads configured and is never shown: %q", r.value)
	}
	if cfg, _ := loadConfig(); cfg.ProviderConfig(providers.OpenRouterProviderID).Field(providers.OpenRouterFieldAPIKey) != "sk-secret" {
		t.Error("the secret is saved")
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyDelete})
	t.Setenv(providers.OpenRouterEnvAPIKey, "env-key")
	if r := configRowByID(t, m, id); r.value != "from $"+providers.OpenRouterEnvAPIKey {
		t.Errorf("with nothing stored the env source shows: %q", r.value)
	}
}

func TestConfigScreen_SaveRestartsOnlyTheMatchingSession(t *testing.T) {
	m := configFixture(t)
	fake := newFakeProvider()
	fake.id = providers.OpenRouterProviderID
	m.provider = fake
	m.proc = &providerProc{}
	m = configEnter(t, m, "provider:"+providers.OpenRouterProviderID+":"+providers.OpenRouterFieldBaseURL)
	m.config.input.SetValue("https://x/v1")
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.proc != nil {
		t.Error("the tab's own provider's settings restart its session")
	}

	m.proc = &providerProc{}
	m = configEnter(t, m, "provider:"+vertexProviderID+":"+providers.VertexFieldLocation)
	m.config.input.SetValue("global")
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.proc == nil {
		t.Error("another provider's settings leave the session alone")
	}

	for _, id := range []string{"braveApiKey", "decision:" + providers.GeminiAPIProviderID + ":" + providers.GeminiFieldAPIKey} {
		m = configEnter(t, m, id)
		m.config.input.SetValue("k")
		m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
		if m.proc == nil {
			t.Errorf("%s is read per call; no restart", id)
		}
	}
}

func TestConfigScreen_PasteRouting(t *testing.T) {
	m := configFixture(t)
	m.input.SetValue("composer")
	m = pressConfig(t, m, tea.PasteMsg{Content: "web"})
	if m.config.query != "web" || m.input.Value() != "composer" {
		t.Fatalf("a paste outside the editor filters: query=%q composer=%q", m.config.query, m.input.Value())
	}
	m = pressConfig(t, m, tea.PasteMsg{Content: "two\nlines"})
	if m.config.query != "web" {
		t.Errorf("a multi-line paste is not a filter: %q", m.config.query)
	}
	m = configEnter(t, m, "braveApiKey")
	m = pressConfig(t, m, tea.PasteMsg{Content: "brave-key\n"})
	if m.config.input.Value() != "brave-key" || m.input.Value() != "composer" {
		t.Errorf("a paste reaches the open editor only: %q", m.config.input.Value())
	}
}

func TestConfigScreen_DecisionsSection(t *testing.T) {
	m := configFixture(t)
	model := configRowByID(t, m, "decisionModel")
	want := providers.GeminiAPIProviderID + "/" + providers.GeminiDecision1
	if model.options[0] != "Automatic" || model.current != 0 || len(model.values) < 2 || model.values[1] != want {
		t.Fatalf("decision model options: %+v", model)
	}
	if !strings.Contains(model.desc, "No decision model can run yet") {
		t.Errorf("without a key the row says so: %q", model.desc)
	}

	keyID := "decision:" + providers.GeminiAPIProviderID + ":" + providers.GeminiFieldAPIKey
	m = configEnter(t, m, keyID)
	if m.config.input.EchoMode != textinput.EchoPassword {
		t.Error("the Gemini API key is edited masked")
	}
	m.config.input.SetValue("AIza-test")
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	cfg, _ := loadConfig()
	if got := cfg.ProviderConfig(providers.GeminiAPIProviderID).Field(providers.GeminiFieldAPIKey); got != "AIza-test" {
		t.Fatalf("the key saves under the provider's block: %q", got)
	}
	if model := configRowByID(t, m, "decisionModel"); !strings.Contains(model.desc, "Now: "+want) {
		t.Errorf("the row names the model that now runs: %q", model.desc)
	}

	m = configEnter(t, m, "decisionModel")
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	cfg, _ = loadConfig()
	if cfg.Decisions.Provider != providers.GeminiAPIProviderID || cfg.Decisions.Model != providers.GeminiDecision1 {
		t.Fatalf("picking from the list saves it: %+v", cfg.Decisions)
	}
	if r := configRowByID(t, m, "decisionModel"); r.current != 1 {
		t.Errorf("the row reads the saved model: %d", r.current)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if cfg, _ := loadConfig(); cfg.Decisions != (config.DecisionsConfig{}) {
		t.Errorf("← back to Automatic clears the choice: %+v", cfg.Decisions)
	}
}

func TestConfigScreen_ProjectRows(t *testing.T) {
	m := configFixture(t)
	m.cwd = t.TempDir()
	saved := func() projectConfig {
		t.Helper()
		cfg, _ := loadConfig()
		return loadProjectConfig(cfg, m.cwd)
	}

	m.proc = &providerProc{}
	m = configEnter(t, m, "githubAuth")
	if saved().MCP.GitHub.Auth != githubAuthGH || m.proc != nil {
		t.Fatalf("Enter switches GitHub auth to the gh CLI and restarts the session: %+v", saved().MCP.GitHub)
	}

	m.proc = &providerProc{}
	m = pressConfig(t, focusConfigRow(t, m, "issueProvider"), tea.KeyPressMsg{Code: tea.KeyRight})
	if saved().Issues.Provider != "github" || m.proc == nil {
		t.Fatalf("gh auth makes GitHub reachable, and the issue provider leaves the session alone: %q", saved().Issues.Provider)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	if saved().Issues.Provider != "" || !strings.Contains(m.config.notice, "Linear") {
		t.Fatalf("Linear without credentials is skipped, wrapping to none: %q %q", saved().Issues.Provider, m.config.notice)
	}

	m = configEnter(t, m, "githubAuth")
	if saved().MCP.GitHub.Auth != "" {
		t.Fatal("Enter again goes back to the PAT")
	}
	m = pressConfig(t, focusConfigRow(t, m, "issueProvider"), tea.KeyPressMsg{Code: tea.KeyRight})
	if saved().Issues.Provider != "" {
		t.Fatalf("without any credentials the provider stays none: %q", saved().Issues.Provider)
	}

	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, m.cwd, projectConfig{Issues: issuesConfig{Provider: "github"}})
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	if saved().Issues.Provider != "" {
		t.Fatalf("leaving GitHub works without a token: %q", saved().Issues.Provider)
	}

	cfg, _ = loadConfig()
	cfg = upsertProjectConfig(cfg, m.cwd, projectConfig{MCP: projectMCPConfig{
		GitHub: githubMCPConfig{Token: "ghp_seed"},
		Linear: linearMCPConfig{Token: "lin_api_seed", TeamKey: "ENG"},
	}})
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if saved().Issues.Provider != "linear" {
		t.Fatalf("← cycles backwards: %q", saved().Issues.Provider)
	}

	m.proc = &providerProc{}
	m = configEnter(t, m, "project:githubMCPToken")
	if m.config.input.EchoMode != textinput.EchoPassword {
		t.Error("the PAT is edited masked")
	}
	m.config.input.SetValue("ghp_abc")
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if saved().MCP.GitHub.Token != "ghp_abc" || m.proc != nil {
		t.Fatalf("a project credential saves and restarts the session (its MCP roster bakes it in): %+v", saved().MCP.GitHub)
	}

	m = configEnter(t, m, "workflows")
	if m.mode != modeInput || m.config != nil || m.screen != screenWorkflows || m.workflowsBuilder == nil {
		t.Errorf("Workflows opens the builder screen: mode=%v screen=%v", m.mode, m.screen)
	}
}

func TestConfigScreen_ActionRowsHandBack(t *testing.T) {
	m := configFixture(t)
	m = configEnter(t, m, "deslopModel")
	if m.mode != modeModelPicker || m.modelPicker == nil || !m.modelPicker.deslopTarget || m.config == nil {
		t.Fatalf("Deslop model opens the retargeted picker and keeps the screen's state: mode=%v", m.mode)
	}
	res, _ := m.dispatchModelPick(modelPickerEntry{providerID: "fake", providerName: "Fake", modelID: "m-two"})
	m = res.(model)
	if m.mode != modeConfig || !strings.Contains(m.config.notice, "fake/m-two") {
		t.Fatalf("a pick lands back on /config with the outcome: mode=%v notice=%q", m.mode, m.config.notice)
	}
	if r := configRowByID(t, m, "deslopModel"); r.value != "fake/m-two" {
		t.Errorf("the row shows the pick: %q", r.value)
	}

	m = configEnter(t, m, "memoryBrowse")
	if m.config != nil || m.mode == modeConfig {
		t.Errorf("Browse memories leaves /config: mode=%v", m.mode)
	}
}

// TestOpenTab_LoadsCfgFromDisk: after a /config change, Ctrl+T spawns the
// new tab on the just-saved provider rather than whatever was on disk at
// startup.
func TestOpenTab_LoadsCfgFromDisk(t *testing.T) {
	isolateHome(t)
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	p1 := newFakeProvider()
	p1.id, p1.displayName = "claude", "Claude"
	p2 := newFakeProvider()
	p2.id, p2.displayName = "codex", "Codex"
	withRegisteredProviders(t, p1, p2)

	first, err := newTab(1, askConfig{})
	if err != nil {
		t.Fatalf("newTab first: %v", err)
	}
	if first.provider == nil || first.provider.ID() != "claude" {
		t.Fatalf("first tab provider did not default to claude: %+v", first.provider)
	}
	a := newApp(first)
	if err := saveConfig(askConfig{Provider: "codex"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	newA, _ := a.openTab()
	a2, ok := newA.(app)
	if !ok || len(a2.tabs) != 2 {
		t.Fatalf("openTab returned %T with %d tabs", newA, len(a2.tabs))
	}
	if nt := a2.tabs[1]; nt.provider == nil || nt.provider.ID() != "codex" {
		t.Errorf("new tab provider must come from the saved default: %+v", nt.provider)
	}
}

func TestThemeIndexByName(t *testing.T) {
	if themeIndexByName(themeRegistry[3].name) != 3 {
		t.Error("finds a theme by name")
	}
	if themeIndexByName("no-such-theme") != 0 {
		t.Error("an unknown theme falls back to the first")
	}
}
