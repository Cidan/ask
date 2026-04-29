package main

import (
	"context"
	"os"
	"path/filepath"
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

func TestMemoryDBPath_UnderHome(t *testing.T) {
	home := isolateHome(t)
	got, err := memoryDBPath()
	if err != nil {
		t.Fatalf("memoryDBPath: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "ask", "memory.db")
	if got != want {
		t.Errorf("memoryDBPath=%q want %q", got, want)
	}
}

func TestOpenCloseMemoryService_RoundTrip(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	if memoryServiceOpen() {
		t.Fatalf("service should be closed before open")
	}
	if err := openMemoryService(); err != nil {
		t.Fatalf("openMemoryService: %v", err)
	}
	t.Cleanup(func() { _ = closeMemoryService() })

	if !memoryServiceOpen() {
		t.Fatalf("service should be open after open")
	}

	// File materializes on disk so the next ask invocation can find it.
	path, _ := memoryDBPath()
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

	if err := openMemoryService(); err != nil {
		t.Fatalf("first open: %v", err)
	}
	t.Cleanup(func() { _ = closeMemoryService() })
	if err := openMemoryService(); err != nil {
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

	if err := openMemoryService(); err != nil {
		t.Fatalf("openMemoryService: %v", err)
	}
	t.Cleanup(func() { _ = closeMemoryService() })

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

	if err := openMemoryService(); err != nil {
		t.Fatalf("openMemoryService: %v", err)
	}
	t.Cleanup(func() { _ = closeMemoryService() })

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

func TestToggleMemoryEnabled_OffToOn_OpensAndPersists(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)

	if memoryServiceOpen() {
		t.Fatalf("precondition: service must be closed")
	}

	newM, cmd := m.toggleMemoryEnabled()
	mm, ok := newM.(model)
	if !ok {
		t.Fatalf("toggleMemoryEnabled returned %T, want model", newM)
	}
	_ = mm

	t.Cleanup(func() { _ = closeMemoryService() })

	if !memoryServiceOpen() {
		t.Fatalf("toggle on should have opened the service")
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Memory.Enabled == nil || *cfg.Memory.Enabled != true {
		t.Errorf("toggle on did not persist, got %+v", cfg.Memory.Enabled)
	}

	if cmd == nil {
		t.Errorf("expected toast cmd, got nil")
	} else if msg := cmd(); msg == nil {
		t.Errorf("toast cmd produced nil msg")
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
	if err := openMemoryService(); err != nil {
		t.Fatalf("seed openMemoryService: %v", err)
	}
	t.Cleanup(func() { _ = closeMemoryService() })

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

func TestUpdateConfigMemoryPicker_EnterToggles(t *testing.T) {
	// End-to-end picker key flow: open the picker, hit Enter on the
	// (only) Enabled row, expect the service to come up.
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m = m.startConfigModal()
	m = m.openConfigMemoryPicker()
	if !m.configMemoryPickerActive {
		t.Fatalf("picker should be active after open")
	}

	newM, _ := m.updateConfigMemoryPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm, ok := newM.(model)
	if !ok {
		t.Fatalf("updateConfigMemoryPicker returned %T, want model", newM)
	}
	_ = mm
	t.Cleanup(func() { _ = closeMemoryService() })

	if !memoryServiceOpen() {
		t.Fatalf("Enter on Enabled row should have toggled service on")
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

	if err := openMemoryService(); err != nil {
		t.Fatalf("openMemoryService: %v", err)
	}
	t.Cleanup(func() { _ = closeMemoryService() })

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
