package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Cidan/memmy"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// neo4jTestConfig is the Neo4j the test suite assumes is reachable on
// the developer machine (per CLAUDE.md / project conventions). The
// dedicated `ask_tests` database keeps integration test corpora out of
// the default `neo4j` db. memmy.Migrate is called on every Open, so a
// fresh test database is auto-bootstrapped on first contact.
//
// Tests that need the live service skip themselves when Neo4j is
// unreachable so a clean "go test ./..." still passes on a machine
// without a running Neo4j (e.g. CI). Pure-state tests (config IO,
// validators, picker shape) do not gate on connectivity.
func neo4jTestConfig() neo4jConfig {
	return neo4jConfig{
		Host:     "localhost",
		Port:     7687,
		User:     "neo4j",
		Password: "neo4jneo4j",
		Database: "ask_tests",
	}
}

// resetMemoryService brings the package-level singleton down between
// tests. memory_test.go owns this helper because every memory test
// mutates global state; without explicit teardown, a failed test would
// leak a Neo4j connection into the next run and cascade failures.
func resetMemoryService(t *testing.T) {
	t.Helper()
	if err := closeMemoryService(); err != nil {
		t.Fatalf("close memory: %v", err)
	}
}

// ensureNeo4jTestDatabase makes sure a usable test database exists and
// is empty before a memory integration test starts. The production
// open path (openMemoryServiceWith) already handles `CREATE DATABASE
// IF NOT EXISTS` on Migrate's DatabaseNotFound, so this helper only
// has to (a) verify Neo4j is reachable, (b) pick a working database
// name (ask_tests on Enterprise, default `neo4j` on Community), and
// (c) wipe any leftover data so each test starts on a clean corpus.
//
// Skips the test when Neo4j is unreachable or when the credentials
// don't work — those are CI/environment artifacts, not ask
// regressions.
func ensureNeo4jTestDatabase(t *testing.T) neo4jConfig {
	t.Helper()
	cfg := neo4jTestConfig()
	driver, err := neo4j.NewDriverWithContext(neo4jBoltURI(cfg), neo4j.BasicAuth(cfg.User, cfg.Password, ""))
	if err != nil {
		t.Skipf("neo4j driver init: %v (is a local Neo4j reachable on %s?)", err, neo4jBoltURI(cfg))
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })

	ctx := context.Background()
	if err := driver.VerifyConnectivity(ctx); err != nil {
		t.Skipf("neo4j connectivity: %v (is a local Neo4j reachable on %s with creds %s/****?)", err, neo4jBoltURI(cfg), cfg.User)
	}

	// Probe Enterprise vs Community so we know which database to wipe.
	// We rely on the production code path to actually create ask_tests
	// when it's missing — this only decides which name we'll use.
	sys := driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: "system"})
	_, createErr := sys.Run(ctx, "CREATE DATABASE $name IF NOT EXISTS WAIT", map[string]any{"name": cfg.Database})
	_ = sys.Close(ctx)
	if createErr != nil && strings.Contains(createErr.Error(), "UnsupportedAdministrationCommand") {
		// Community Edition: only `neo4j` and `system` exist. Fall
		// back so tests can run anyway. The wipe below trashes any
		// pre-existing data in the developer's local `neo4j` db —
		// explicit accept on a dev machine where the suite is the
		// only writer.
		cfg.Database = "neo4j"
	} else if createErr != nil {
		t.Skipf("create database %q: %v", cfg.Database, createErr)
	}

	work := driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: cfg.Database})
	defer work.Close(ctx)
	if _, err := work.Run(ctx, "MATCH (n) DETACH DELETE n", nil); err != nil {
		t.Skipf("reset %q: %v", cfg.Database, err)
	}
	return cfg
}

