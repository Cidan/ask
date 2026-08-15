package main

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestConfigVertexPicker covers the /config → Vertex AI submenu:
// opening it, entering the editor for each of the three fields,
// validation failures, persistence to disk, paste accumulation, and
// Esc / Ctrl+C close.
func TestConfigVertexPicker(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)

	// The global config list must surface the Vertex row.
	var hasRow bool
	for _, it := range m.globalConfigItems() {
		if it.id == "vertex" {
			hasRow = true
		}
	}
	if !hasRow {
		t.Fatal("globalConfigItems missing the Vertex row")
	}

	// Unconfigured summary is "off".
	if got := vertexSummary(); got != "off" {
		t.Errorf("unconfigured summary = %q want off", got)
	}

	m = m.openConfigVertexPicker()
	if !m.configVertexPickerActive || m.configVertexFieldEditing != "" {
		t.Fatalf("open: active=%v editing=%v", m.configVertexPickerActive, m.configVertexFieldEditing)
	}

	// Three rows; first is "project".
	rows := m.vertexPickerItems()
	if len(rows) != 3 {
		t.Fatalf("rows=%d want 3", len(rows))
	}
	if rows[0].id != "project" || rows[1].id != "location" || rows[2].id != "serviceAccountKey" {
		t.Errorf("row order wrong: %+v", rows)
	}

	// Enter on Project row opens the inline editor.
	mm, _ := m.updateConfigVertexPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(model)
	if m.configVertexFieldEditing != "project" {
		t.Fatalf("Enter on project row should open the editor, got editing=%q", m.configVertexFieldEditing)
	}

	// Editor pre-fills with the on-disk value (empty here).
	if m.configVertexFieldDraft != "" {
		t.Errorf("editor draft should start empty, got %q", m.configVertexFieldDraft)
	}

	// Type a valid project id.
	mm, _ = m.updateConfigVertexFieldInput(tea.KeyPressMsg{Code: 'm', Text: "m"})
	m = mm.(model)
	mm, _ = m.updateConfigVertexFieldInput(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m = mm.(model)
	mm, _ = m.applyConfigVertexPaste("-proj")
	m = mm.(model)
	if m.configVertexFieldDraft != "my-proj" {
		t.Fatalf("draft = %q, want my-proj", m.configVertexFieldDraft)
	}

	// Enter commits and closes the editor; value lands on disk.
	mm, _ = m.updateConfigVertexFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(model)
	if m.configVertexFieldEditing != "" {
		t.Error("commit should close the editor")
	}
	cfg, _ := loadConfig()
	if cfg.Vertex.Project != "my-proj" {
		t.Fatalf("persisted project = %q, want my-proj", cfg.Vertex.Project)
	}

	// Row summary now reflects the saved value.
	rows = m.vertexPickerItems()
	if rows[0].key != "my-proj" {
		t.Errorf("project row key = %q, want my-proj", rows[0].key)
	}

	// Vertex summary now mentions the project.
	if got := vertexSummary(); got != "my-proj/global" {
		t.Errorf("configured summary = %q want my-proj/global", got)
	}
}

func TestConfigVertexPicker_LocationValidation(t *testing.T) {
	isolateHome(t)
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.Vertex.Project = "my-proj"
		return saveConfig(cfg)
	}); err != nil {
		t.Fatal(err)
	}

	// Valid: "global".
	if err := validateVertexLocation("global"); err != nil {
		t.Errorf("global must validate: %v", err)
	}
	// Valid: "us-central1".
	if err := validateVertexLocation("us-central1"); err != nil {
		t.Errorf("us-central1 must validate: %v", err)
	}
	// Invalid: empty.
	if err := validateVertexLocation(""); err == nil {
		t.Error("empty location must fail")
	}
	// Invalid: weird shape.
	if err := validateVertexLocation("blah"); err == nil {
		t.Error("non-shape location must fail")
	}

	// Drive the picker: invalid draft → toast + editor stays open.
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m = m.openConfigVertexPicker()
	// Cursor down to Location.
	mm, _ := m.updateConfigVertexPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(model)
	// Open the location editor.
	mm, _ = m.updateConfigVertexPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(model)
	if m.configVertexFieldEditing != "location" {
		t.Fatalf("expected location editor, got %q", m.configVertexFieldEditing)
	}
	// Type an invalid value.
	for _, r := range "blah" {
		mm, _ = m.updateConfigVertexFieldInput(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = mm.(model)
	}
	mm, _ = m.updateConfigVertexFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(model)
	// Editor stays open on validation failure.
	if m.configVertexFieldEditing != "location" {
		t.Error("invalid location should keep the editor open")
	}
	cfg, _ := loadConfig()
	if cfg.Vertex.Location != "" {
		t.Errorf("invalid location must not be persisted, got %q", cfg.Vertex.Location)
	}
}

