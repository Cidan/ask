package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/providers"
)

func fieldsFixture(t *testing.T) model {
	t.Helper()
	isolateHome(t)
	t.Setenv(providers.VertexEnvCloudProject, "")
	t.Setenv(providers.VertexEnvApplicationCredentials, "")
	t.Setenv(providers.OpenRouterEnvAPIKey, "")
	t.Setenv(braveEnvAPIKey, "")
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m = m.startConfigModal()
	return m
}

func rowIDs(items []configItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.id)
	}
	return out
}

// Every registered provider gets a /config row, generated from the
// registry, and Web Search keeps its own.
func TestGlobalConfigItems_ProviderRowsComeFromRegistry(t *testing.T) {
	m := fieldsFixture(t)
	items := m.globalConfigItems()
	byID := map[string]configItem{}
	for _, it := range items {
		byID[it.id] = it
	}
	for _, p := range providers.All() {
		row, ok := byID[providerConfigItemID(p)]
		if !ok {
			t.Fatalf("missing /config row for provider %q: %v", p.ID(), rowIDs(items))
		}
		// A provider with no config reads "off" unless it is already
		// configured from the environment — Claude Code is configured
		// whenever the `claude` binary is on PATH, so derive the expectation.
		want := "off"
		if p.Configured(providerConfig{}) {
			want = "on"
		}
		if row.name != p.DisplayName()+"..." || row.key != want {
			t.Errorf("%s row = %+v, want %q / %s", p.ID(), row, p.DisplayName()+"...", want)
		}
	}
	if _, ok := byID["webSearch"]; !ok {
		t.Error("Web Search row must stay")
	}
	ids := rowIDs(items)
	if ids[len(ids)-1] != "keybindings" {
		t.Errorf("Keybindings stays last: %v", ids)
	}

	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.SetProviderConfig(vertexProviderID, providerConfig{}.WithField(providers.VertexFieldProject, "my-proj"))
		return saveConfig(cfg)
	}); err != nil {
		t.Fatal(err)
	}
	for _, it := range m.globalConfigItems() {
		if it.id == providerConfigItemID(providers.Vertex{}) && it.key != "on" {
			t.Errorf("a configured provider reads on, got %q", it.key)
		}
	}
}

// Enter on a provider row opens the fields picker for that provider
// through the global-options dispatcher.
func TestFieldsPicker_OpensFromProviderRow(t *testing.T) {
	m := fieldsFixture(t)
	mi, _ := m.handleGlobalConfigEnter(providerConfigItemID(providers.OpenRouter{}))
	m = mi.(model)
	if !m.configFields.active || m.configFields.title != "OpenRouter" || m.configFields.id != providers.OpenRouterProviderID {
		t.Fatalf("provider row must open its fields picker: %+v", m.configFields)
	}
	rows := m.fieldsPickerItems()
	if got := rowIDs(rows); len(got) != 2 || got[0] != providers.OpenRouterFieldAPIKey || got[1] != providers.OpenRouterFieldBaseURL {
		t.Errorf("rows follow the provider's Settings order: %v", got)
	}
	if rows[0].key != "(not set)" || rows[1].key != providers.OpenRouterDefaultBaseURL+" (default)" {
		t.Errorf("summaries: %+v", rows)
	}

	// Keys route through updateConfigModal while the picker is active.
	m.configGlobalPickerActive = true
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mi.(model)
	if m.configFields.active {
		t.Error("Esc must close the fields picker")
	}
	if !m.configGlobalPickerActive {
		t.Error("closing the fields picker must return to Global Options, not close it")
	}

	mi, _ = m.handleGlobalConfigEnter(providerConfigItemPrefix + "nosuch")
	if mi.(model).configFields.active {
		t.Error("an unknown provider id must not open a picker")
	}
}

