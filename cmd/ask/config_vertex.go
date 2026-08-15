package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// vertexPickerRow is one row in the /config → Vertex AI submenu.
// Three editable fields: project, location, and the optional
// service-account key path. Echoes the typed value (no masking —
// the SA key is a path on disk, not a secret).
type vertexPickerRow struct {
	name string
	key  string
	id   string
}

func (m model) vertexPickerItems() []vertexPickerRow {
	cfg, _ := loadConfig()
	rows := []vertexPickerRow{
		{"Project", plainSummary(cfg.Vertex.Project), "project"},
		{"Location", plainSummary(vertexResolveLocation(cfg.Vertex)), "location"},
		{"Service Account Key", plainSummary(cfg.Vertex.ServiceAccountKey), "serviceAccountKey"},
	}
	return rows
}

// vertexFieldSpec describes one editable row in the picker — the
// validation, load, and save logic. Centralised so the input
// handler doesn't grow a switch per field.
type vertexFieldSpec struct {
	id       string
	title    string
	helpHint string
	validate func(string) error
	load     func(askConfig) string
	save     func(*askConfig, string)
}

// vertexFieldSpecs is the registry, keyed by the row id. Order
// doesn't matter; row order comes from vertexPickerItems.
var vertexFieldSpecs = map[string]vertexFieldSpec{
	"project": {
		id:       "project",
		title:    "Project",
		helpHint: "Google Cloud project id; enter to save",
		validate: validateVertexProject,
		load:     func(c askConfig) string { return c.Vertex.Project },
		save:     func(c *askConfig, v string) { c.Vertex.Project = v },
	},
	"location": {
		id:       "location",
		title:    "Location",
		helpHint: "Vertex region (e.g. us-central1) or 'global'; enter to save",
		validate: validateVertexLocation,
		load:     func(c askConfig) string { return vertexResolveLocation(c.Vertex) },
		save:     func(c *askConfig, v string) { c.Vertex.Location = v },
	},
	"serviceAccountKey": {
		id:       "serviceAccountKey",
		title:    "Service Account Key",
		helpHint: "path to a service-account JSON; blank to use ADC; enter to save",
		validate: validateVertexServiceAccountKey,
		load:     func(c askConfig) string { return c.Vertex.ServiceAccountKey },
		save:     func(c *askConfig, v string) { c.Vertex.ServiceAccountKey = v },
	},
}

// validateVertexProject screens the picker input. GCP project ids
// are 6-30 chars, lowercase letters / digits / hyphens, must start
// with a letter. Empty is invalid (the picker would surface a
// "project is required" error at session start — better to block
// it here so the field visibly requires a value).
func validateVertexProject(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return errors.New("project is required")
	}
	if len(t) < 6 || len(t) > 30 {
		return errors.New("project id must be 6-30 characters")
	}
	if !vertexProjectIDPattern.MatchString(t) {
		return errors.New("project id must start with a lowercase letter and contain only lowercase letters, digits, or hyphens")
	}
	return nil
}

// vertexProjectIDPattern matches a valid GCP project id: starts
// with a lowercase letter, followed by 5-29 lowercase letters,
// digits, or hyphens.
var vertexProjectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{5,29}$`)

// validateVertexLocation accepts either the literal "global" or a
// region id matching the canonical GCP shape ([a-z]+-[a-z]+[0-9],
// e.g. "us-central1", "europe-west4"). Empty is invalid.
func validateVertexLocation(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return errors.New("location is required")
	}
	if t == "global" {
		return nil
	}
	if !vertexLocationPattern.MatchString(t) {
		return errors.New("location must be 'global' or match shape like 'us-central1'")
	}
	return nil
}

var vertexLocationPattern = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]$`)

// validateVertexServiceAccountKey accepts an empty string (clears
// the field, falls back to ADC) or a path to a readable file. The
// path is tilde-expanded so a user can paste `~/keys/vertex.json`.
func validateVertexServiceAccountKey(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil
	}
	expanded, _ := expandTilde(t)
	info, err := os.Stat(expanded)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", expanded, err)
	}
	if info.IsDir() {
		return errors.New("path is a directory, expected a JSON file")
	}
	return nil
}

func (m model) openConfigVertexPicker() model {
	m.configVertexPickerActive = true
	m.configVertexCursor = 0
	m.configVertexFieldEditing = ""
	m.configVertexFieldDraft = ""
	return m
}

func (m model) closeConfigVertexPicker() model {
	m.configVertexPickerActive = false
	m.configVertexCursor = 0
	m.configVertexFieldEditing = ""
	m.configVertexFieldDraft = ""
	return m
}

