package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Cidan/memmy"
)

// resetMemoryService brings the package-level singleton down between
// tests. memory_test.go owns this helper because every memory test
// mutates global state; without explicit teardown, a failed test would
// leak a bbolt file lock into the next run and cascade failures.
func resetMemoryService(t *testing.T) {
	t.Helper()
	if err := closeMemoryService(); err != nil {
		t.Fatalf("close memory: %v", err)
	}
}

// openFakeMemoryService is the test entry point: opens the singleton
// against a fake embedder + the dedicated memory-fake.db path so we
// never need a real Gemini key in CI. Production code never reaches
// this — openMemoryService is the only public constructor and it
// requires a configured key.
func openFakeMemoryService(t *testing.T) {
	t.Helper()
	if err := openMemoryServiceWith(memmy.NewFakeEmbedder(memoryFakeEmbedderDim), true); err != nil {
		t.Fatalf("openFakeMemoryService: %v", err)
	}
	t.Cleanup(func() { _ = closeMemoryService() })
}

func TestMemoryDBPath_UnderHome(t *testing.T) {
	home := isolateHome(t)
	cases := []struct {
		name    string
		useFake bool
		want    string
	}{
		{"production", false, filepath.Join(home, ".local", "share", "ask", "memory.db")},
		{"fake", true, filepath.Join(home, ".local", "share", "ask", "memory-fake.db")},
	}
	for _, c := range cases {
		got, err := memoryDBPath(c.useFake)
		if err != nil {
			t.Fatalf("%s: memoryDBPath: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: memoryDBPath=%q want %q", c.name, got, c.want)
		}
	}
}

func TestOpenMemoryService_RequiresGeminiKey(t *testing.T) {
	// The production constructor should refuse to open without a key,
	// returning the typed sentinel so the picker can distinguish "no
	// key" from a real init failure.
	isolateHome(t)
	resetMemoryService(t)

	err := openMemoryService(askConfig{}) // empty config, no GeminiKey
	if err == nil {
		t.Fatalf("expected errMemoryNoKey, got nil")
	}
	if err != errMemoryNoKey {
		t.Errorf("expected errMemoryNoKey, got %v", err)
	}
	if memoryServiceOpen() {
		t.Errorf("service should not be open after no-key failure")
	}
}

func TestOpenCloseMemoryService_RoundTrip(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	if memoryServiceOpen() {
		t.Fatalf("service should be closed before open")
	}
	openFakeMemoryService(t)
	if !memoryServiceOpen() {
		t.Fatalf("service should be open after open")
	}

	// File materializes on disk so the next ask invocation can find it.
	path, _ := memoryDBPath(true)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected db at %s, got %v", path, err)
	}

	if err := closeMemoryService(); err != nil {
		t.Fatalf("closeMemoryService: %v", err)
	}
	if memoryServiceOpen() {
		t.Fatalf("service should be closed after close")
	}
}

func TestOpenMemoryService_Idempotent(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	openFakeMemoryService(t)
	// Second open is a no-op: openMemoryServiceWith returns early when
	// the singleton is already populated.
	if err := openMemoryServiceWith(memmy.NewFakeEmbedder(memoryFakeEmbedderDim), true); err != nil {
		t.Fatalf("second open should be no-op, got %v", err)
	}
	if !memoryServiceOpen() {
		t.Fatalf("service should remain open across redundant open calls")
	}
}

func TestCloseMemoryService_Idempotent(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	// Closing a never-opened service is the production shutdown path
	// when memory is disabled. It must not error.
	if err := closeMemoryService(); err != nil {
		t.Fatalf("close-when-closed should be no-op, got %v", err)
	}
	if memoryServiceOpen() {
		t.Fatalf("service should report closed")
	}
}