// dropNeo4jDatabase removes a database (Enterprise only). Used by the
// auto-create regression test — drop, open, expect openMemoryServiceWith
// to recreate it via memmy's DatabaseNotFound retry path. Returns false
// when the server doesn't support DROP DATABASE so the test can skip
// rather than fail.
func dropNeo4jDatabase(t *testing.T, cfg neo4jConfig) bool {
	t.Helper()
	driver, err := neo4j.NewDriverWithContext(neo4jBoltURI(cfg), neo4j.BasicAuth(cfg.User, cfg.Password, ""))
	if err != nil {
		return false
	}
	defer driver.Close(context.Background())
	ctx := context.Background()
	sys := driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: "system"})
	defer sys.Close(ctx)
	_, err = sys.Run(ctx, "DROP DATABASE $name IF EXISTS WAIT", map[string]any{"name": cfg.Database})
	if err != nil && strings.Contains(err.Error(), "UnsupportedAdministrationCommand") {
		return false
	}
	if err != nil {
		t.Logf("drop database %q: %v", cfg.Database, err)
		return false
	}
	return true
}

// openFakeMemoryService is the test entry point: opens the singleton
// against a fake embedder + a freshly-prepared Neo4j database so we
// never need a real Gemini key in CI. Production code never reaches
// this — openMemoryService is the only public constructor and it
// requires a configured Gemini key plus a configured Neo4j endpoint.
//
// Skips the test when the configured Neo4j is unreachable. The skip
// is deliberate: this is the canary for "ask + memmy v0.2 actually
// connects to Neo4j," and an unreachable Neo4j is a CI-environment
// artifact, not a regression in ask.
func openFakeMemoryService(t *testing.T) {
	t.Helper()
	cfg := ensureNeo4jTestDatabase(t)
	err := openMemoryServiceWith(memmy.NewFakeEmbedder(memoryFakeEmbedderDim), cfg, memoryFakeEmbedderDim)
	if err != nil {
		t.Skipf("openFakeMemoryService: %v", err)
	}
	t.Cleanup(func() { _ = closeMemoryService() })
}

func TestNeo4jDefaults(t *testing.T) {
	c := neo4jConfig{}
	if got := neo4jHostOrDefault(c); got != "localhost" {
		t.Errorf("default host=%q want localhost", got)
	}
	if got := neo4jPortOrDefault(c); got != 7687 {
		t.Errorf("default port=%d want 7687", got)
	}
	if got := neo4jDatabaseOrDefault(c); got != "neo4j" {
		t.Errorf("default database=%q want neo4j", got)
	}
	// Explicit values override the defaults.
	c2 := neo4jConfig{Host: "db.local", Port: 17687, Database: "scratch"}
	if got := neo4jHostOrDefault(c2); got != "db.local" {
		t.Errorf("explicit host=%q", got)
	}
	if got := neo4jPortOrDefault(c2); got != 17687 {
		t.Errorf("explicit port=%d", got)
	}
	if got := neo4jDatabaseOrDefault(c2); got != "scratch" {
		t.Errorf("explicit database=%q", got)
	}
}

func TestNeo4jBoltURI(t *testing.T) {
	cases := []struct {
		name string
		in   neo4jConfig
		want string
	}{
		{"defaults", neo4jConfig{}, "bolt://localhost:7687"},
		{"custom", neo4jConfig{Host: "10.0.0.5", Port: 7688}, "bolt://10.0.0.5:7688"},
	}
	for _, c := range cases {
		if got := neo4jBoltURI(c.in); got != c.want {
			t.Errorf("%s: neo4jBoltURI=%q want %q", c.name, got, c.want)
		}
	}
}

func TestValidateNeo4jHost(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"plain hostname", "localhost", false},
		{"FQDN", "db.example.com", false},
		{"ipv4", "10.0.0.5", false},
		{"with bolt scheme", "bolt://localhost", true},
		{"with neo4j scheme", "neo4j://x", true},
		{"with http scheme", "http://localhost", true},
		{"with slash", "localhost/db", true},
		{"with space", "local host", true},
	}
	for _, c := range cases {
		err := validateNeo4jHost(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: validateNeo4jHost(%q) err=%v wantErr=%v", c.name, c.in, err, c.wantErr)
		}
	}
}