func (m model) openConfigVertexFieldEditor(id string) model {
	if _, ok := vertexFieldSpecs[id]; !ok {
		return m
	}
	cfg, _ := loadConfig()
	m.configVertexFieldEditing = id
	m.configVertexFieldDraft = vertexFieldSpecs[id].load(cfg)
	return m
}

func (m model) closeConfigVertexFieldEditor() model {
	m.configVertexFieldEditing = ""
	m.configVertexFieldDraft = ""
	return m
}

func (m model) updateConfigVertexPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configVertexFieldEditing != "" {
		return m.updateConfigVertexFieldInput(msg)
	}
	rows := m.vertexPickerItems()
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigVertexPicker()
		return m, nil
	case listNavPrev(msg):
		m.configVertexCursor = listNavWrap(m.configVertexCursor, -1, len(rows))
		return m, nil
	case listNavNext(msg):
		m.configVertexCursor = listNavWrap(m.configVertexCursor, +1, len(rows))
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configVertexCursor < 0 || m.configVertexCursor >= len(rows) {
			return m, nil
		}
		m = m.openConfigVertexFieldEditor(rows[m.configVertexCursor].id)
		return m, nil
	}
	return m, nil
}

func (m model) updateConfigVertexFieldInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigVertexFieldEditor()
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.commitConfigVertexField()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.configVertexFieldDraft); len(r) > 0 {
			m.configVertexFieldDraft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		m.configVertexFieldDraft += msg.Text
		return m, nil
	}
	return m, nil
}

// applyConfigVertexPaste appends pasted text to the field draft.
// Called from the top-level update.go PasteMsg dispatcher.
func (m model) applyConfigVertexPaste(text string) (tea.Model, tea.Cmd) {
	m.configVertexFieldDraft += text
	return m, nil
}

// commitConfigVertexField validates, persists, and closes the
// inline editor. Validation failure keeps the editor open so the
// user can correct without retyping; success emits a toast.
func (m model) commitConfigVertexField() (tea.Model, tea.Cmd) {
	id := m.configVertexFieldEditing
	spec, ok := vertexFieldSpecs[id]
	if !ok {
		m = m.closeConfigVertexFieldEditor()
		return m, nil
	}
	draft := strings.TrimSpace(m.configVertexFieldDraft)
	if spec.validate != nil {
		if err := spec.validate(draft); err != nil {
			return m, m.toast.show("vertex: "+spec.title+": "+err.Error())
		}
	}
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		spec.save(&cfg, draft)
		return saveConfig(cfg)
	}); err != nil {
		debugLog("vertex %s saveConfig: %v", id, err)
		m = m.closeConfigVertexFieldEditor()
		return m, m.toast.show("vertex: save: "+err.Error())
	}
	m = m.closeConfigVertexFieldEditor()
	if draft == "" {
		return m, m.toast.show("vertex: " + spec.title + " cleared")
	}
	return m, m.toast.show("vertex: " + spec.title + " saved")
}

func (m model) viewConfigVertexPicker() string {
	if m.configVertexFieldEditing != "" {
		return m.viewConfigVertexFieldInput()
	}
	rows := m.vertexPickerItems()
	innerW := 0
	for _, r := range rows {
		w := lipgloss.Width(r.name) + lipgloss.Width(r.key) + 4
		if w > innerW {
			innerW = w
		}
	}
	if innerW < 40 {
		innerW = 40
	}
	title := themePickerTitleStyle.Render("Vertex AI")
	body := make([]string, 0, len(rows)+4)
	body = append(body, title, "")
	for i, r := range rows {
		body = append(body, renderMemoryPickerRow(memoryPickerRow(r), innerW, i == m.configVertexCursor))
	}
	body = append(body,
		"",
		configHelpStyle.Render("Project + Location are required. Service Account Key is optional; blank falls back to ADC."),
		"",
		themePickerHelpStyle.Render("enter edit · esc close"),
	)
	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

func (m model) viewConfigVertexFieldInput() string {
	spec, ok := vertexFieldSpecs[m.configVertexFieldEditing]
	if !ok {
		return ""
	}
	innerW := 60
	title := themePickerTitleStyle.Render(spec.title)
	body := []string{
		title,
		"",
		configHelpStyle.Render(spec.helpHint),
		"",
		configPromptStyle.Render("> ") + m.configVertexFieldDraft + configCaretStyle.Render("▏"),
		"",
		themePickerHelpStyle.Render("enter save · esc cancel"),
	}
	for _, line := range body {
		if w := lipgloss.Width(line); w > innerW {
			innerW = w
		}
	}
	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}