func TestMemoryService_AcceptsAskTenant(t *testing.T) {
	// Round-trip a minimal Write through the live service to prove the
	// embedded library is wired correctly and the configured tenant
	// schema accepts the {project, scope: "ask"} tuple ask uses. This
	// is the canary for "memmy v0.1.0 is actually importable and
	// functional from ask," not a unit test of memmy itself.
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	res, err := memorySvc.Write(context.Background(), memmy.WriteRequest{
		Tenant: map[string]string{
			"project": "/tmp/example",
			"scope":   memoryTenantScope,
		},
		Message: "ask + memmy integration smoke test",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.NodeIDs) == 0 {
		t.Errorf("Write should produce at least one node, got %+v", res)
	}
}

func TestMemoryService_RejectsForeignScope(t *testing.T) {
	// The schema pins scope to "ask" via Enum so future harnesses on
	// the same DB do not accidentally pollute ask's recall set. A
	// non-"ask" scope must be rejected at the schema layer.
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	_, err := memorySvc.Write(context.Background(), memmy.WriteRequest{
		Tenant: map[string]string{
			"project": "/tmp/example",
			"scope":   "intruder",
		},
		Message: "should not land",
	})
	if err == nil {
		t.Fatalf("expected schema rejection for foreign scope, got nil")
	}
}

func TestMemoryConfigEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  askConfig
		want bool
	}{
		{"unset is off", askConfig{}, false},
		{"explicit false", askConfig{Memory: memoryConfig{Enabled: boolPtr(false)}}, false},
		{"explicit true", askConfig{Memory: memoryConfig{Enabled: boolPtr(true)}}, true},
	}
	for _, c := range cases {
		if got := memoryConfigEnabled(c.cfg); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestMemoryWriteRecall_RoundTripsThroughTenant(t *testing.T) {
	// End-to-end: write through the helper, recall through the helper,
	// expect the same content back. The fake embedder is deterministic
	// so this is reproducible without a real model.
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	cwd := "/tmp/proj-a"
	ctx := context.Background()
	if err := memoryWrite(ctx, cwd, "the canary observation about session-id resolution"); err != nil {
		t.Fatalf("memoryWrite: %v", err)
	}
	hits, err := memoryRecall(ctx, cwd, "the canary observation about session-id resolution", 5)
	if err != nil {
		t.Fatalf("memoryRecall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit, got 0")
	}
}

func TestMemoryWrite_ServiceClosed_IsNoop(t *testing.T) {
	// Hooks call memoryWrite unconditionally; when memory is disabled
	// the call must succeed silently rather than erroring.
	isolateHome(t)
	resetMemoryService(t)

	err := memoryWrite(context.Background(), "/tmp/any", "ignored")
	if err != nil {
		t.Errorf("write while closed should be silent no-op, got %v", err)
	}
}

func TestMemoryRecall_ServiceClosed_ReturnsEmpty(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	hits, err := memoryRecall(context.Background(), "/tmp/any", "anything", 5)
	if err != nil {
		t.Errorf("recall while closed should be silent no-op, got %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("recall while closed should return empty, got %d hits", len(hits))
	}
}

func TestMemoryTenant_RejectsEmptyCwd(t *testing.T) {
	// memmy's tenant schema rejects empty values; we screen at the
	// helper layer so callers never have to construct the tuple.
	if got := memoryTenant(""); got != nil {
		t.Errorf("empty cwd should yield nil tenant, got %v", got)
	}
	got := memoryTenant("/abs/path")
	if got["project"] != "/abs/path" || got["scope"] != "ask" {
		t.Errorf("unexpected tenant tuple: %+v", got)
	}
}

func TestToggleMemoryEnabled_NoKey_PersistsAndSurfacesError(t *testing.T) {
	// The toggle persists the user's intent (Enabled=true) even when
	// the open fails for lack of a key, so a subsequent key paste
	// doesn't have to re-toggle. The picker shows "off (open failed)"
	// in the meantime — a status that distinguishes the failure mode
	// from a clean "off".
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)

	if memoryServiceOpen() {
		t.Fatalf("precondition: service must be closed")
	}

	newM, cmd := m.toggleMemoryEnabled()
	if _, ok := newM.(model); !ok {
		t.Fatalf("toggleMemoryEnabled returned %T, want model", newM)
	}

	if memoryServiceOpen() {
		t.Fatalf("toggle on without key should not open the service")
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Memory.Enabled == nil || *cfg.Memory.Enabled != true {
		t.Errorf("toggle on should persist intent regardless of open failure, got %+v", cfg.Memory.Enabled)
	}

	if cmd == nil {
		t.Errorf("expected toast cmd, got nil")
	}
}

func TestToggleMemoryEnabled_OnToOff_ClosesAndPersists(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	// Seed: persist on + open service (the live shape after a prior
	// startup). The toggle should bring both states down together.
	on := true
	if err := saveConfig(askConfig{Memory: memoryConfig{Enabled: &on}}); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	openFakeMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)

	if !memoryServiceOpen() {
		t.Fatalf("precondition: service must be open")
	}

	newM, cmd := m.toggleMemoryEnabled()
	if _, ok := newM.(model); !ok {
		t.Fatalf("toggleMemoryEnabled returned %T, want model", newM)
	}

	if memoryServiceOpen() {
		t.Fatalf("toggle off should have closed the service")
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Memory.Enabled == nil || *cfg.Memory.Enabled != false {
		t.Errorf("toggle off did not persist, got %+v", cfg.Memory.Enabled)
	}
	if cmd == nil {
		t.Errorf("expected toast cmd, got nil")
	}
}

func TestUpdateConfigMemoryPicker_EscClosesOnly(t *testing.T) {
	// Esc must dismiss the picker without altering memory state. This
	// guards against an accidental dispatch into toggleMemoryEnabled
	// when the user just wants to back out of the submenu.
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m = m.startConfigModal()
	m = m.openConfigMemoryPicker()

	newM, _ := m.updateConfigMemoryPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
	mm, ok := newM.(model)
	if !ok {
		t.Fatalf("updateConfigMemoryPicker returned %T, want model", newM)
	}
	if mm.configMemoryPickerActive {
		t.Errorf("Esc should have closed the picker")
	}
	if memoryServiceOpen() {
		t.Errorf("Esc should not have toggled the service")
	}
}

func TestConfigItemsAll_IncludesMemoryRow(t *testing.T) {
	// The /config main list must surface the Memory entry; without it
	// the submenu is unreachable from the UI.
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	items := m.configItemsAll()

	var found *configItem
	for i := range items {
		if items[i].id == "memory" {
			found = &items[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("configItemsAll missing memory row, got %+v", items)
	}
	if found.name != "Memory..." {
		t.Errorf("memory row name=%q want Memory...", found.name)
	}
	if found.key != "off" {
		t.Errorf("memory row summary=%q want 'off' when service closed", found.key)
	}
}

func TestConfigItemsAll_MemoryRowReflectsOpenState(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	for _, it := range m.configItemsAll() {
		if it.id == "memory" {
			if it.key != "on" {
				t.Errorf("memory row summary=%q want 'on' when service open", it.key)
			}
			return
		}
	}
	t.Fatal("memory row not found")
}

func TestMemoryPicker_GeminiKeyRowReflectsConfigState(t *testing.T) {
	// Without a key, the key row should say "(not set)". With a key,
	// it should say "configured" — never echo the actual key (that
	// would surface it in any screen recording / terminal scrollback).
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	rows := m.memoryPickerItems()
	if len(rows) < 2 {
		t.Fatalf("expected at least 2 picker rows, got %d", len(rows))
	}
	var keyRow *memoryPickerRow
	for i := range rows {
		if rows[i].id == "geminiKey" {
			keyRow = &rows[i]
		}
	}
	if keyRow == nil {
		t.Fatalf("memory picker missing geminiKey row")
	}
	if keyRow.key != "(not set)" {
		t.Errorf("expected '(not set)' when no key, got %q", keyRow.key)
	}

	// Seed a config with a key, re-evaluate.
	if err := saveConfig(askConfig{Memory: memoryConfig{GeminiKey: "AIza-secret"}}); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	rows = m.memoryPickerItems()
	for _, r := range rows {
		if r.id == "geminiKey" {
			if r.key != "configured" {
				t.Errorf("expected 'configured' when key set, got %q", r.key)
			}
			if strings.Contains(r.key, "AIza") {
				t.Errorf("key value should never be echoed in picker, got %q", r.key)
			}
			return
		}
	}
	t.Fatal("geminiKey row missing on second pass")
}

func TestMemoryPicker_EnterOnKeyRow_OpensEditor(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m = m.startConfigModal()
	m = m.openConfigMemoryPicker()
	// Move cursor to the geminiKey row (it's the second row).
	m.configMemoryCursor = 1

	newM, _ := m.updateConfigMemoryPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm, ok := newM.(model)
	if !ok {
		t.Fatalf("updateConfigMemoryPicker returned %T", newM)
	}
	if !mm.configMemoryKeyEditing {
		t.Errorf("Enter on geminiKey should open the editor")
	}
}

func TestMemoryKeyInput_BackspaceTrimsRune(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.configMemoryPickerActive = true
	m.configMemoryKeyEditing = true
	m.configMemoryKeyDraft = "abc"

	newM, _ := m.updateConfigMemoryKeyInput(tea.KeyPressMsg{Code: tea.KeyBackspace})
	mm := newM.(model)
	if mm.configMemoryKeyDraft != "ab" {
		t.Errorf("backspace should trim trailing rune: %q", mm.configMemoryKeyDraft)
	}
}

func TestMemoryKeyInput_EscapeCancelsWithoutSaving(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m.configMemoryPickerActive = true
	m.configMemoryKeyEditing = true
	m.configMemoryKeyDraft = "AIza-typed-but-not-saved"

	newM, _ := m.updateConfigMemoryKeyInput(tea.KeyPressMsg{Code: tea.KeyEsc})
	mm := newM.(model)
	if mm.configMemoryKeyEditing {
		t.Errorf("Esc should close the editor")
	}
	if mm.configMemoryKeyDraft != "" {
		t.Errorf("Esc should clear the draft: %q", mm.configMemoryKeyDraft)
	}
	cfg, _ := loadConfig()
	if cfg.Memory.GeminiKey != "" {
		t.Errorf("Esc must not have persisted the draft, got %q", cfg.Memory.GeminiKey)
	}
}

func TestMemoryKeyInput_EnterPersistsDraft(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m.configMemoryPickerActive = true
	m.configMemoryKeyEditing = true
	m.configMemoryKeyDraft = "  AIza-from-clipboard  " // includes whitespace to verify trim

	newM, cmd := m.updateConfigMemoryKeyInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := newM.(model)
	if mm.configMemoryKeyEditing {
		t.Errorf("Enter should close the editor")
	}
	cfg, _ := loadConfig()
	if cfg.Memory.GeminiKey != "AIza-from-clipboard" {
		t.Errorf("expected trimmed key persisted, got %q", cfg.Memory.GeminiKey)
	}
	if cmd == nil {
		t.Errorf("expected toast cmd, got nil")
	}
}

func TestMemoryKeyInput_ClearingPersistedKeyClosesService(t *testing.T) {
	// User had a key, opened the service, then opened the editor and
	// cleared the field. Saving an empty key must tear the service
	// down — leaving it dialing the prior key would be surprising.
	isolateHome(t)
	resetMemoryService(t)

	on := true
	if err := saveConfig(askConfig{Memory: memoryConfig{Enabled: &on, GeminiKey: "AIza-old"}}); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	openFakeMemoryService(t)
	if !memoryServiceOpen() {
		t.Fatalf("precondition: service must be open")
	}

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m.configMemoryPickerActive = true
	m.configMemoryKeyEditing = true
	m.configMemoryKeyDraft = "" // user erased the key

	_, _ = m.updateConfigMemoryKeyInput(tea.KeyPressMsg{Code: tea.KeyEnter})

	if memoryServiceOpen() {
		t.Errorf("clearing the key should have closed the service")
	}
	cfg, _ := loadConfig()
	if cfg.Memory.GeminiKey != "" {
		t.Errorf("expected empty key persisted, got %q", cfg.Memory.GeminiKey)
	}
}

func TestApplyConfigMemoryPaste_AppendsToDraft(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.configMemoryKeyEditing = true
	m.configMemoryKeyDraft = "AIza"
	newM, _ := m.applyConfigMemoryPaste("-pasted-tail")
	mm := newM.(model)
	if mm.configMemoryKeyDraft != "AIza-pasted-tail" {
		t.Errorf("paste should append to draft: %q", mm.configMemoryKeyDraft)
	}
}