func TestConfigVertexPicker_ServiceAccountKeyValidation(t *testing.T) {
	isolateHome(t)

	// Valid: empty (clears the field).
	if err := validateVertexServiceAccountKey(""); err != nil {
		t.Errorf("empty SA key must validate: %v", err)
	}

	// Valid: a path to a real file.
	dir := t.TempDir()
	realPath := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(realPath, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateVertexServiceAccountKey(realPath); err != nil {
		t.Errorf("real SA key path must validate: %v", err)
	}

	// Valid: tilde expansion.
	home := t.TempDir()
	t.Setenv("HOME", home)
	tildeDir := filepath.Join(home, "keys")
	if err := os.MkdirAll(tildeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tildePath := filepath.Join(tildeDir, "sa.json")
	if err := os.WriteFile(tildePath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateVertexServiceAccountKey("~/keys/sa.json"); err != nil {
		t.Errorf("tilde-expanded SA key must validate: %v", err)
	}

	// Invalid: non-existent file.
	if err := validateVertexServiceAccountKey("/nonexistent/path/sa.json"); err == nil {
		t.Error("non-existent SA key must fail validation")
	}

	// Invalid: a directory.
	if err := validateVertexServiceAccountKey(dir); err == nil {
		t.Errorf("directory must fail SA key validation, got nil")
	}
}

func TestConfigVertexPicker_ServiceAccountKeyPersists(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	saPath := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(saPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.Vertex.Project = "my-proj"
		return saveConfig(cfg)
	}); err != nil {
		t.Fatal(err)
	}
	m = m.openConfigVertexPicker()
	// Cursor to SA key row (3rd).
	mm, _ := m.updateConfigVertexPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(model)
	mm, _ = m.updateConfigVertexPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(model)
	mm, _ = m.updateConfigVertexPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(model)
	if m.configVertexFieldEditing != "serviceAccountKey" {
		t.Fatalf("expected SA key editor, got %q", m.configVertexFieldEditing)
	}
	// Type + paste.
	for _, r := range saPath {
		mm, _ = m.updateConfigVertexFieldInput(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = mm.(model)
	}
	mm, _ = m.updateConfigVertexFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(model)
	cfg, _ := loadConfig()
	if cfg.Vertex.ServiceAccountKey != saPath {
		t.Errorf("persisted SA key = %q, want %q", cfg.Vertex.ServiceAccountKey, saPath)
	}
}

func TestConfigVertexPicker_ProjectValidation(t *testing.T) {
	// Valid: 6-30 lowercase chars starting with a letter.
	for _, p := range []string{"myproj", "my-proj", "abc123", "a12345"} {
		if err := validateVertexProject(p); err != nil {
			t.Errorf("%q must validate: %v", p, err)
		}
	}
	// Invalid: empty / too short / too long / uppercase / starts with digit.
	for _, p := range []string{"", "abc", "ABCdef", "1abcde", "a-very-long-project-name-that-is-too-long"} {
		if err := validateVertexProject(p); err == nil {
			t.Errorf("%q must fail validation", p)
		}
	}
}

func TestConfigVertexPicker_EscClose(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.openConfigVertexPicker()
	mm, _ := m.updateConfigVertexPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mm.(model)
	if m.configVertexPickerActive {
		t.Error("Esc should close the Vertex picker")
	}
}