// Vertex end to end: three rows, the editor pre-fills, validation keeps
// the editor open, a valid draft persists into the provider block, and
// the row summaries reflect the saved values.
func TestFieldsPicker_VertexEditAndPersist(t *testing.T) {
	m := fieldsFixture(t)
	m = m.openProviderFieldsPicker(providers.Vertex{})
	rows := m.fieldsPickerItems()
	if got := rowIDs(rows); len(got) != 3 || got[0] != providers.VertexFieldProject || got[1] != providers.VertexFieldLocation || got[2] != providers.VertexFieldServiceAccountKey {
		t.Fatalf("vertex rows: %v", got)
	}
	if rows[1].key != "global (default)" {
		t.Errorf("unset location shows its default: %q", rows[1].key)
	}

	// Enter on Project opens the editor, pre-filled with the stored value (empty).
	mi, _ := m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configFields.editing != providers.VertexFieldProject || m.configFields.draft != "" {
		t.Fatalf("editor: %+v", m.configFields)
	}
	// Invalid draft: toast, editor stays open, nothing persisted.
	for _, r := range "abc" {
		mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = mi.(model)
	}
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configFields.editing != providers.VertexFieldProject {
		t.Error("a validation failure must keep the editor open")
	}
	if cfg, _ := loadConfig(); len(cfg.Providers) != 0 {
		t.Errorf("invalid draft must not persist: %+v", cfg.Providers)
	}
	// Type + paste a valid id, Enter persists and closes the editor.
	m.configFields.draft = ""
	for _, r := range "my" {
		mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = mi.(model)
	}
	mi, _ = m.applyFieldsPickerPaste("-proj")
	m = mi.(model)
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configFields.editing != "" || !m.configFields.active {
		t.Errorf("commit closes the editor and keeps the picker: %+v", m.configFields)
	}
	cfg, _ := loadConfig()
	if got := cfg.ProviderConfig(vertexProviderID).Field(providers.VertexFieldProject); got != "my-proj" {
		t.Fatalf("persisted project = %q", got)
	}
	if rows := m.fieldsPickerItems(); rows[0].key != "my-proj" {
		t.Errorf("row reflects the saved value: %+v", rows[0])
	}

	// Location: invalid keeps the editor open, "us-central1" lands.
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mi.(model)
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configFields.editing != providers.VertexFieldLocation || m.configFields.draft != "" {
		t.Fatalf("location editor pre-fills the stored value, not the default: %+v", m.configFields)
	}
	m.configFields.draft = "blah"
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configFields.editing != providers.VertexFieldLocation {
		t.Error("invalid location keeps the editor open")
	}
	m.configFields.draft = "us-central1"
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	cfg, _ = loadConfig()
	if v := cfg.ProviderConfig(vertexProviderID); v.Field(providers.VertexFieldLocation) != "us-central1" || v.Field(providers.VertexFieldProject) != "my-proj" {
		t.Errorf("location saved next to the project: %+v", v)
	}

	// Service account key: a real file validates and persists; clearing removes the key.
	dir := t.TempDir()
	sa := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(sa, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mi.(model)
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	m.configFields.draft = sa
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	cfg, _ = loadConfig()
	if got := cfg.ProviderConfig(vertexProviderID).Field(providers.VertexFieldServiceAccountKey); got != sa {
		t.Errorf("SA key = %q want %q", got, sa)
	}
	m = m.openFieldsPickerEditor(providers.VertexFieldServiceAccountKey)
	if m.configFields.draft != sa {
		t.Errorf("editor pre-fills the saved path: %q", m.configFields.draft)
	}
	m.configFields.draft = ""
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	cfg, _ = loadConfig()
	if _, ok := cfg.ProviderConfig(vertexProviderID).Fields[providers.VertexFieldServiceAccountKey]; ok {
		t.Error("clearing a field removes its key")
	}
}

// Secrets are masked in the row and the editor; an env fallback is
// surfaced on the row so the user knows why the provider counts as on.
func TestFieldsPicker_SecretMaskingAndEnv(t *testing.T) {
	m := fieldsFixture(t)
	m.width, m.height = 120, 40
	m = m.openProviderFieldsPicker(providers.OpenRouter{})
	m = m.openFieldsPickerEditor(providers.OpenRouterFieldAPIKey)
	m.configFields.draft = "sk-secret"
	if view := m.viewFieldsPicker(); strings.Contains(view, "sk-secret") || !strings.Contains(view, "•••••••••") {
		t.Error("the editor must mask a secret draft")
	}
	mi, _ := m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	rows := m.fieldsPickerItems()
	if rows[0].key != "configured" {
		t.Errorf("a stored secret reads configured: %+v", rows[0])
	}
	if view := m.viewFieldsPicker(); strings.Contains(view, "sk-secret") {
		t.Error("the row list must never show the secret")
	}
	cfg, _ := loadConfig()
	if got := cfg.ProviderConfig(providers.OpenRouterProviderID).Field(providers.OpenRouterFieldAPIKey); got != "sk-secret" {
		t.Errorf("persisted key = %q", got)
	}

	// Clear it; with the env fallback set the row says where the value comes from.
	m = m.openFieldsPickerEditor(providers.OpenRouterFieldAPIKey)
	m.configFields.draft = ""
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	t.Setenv(providers.OpenRouterEnvAPIKey, "env-key")
	if rows := m.fieldsPickerItems(); rows[0].key != "from $"+providers.OpenRouterEnvAPIKey {
		t.Errorf("env-sourced value: %+v", rows[0])
	}
}

// Web Search rides the same picker with its own store.
func TestFieldsPicker_WebSearchRoundTrip(t *testing.T) {
	m := fieldsFixture(t)
	mi, _ := m.handleGlobalConfigEnter("webSearch")
	m = mi.(model)
	if !m.configFields.active || m.configFields.title != "Web Search" {
		t.Fatalf("webSearch row opens the fields picker: %+v", m.configFields)
	}
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configFields.editing != "braveApiKey" {
		t.Fatalf("editing = %q", m.configFields.editing)
	}
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: 'B', Text: "B"})
	m = mi.(model)
	mi, _ = m.applyFieldsPickerPaste("rave-Key")
	m = mi.(model)
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	cfg, _ := loadConfig()
	if cfg.WebSearch.BraveAPIKey != "Brave-Key" {
		t.Fatalf("persisted key = %q", cfg.WebSearch.BraveAPIKey)
	}
	if rows := m.fieldsPickerItems(); rows[0].key != "configured" {
		t.Errorf("masked summary: %+v", rows)
	}
	m = m.openFieldsPickerEditor("braveApiKey")
	if m.configFields.draft != "Brave-Key" {
		t.Errorf("editor pre-fills the current key: %q", m.configFields.draft)
	}
	m.configFields.draft = ""
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if cfg, _ := loadConfig(); cfg.WebSearch.BraveAPIKey != "" {
		t.Errorf("cleared key persists empty, got %q", cfg.WebSearch.BraveAPIKey)
	}
	if len(cfg.Providers) != 0 {
		t.Error("web search must not write a provider block")
	}
}