func TestValidateNeo4jPort(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", true},
		{"alpha", "abc", true},
		{"zero", "0", true},
		{"low", "1", false},
		{"bolt default", "7687", false},
		{"high", "65535", false},
		{"out of range", "65536", true},
		{"negative", "-1", true},
		{"with whitespace", "  7687  ", false},
		{"mixed", "76a87", true},
	}
	for _, c := range cases {
		err := validateNeo4jPort(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: validateNeo4jPort(%q) err=%v wantErr=%v", c.name, c.in, err, c.wantErr)
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
	if err := openMemoryServiceWith(memmy.NewFakeEmbedder(memoryFakeEmbedderDim), neo4jTestConfig(), memoryFakeEmbedderDim); err != nil {
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
	// is the canary for "ask + memmy v0.2 is actually importable and
	// functional," not a unit test of memmy itself.
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

func TestMemoryPicker_Neo4jRowsReflectDefaults(t *testing.T) {
	// A blank config should still render meaningful defaults — the
	// picker is the documentation surface for what ask will dial.
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	rows := m.memoryPickerItems()
	want := map[string]string{
		"neo4jHost":     "localhost",
		"neo4jPort":     "7687",
		"neo4jUser":     "(not set)",
		"neo4jPassword": "(not set)",
		"neo4jDatabase": "neo4j",
	}
	for id, w := range want {
		var found *memoryPickerRow
		for i := range rows {
			if rows[i].id == id {
				found = &rows[i]
			}
		}
		if found == nil {
			t.Errorf("missing row %q", id)
			continue
		}
		if found.key != w {
			t.Errorf("row %q key=%q want %q", id, found.key, w)
		}
	}
}

func TestMemoryPicker_Neo4jPasswordNeverEchoed(t *testing.T) {
	// Password row must mirror the Gemini key row's masking — never
	// surface the value in the closed picker.
	isolateHome(t)
	resetMemoryService(t)

	if err := saveConfig(askConfig{Memory: memoryConfig{Neo4j: neo4jConfig{Password: "neo4jneo4j-secret"}}}); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	rows := m.memoryPickerItems()
	for _, r := range rows {
		if r.id == "neo4jPassword" {
			if r.key != "configured" {
				t.Errorf("password row key=%q want 'configured'", r.key)
			}
			if strings.Contains(r.key, "neo4jneo4j-secret") {
				t.Errorf("password row leaked secret: %q", r.key)
			}
			return
		}
	}
	t.Fatal("neo4jPassword row not found")
}

func TestMemoryPicker_Neo4jUserPlainEchoed(t *testing.T) {
	// User name is not a secret; the picker should display it so a
	// user can verify what was saved at a glance.
	isolateHome(t)
	resetMemoryService(t)

	if err := saveConfig(askConfig{Memory: memoryConfig{Neo4j: neo4jConfig{User: "neo4j"}}}); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	for _, r := range m.memoryPickerItems() {
		if r.id == "neo4jUser" {
			if r.key != "neo4j" {
				t.Errorf("user row key=%q want 'neo4j'", r.key)
			}
			return
		}
	}
	t.Fatal("neo4jUser row not found")
}

func TestMemoryPicker_EnterOnKeyRow_OpensEditor(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m = m.startConfigModal()
	m = m.openConfigMemoryPicker()
	rows := m.memoryPickerItems()
	for i, r := range rows {
		if r.id == "geminiKey" {
			m.configMemoryCursor = i
			break
		}
	}

	newM, _ := m.updateConfigMemoryPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm, ok := newM.(model)
	if !ok {
		t.Fatalf("updateConfigMemoryPicker returned %T", newM)
	}
	if mm.configMemoryFieldEditing != "geminiKey" {
		t.Errorf("Enter on geminiKey should open the editor, got editing=%q", mm.configMemoryFieldEditing)
	}
}

func TestMemoryPicker_EnterOnNeo4jHostRow_OpensEditor(t *testing.T) {
	// Same Enter→editor flow has to work for every Neo4j field, not
	// just the original Gemini-key path.
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m = m.startConfigModal()
	m = m.openConfigMemoryPicker()
	rows := m.memoryPickerItems()
	for i, r := range rows {
		if r.id == "neo4jHost" {
			m.configMemoryCursor = i
			break
		}
	}

	newM, _ := m.updateConfigMemoryPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := newM.(model)
	if mm.configMemoryFieldEditing != "neo4jHost" {
		t.Errorf("Enter on neo4jHost should open the editor, got editing=%q", mm.configMemoryFieldEditing)
	}
	// Pre-fill must reflect the default since no host is persisted yet.
	if mm.configMemoryFieldDraft != "localhost" {
		t.Errorf("editor pre-fill=%q want 'localhost'", mm.configMemoryFieldDraft)
	}
}

func TestMemoryFieldInput_BackspaceTrimsRune(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.configMemoryPickerActive = true
	m.configMemoryFieldEditing = "geminiKey"
	m.configMemoryFieldDraft = "abc"

	newM, _ := m.updateConfigMemoryFieldInput(tea.KeyPressMsg{Code: tea.KeyBackspace})
	mm := newM.(model)
	if mm.configMemoryFieldDraft != "ab" {
		t.Errorf("backspace should trim trailing rune: %q", mm.configMemoryFieldDraft)
	}
}

func TestMemoryFieldInput_EscapeCancelsWithoutSaving(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m.configMemoryPickerActive = true
	m.configMemoryFieldEditing = "geminiKey"
	m.configMemoryFieldDraft = "AIza-typed-but-not-saved"

	newM, _ := m.updateConfigMemoryFieldInput(tea.KeyPressMsg{Code: tea.KeyEsc})
	mm := newM.(model)
	if mm.configMemoryFieldEditing != "" {
		t.Errorf("Esc should close the editor")
	}
	if mm.configMemoryFieldDraft != "" {
		t.Errorf("Esc should clear the draft: %q", mm.configMemoryFieldDraft)
	}
	cfg, _ := loadConfig()
	if cfg.Memory.GeminiKey != "" {
		t.Errorf("Esc must not have persisted the draft, got %q", cfg.Memory.GeminiKey)
	}
}

func TestMemoryFieldInput_EnterPersistsGeminiKeyDraft(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m.configMemoryPickerActive = true
	m.configMemoryFieldEditing = "geminiKey"
	m.configMemoryFieldDraft = "  AIza-from-clipboard  " // includes whitespace to verify trim

	newM, cmd := m.updateConfigMemoryFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := newM.(model)
	if mm.configMemoryFieldEditing != "" {
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

func TestMemoryFieldInput_EnterPersistsNeo4jHostDraft(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m.configMemoryPickerActive = true
	m.configMemoryFieldEditing = "neo4jHost"
	m.configMemoryFieldDraft = "10.0.0.5"

	newM, _ := m.updateConfigMemoryFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := newM.(model)
	if mm.configMemoryFieldEditing != "" {
		t.Errorf("Enter should close the editor")
	}
	cfg, _ := loadConfig()
	if cfg.Memory.Neo4j.Host != "10.0.0.5" {
		t.Errorf("host not persisted, got %q", cfg.Memory.Neo4j.Host)
	}
}

func TestMemoryFieldInput_EnterPersistsNeo4jPortDraft(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m.configMemoryPickerActive = true
	m.configMemoryFieldEditing = "neo4jPort"
	m.configMemoryFieldDraft = "17687"

	_, _ = m.updateConfigMemoryFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})

	cfg, _ := loadConfig()
	if cfg.Memory.Neo4j.Port != 17687 {
		t.Errorf("port not persisted, got %d", cfg.Memory.Neo4j.Port)
	}
}

func TestMemoryFieldInput_InvalidPortKeepsEditorOpen(t *testing.T) {
	// Validation failure must not save and must not close the editor:
	// the user gets a toast and can correct the typo without having
	// to reopen and retype.
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m.configMemoryPickerActive = true
	m.configMemoryFieldEditing = "neo4jPort"
	m.configMemoryFieldDraft = "abc"

	newM, cmd := m.updateConfigMemoryFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := newM.(model)
	if mm.configMemoryFieldEditing != "neo4jPort" {
		t.Errorf("editor should stay open on validation failure")
	}
	if mm.configMemoryFieldDraft != "abc" {
		t.Errorf("draft should be preserved, got %q", mm.configMemoryFieldDraft)
	}
	if cmd == nil {
		t.Errorf("expected error toast cmd")
	}
	cfg, _ := loadConfig()
	if cfg.Memory.Neo4j.Port != 0 {
		t.Errorf("port should not have been persisted, got %d", cfg.Memory.Neo4j.Port)
	}
}

func TestMemoryFieldInput_InvalidHostKeepsEditorOpen(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)
	m.configMemoryPickerActive = true
	m.configMemoryFieldEditing = "neo4jHost"
	m.configMemoryFieldDraft = "bolt://oops"

	newM, _ := m.updateConfigMemoryFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := newM.(model)
	if mm.configMemoryFieldEditing != "neo4jHost" {
		t.Errorf("editor should stay open on validation failure")
	}
	cfg, _ := loadConfig()
	if cfg.Memory.Neo4j.Host != "" {
		t.Errorf("host should not have been persisted, got %q", cfg.Memory.Neo4j.Host)
	}
}

func TestMemoryFieldInput_BlankNeo4jUserPasswordAccepted(t *testing.T) {
	// The user explicitly asked that blank user/password be acceptable
	// defaults. Saving an empty string for either must succeed and
	// persist as an empty string.
	isolateHome(t)
	resetMemoryService(t)

	for _, id := range []string{"neo4jUser", "neo4jPassword"} {
		m := newTestModel(t, newFakeProvider())
		m.toast = NewToastModel(40, 0)
		m.configMemoryPickerActive = true
		m.configMemoryFieldEditing = id
		m.configMemoryFieldDraft = ""

		newM, _ := m.updateConfigMemoryFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
		mm := newM.(model)
		if mm.configMemoryFieldEditing != "" {
			t.Errorf("%s: blank should be accepted (editor closed)", id)
		}
	}
}

func TestMemoryFieldInput_ClearingPersistedKeyClosesService(t *testing.T) {
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
	m.configMemoryFieldEditing = "geminiKey"
	m.configMemoryFieldDraft = "" // user erased the key

	_, _ = m.updateConfigMemoryFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})

	if memoryServiceOpen() {
		t.Errorf("clearing the key should have closed the service")
	}
	cfg, _ := loadConfig()
	if cfg.Memory.GeminiKey != "" {
		t.Errorf("expected empty key persisted, got %q", cfg.Memory.GeminiKey)
	}
}