// A paste while the editor is open lands in the draft via the top-level
// PasteMsg dispatcher; outside the editor it is ignored by the picker.
func TestFieldsPicker_PasteRouting(t *testing.T) {
	m := fieldsFixture(t)
	m = m.openProviderFieldsPicker(providers.OpenRouter{})
	mi, _ := m.Update(tea.PasteMsg{Content: "ignored"})
	m = mi.(model)
	if m.configFields.draft != "" {
		t.Error("paste outside the editor must not touch the draft")
	}
	m = m.openFieldsPickerEditor(providers.OpenRouterFieldBaseURL)
	mi, _ = m.Update(tea.PasteMsg{Content: "https://x/v1"})
	m = mi.(model)
	if m.configFields.draft != "https://x/v1" {
		t.Errorf("paste must reach the editor draft, got %q", m.configFields.draft)
	}
}

// Saving a provider's credentials restarts the tab's live session only
// when the tab runs on that provider.
func TestFieldsPicker_SaveRestartsMatchingSession(t *testing.T) {
	m := fieldsFixture(t)
	fake := newFakeProvider()
	fake.id = providers.OpenRouterProviderID
	m.provider = fake
	m.proc = &providerProc{}
	m = m.openProviderFieldsPicker(providers.OpenRouter{})
	m = m.openFieldsPickerEditor(providers.OpenRouterFieldBaseURL)
	m.configFields.draft = "https://x/v1"
	mi, _ := m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.proc != nil {
		t.Error("editing the current provider's settings must drop the live session")
	}

	m.proc = &providerProc{}
	m = m.openProviderFieldsPicker(providers.Vertex{})
	m = m.openFieldsPickerEditor(providers.VertexFieldLocation)
	m.configFields.draft = "global"
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.proc == nil {
		t.Error("another provider's settings must leave the session alone")
	}

	m.proc = &providerProc{}
	m = m.openWebSearchFieldsPicker()
	m = m.openFieldsPickerEditor("braveApiKey")
	m.configFields.draft = "k"
	mi, _ = m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.proc == nil {
		t.Error("the Brave key is read per call; no restart")
	}
}

// The picker and its editor render through the layered config box and
// are drawn as the /config overlay; filtering narrows the rows.
func TestFieldsPicker_ViewAndFilter(t *testing.T) {
	m := fieldsFixture(t)
	m.width, m.height = 120, 40
	m = m.openProviderFieldsPicker(providers.Vertex{})
	view := m.viewFieldsPicker()
	for _, want := range []string{"Vertex AI", "Project", "Location", "Service Account Key", "enter edit"} {
		if !strings.Contains(view, want) {
			t.Errorf("picker view missing %q", want)
		}
	}
	if !strings.Contains(m.View().Content, "Service Account Key") {
		t.Error("the active picker must be drawn as the /config overlay")
	}
	for _, r := range "loc" {
		mi, _ := m.updateFieldsPicker(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = mi.(model)
	}
	if rows := m.filteredFieldsPickerItems(); len(rows) != 1 || rows[0].id != providers.VertexFieldLocation {
		t.Errorf("filter narrows rows: %v", rowIDs(rows))
	}
	mi, _ := m.updateFieldsPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configFields.editing != providers.VertexFieldLocation {
		t.Errorf("Enter on the filtered row edits it: %q", m.configFields.editing)
	}
	edit := m.viewFieldsPicker()
	for _, want := range []string{"Vertex AI · Location", "us-central1", "enter save"} {
		if !strings.Contains(edit, want) {
			t.Errorf("editor view missing %q", want)
		}
	}
	m = m.closeFieldsPicker()
	if m.configFields.active || m.configFilter != "" {
		t.Error("close resets the picker and the shared filter")
	}
}