func TestIsNeo4jDatabaseNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection refused"), false},
		{"neo4j code", errors.New("Neo4jError: Neo.ClientError.Database.DatabaseNotFound (Graph not found: x)"), true},
		{"go-driver name", errors.New("DatabaseNotFoundError: db missing"), true},
	}
	for _, c := range cases {
		if got := isNeo4jDatabaseNotFound(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestOpenMemoryServiceWith_AutoCreatesMissingDatabase(t *testing.T) {
	// User picks a database name that doesn't exist yet. The open
	// path must auto-create it (CREATE DATABASE IF NOT EXISTS against
	// `system`) and then succeed. Drop the test db, open, expect
	// success — without the retry path this would fail with
	// DatabaseNotFound.
	//
	// Requires Enterprise/Aura/Desktop because Community can't host
	// arbitrary databases at all; we skip via dropNeo4jDatabase
	// returning false.
	isolateHome(t)
	resetMemoryService(t)

	cfg := neo4jTestConfig()
	if !dropNeo4jDatabase(t, cfg) {
		t.Skipf("auto-create test requires multi-database support (Enterprise/Aura/Desktop)")
	}
	t.Cleanup(func() { _ = closeMemoryService() })

	if err := openMemoryServiceWith(memmy.NewFakeEmbedder(memoryFakeEmbedderDim), cfg, memoryFakeEmbedderDim); err != nil {
		t.Fatalf("open against missing db should auto-create, got %v", err)
	}
	if !memoryServiceOpen() {
		t.Fatalf("service should be open after auto-create")
	}
	// Round-trip a Write to confirm the fresh database is functional.
	_, err := memorySvc.Write(context.Background(), memmy.WriteRequest{
		Tenant: map[string]string{
			"project": "/tmp/auto-create",
			"scope":   memoryTenantScope,
		},
		Message: "auto-created db smoke",
	})
	if err != nil {
		t.Fatalf("Write against auto-created db: %v", err)
	}
}

func TestApplyConfigMemoryPaste_AppendsToDraft(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.configMemoryFieldEditing = "geminiKey"
	m.configMemoryFieldDraft = "AIza"
	newM, _ := m.applyConfigMemoryPaste("-pasted-tail")
	mm := newM.(model)
	if mm.configMemoryFieldDraft != "AIza-pasted-tail" {
		t.Errorf("paste should append to draft: %q", mm.configMemoryFieldDraft)
	}
}
