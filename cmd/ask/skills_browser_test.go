package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/plugin"
)

func writeSkillsFixtureMarketplace(t *testing.T, dir, name string) {
	t.Helper()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude-plugin/marketplace.json", `{"name":"`+name+`","owner":{"name":"t"},"plugins":[
  {"name":"tools","source":"./plugins/tools","description":"Spreadsheet tooling","version":"1.0.0"},
  {"name":"other","source":"./plugins/other","description":"Unrelated"}]}`)
	write("plugins/tools/skills/xlsx/SKILL.md", "---\nname: xlsx\ndescription: Work with spreadsheets\n---\nUse openpyxl.\n")
	write("plugins/tools/agents/reviewer.md", "---\nname: reviewer\ndescription: Reviews code\n---\nReview.\n")
	write("plugins/tools/workflows/release.json", `{"name":"release","steps":[{"name":"plan","provider":"vertex","prompt":"p"}]}`)
	write("plugins/other/skills/misc/SKILL.md", "---\nname: misc\ndescription: Misc\n---\nx\n")
}

// skillsBrowserFixture builds a tab over a checkout holding a project
// skill + agent + workflow, a user skill, and one registered directory
// marketplace with two plugins (none installed).
func skillsBrowserFixture(t *testing.T) (model, *fakeProvider, string) {
	t.Helper()
	home := isolateHome(t)
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	prov := newFakeProvider()
	m := newTestModel(t, prov)
	m.toast = NewToastModel(80, 0)
	if err := os.MkdirAll(filepath.Join(m.cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateSkill(m.cwd, engine.OriginProject, engine.SkillSpec{Name: "deploy", Description: "Deploy the service", Body: "1. build\n2. ship"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateSkill(m.cwd, engine.OriginUser, engine.SkillSpec{Name: "notes", Description: "Take notes", Body: "write notes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateAgent(m.cwd, engine.OriginProject, engine.AgentSpec{Name: "critic", Description: "Critiques plans", Prompt: "Be harsh."}); err != nil {
		t.Fatal(err)
	}
	if err := mutateWorkflows(m.cwd, func(items []workflowDef) ([]workflowDef, error) {
		return append(items, workflowDef{Name: "ship", Scope: workflowScopeRepo, Steps: []workflowStep{{Name: "s1", Provider: "vertex"}}}), nil
	}); err != nil {
		t.Fatal(err)
	}
	mkt := filepath.Join(home, "fixture-mkt")
	writeSkillsFixtureMarketplace(t, mkt, "mkt")
	if _, err := plugin.AddMarketplace(context.Background(), m.cwd, mkt, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}
	return m, prov, mkt
}

func skillsRowsByTitle(rows []skillsRow) map[string]skillsRow {
	out := map[string]skillsRow{}
	for _, r := range rows {
		switch r.kind {
		case skillsRowHeader, skillsRowAction, skillsRowNote:
			out[r.title] = r
		case skillsRowItem:
			out[r.item.kind+":"+r.item.name] = r
		case skillsRowPlugin:
			out["plugin:"+r.plugin.ref] = r
		}
	}
	return out
}

func moveSkillsCursorTo(t *testing.T, m model, match func(skillsRow) bool) model {
	t.Helper()
	s := m.skillsBrowser
	rows := s.rows()
	for i, r := range rows {
		if r.selectable() && match(r) {
			s.cursor = i
			s.detailScroll = 0
			return m
		}
	}
	t.Fatalf("no selectable row matched; rows=%v", skillsRowTitles(rows))
	return m
}

func skillsRowTitles(rows []skillsRow) []string {
	var out []string
	for k := range skillsRowsByTitle(rows) {
		out = append(out, k)
	}
	return out
}

// runSkillsOp executes the async op cmd the browser returned and feeds
// its completion back through Update, returning the follow-up cmd.
func runSkillsOp(t *testing.T, m model, cmd tea.Cmd) (model, tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected an async op cmd")
	}
	var done *skillsBrowserOpDoneMsg
	for _, msg := range drainBatch(t, cmd) {
		if d, ok := msg.(skillsBrowserOpDoneMsg); ok {
			done = &d
		}
	}
	if done == nil {
		t.Fatal("op cmd did not resolve to skillsBrowserOpDoneMsg")
	}
	if done.err != nil {
		t.Fatalf("op failed: %v", done.err)
	}
	mi, next := m.Update(*done)
	return mi.(model), next
}

func TestSkillsBrowser_OpenAndClose(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	m = stepKey(t, m, pressKey('s', tea.ModCtrl))
	if m.mode != modeSkillsBrowser || m.skillsBrowser == nil {
		t.Fatalf("Ctrl+S must open the browser: mode=%v", m.mode)
	}
	if m.skillsBrowser.lens != skillsLensInstalled {
		t.Error("browser opens on the installed lens")
	}
	if !m.wantsTabKey() {
		t.Error("an open browser owns Tab (lens toggle), the sidebar must not steal it")
	}
	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	if m.mode != modeInput || m.skillsBrowser != nil {
		t.Fatal("Esc must close the browser")
	}

	mi, _ := m.handleCommand("/skills")
	m = mi.(model)
	if m.mode != modeSkillsBrowser {
		t.Fatal("/skills must open the browser")
	}
	m = stepKey(t, m, pressKey('c', tea.ModCtrl))
	if m.mode != modeInput {
		t.Fatal("Ctrl+C must close the browser")
	}
	m.mode = modeConfig
	mi, _ = m.handleCommand("/skills")
	if mi.(model).mode == modeSkillsBrowser {
		t.Fatal("/skills must not open over another modal")
	}
	found := false
	for _, c := range appBuiltinSlashCmds {
		if c.name == "/skills" {
			found = true
		}
	}
	if !found {
		t.Error("/skills must be in the slash menu")
	}
}

func TestSkillsBrowser_InstalledLensGroupsByOrigin(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	m = m.openSkillsBrowser()
	s := m.skillsBrowser
	rows := s.rows()
	by := skillsRowsByTitle(rows)
	for _, want := range []string{"Project", "User", "skill:deploy", "skill:notes", "agent:critic", "workflow:ship"} {
		if _, ok := by[want]; !ok {
			t.Errorf("missing row %q in %v", want, skillsRowTitles(rows))
		}
	}
	if by["skill:deploy"].item.origin.Scope != engine.OriginProject || by["skill:notes"].item.origin.Scope != engine.OriginUser {
		t.Error("origins wrong")
	}
	if wf := by["workflow:ship"].item; len(wf.warnings) != 1 || !strings.Contains(wf.warnings[0], "not configured") {
		t.Errorf("unconfigured step provider must warn on the workflow item: %+v", wf.warnings)
	}
	if !rows[s.cursor].selectable() {
		t.Error("cursor must land on a selectable row")
	}
	// Header before items: Project group first.
	if rows[0].kind != skillsRowHeader || rows[0].title != "Project" {
		t.Errorf("first row = %+v, want Project header", rows[0])
	}
	// Type-to-search narrows to matching items.
	m = pressText(t, m, "crit")
	rows = s.rows()
	by = skillsRowsByTitle(rows)
	if _, ok := by["agent:critic"]; !ok || len(by) != 2 {
		t.Errorf("filter must keep only critic (+ its header): %v", skillsRowTitles(rows))
	}
	m = stepKey(t, m, pressSpecial(tea.KeyBackspace))
	if s.query != "cri" {
		t.Errorf("backspace trims the query: %q", s.query)
	}
}

func TestSkillsBrowser_EnterInsertsSlashCommand(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	m = m.openSkillsBrowser()
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "deploy" })
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.mode != modeInput || m.skillsBrowser != nil {
		t.Fatal("Enter on a skill closes the browser")
	}
	if got := m.input.Value(); got != "/deploy " {
		t.Fatalf("input = %q, want the slash command", got)
	}
	m = m.openSkillsBrowser()
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.kind == "agent" })
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if !strings.Contains(m.input.Value(), "critic agent") {
		t.Fatalf("Enter on an agent pre-fills a delegation: %q", m.input.Value())
	}
}

func TestSkillsBrowser_MarketplaceLensAndDrill(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	m = m.openSkillsBrowser()
	s := m.skillsBrowser
	m = stepKey(t, m, pressSpecial(tea.KeyTab))
	if s.lens != skillsLensMarketplace {
		t.Fatal("Tab switches to the marketplace lens")
	}
	rows := s.rows()
	by := skillsRowsByTitle(rows)
	for _, want := range []string{"mkt", "plugin:tools@mkt", "plugin:other@mkt", "+ Add marketplace", "Import from Claude Code", "+ New marketplace"} {
		if _, ok := by[want]; !ok {
			t.Errorf("missing %q in %v", want, skillsRowTitles(rows))
		}
	}
	if by["plugin:tools@mkt"].plugin.installed {
		t.Error("nothing is installed yet")
	}
	if by["plugin:tools@mkt"].plugin.dir == "" {
		t.Error("path-source plugins resolve to a local dir for browsing before install")
	}
	m = pressText(t, m, "tools")
	if _, ok := skillsRowsByTitle(s.rows())["plugin:other@mkt"]; ok {
		t.Error("query must filter plugins")
	}
	s.query = ""
	s.resetCursorToFirst()
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowPlugin && r.plugin.entry.Name == "tools" })
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if s.drill == nil || s.drill.ref != "tools@mkt" {
		t.Fatal("Enter drills into the plugin")
	}
	by = skillsRowsByTitle(s.rows())
	for _, want := range []string{"tools@mkt", "skill:tools:xlsx", "agent:tools:reviewer", "workflow:release"} {
		if _, ok := by[want]; !ok {
			t.Errorf("drill rows missing %q: %v", want, skillsRowTitles(s.rows()))
		}
	}
	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	if s.drill != nil || m.mode != modeSkillsBrowser {
		t.Fatal("Esc leaves the drill without closing the browser")
	}
	if row, ok := s.current(); !ok || row.kind != skillsRowPlugin || row.plugin.ref != "tools@mkt" {
		t.Errorf("cursor returns to the drilled plugin: %+v", row)
	}
	m = stepKey(t, m, pressSpecial(tea.KeyTab))
	if s.lens != skillsLensInstalled {
		t.Fatal("Tab toggles back")
	}
}

func TestSkillsBrowser_InstallAndUninstallFlow(t *testing.T) {
	m, prov, _ := skillsBrowserFixture(t)
	probes := 0
	prov.probeInitFn = func(ProviderSessionArgs) tea.Cmd {
		probes++
		return nil
	}
	m = m.openSkillsBrowser()
	s := m.skillsBrowser
	s.setLens(skillsLensMarketplace)
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowPlugin && r.plugin.entry.Name == "tools" })
	m = stepKey(t, m, pressKey('g', tea.ModCtrl))
	if s.editor == nil || s.editor.kind != skillsEditorScope {
		t.Fatal("ctrl+g opens the scope prompt")
	}
	mi, cmd := m.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	m = mi.(model)
	if s.editor != nil || s.busy == "" {
		t.Fatal("choosing a scope starts the async install")
	}
	m, next := runSkillsOp(t, m, cmd)
	s = m.skillsBrowser
	if s.busy != "" {
		t.Error("busy clears when the op lands")
	}
	if pf := plugin.ReadProjectFile(m.cwd); !pf.Enabled["tools@mkt"] {
		t.Fatalf("project-scope install must write .ask/plugins.json: %+v", pf)
	}
	by := skillsRowsByTitle(s.rows())
	if row, ok := by["plugin:tools@mkt"]; !ok || !row.plugin.installed || row.plugin.scopes[0] != "project" {
		t.Fatalf("catalog row must show installed: %+v", row)
	}
	s.setLens(skillsLensInstalled)
	by = skillsRowsByTitle(s.rows())
	for _, want := range []string{"tools@mkt", "skill:tools:xlsx", "agent:tools:reviewer", "workflow:release"} {
		if _, ok := by[want]; !ok {
			t.Errorf("installed lens missing %q: %v", want, skillsRowTitles(s.rows()))
		}
	}
	// The follow-up broadcasts extensionsChangedMsg; feeding it re-probes
	// slash commands so the new plugin skills are invocable.
	var changed bool
	for _, msg := range drainBatch(t, next) {
		if _, ok := msg.(extensionsChangedMsg); ok {
			changed = true
			mi, _ := m.Update(msg)
			m = mi.(model)
		}
	}
	if !changed || probes != 1 {
		t.Fatalf("extensionsChangedMsg=%v probes=%d", changed, probes)
	}

	// Uninstall from the installed lens via the plugin item.
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "tools:xlsx" })
	m = stepKey(t, m, pressSpecial(tea.KeyDelete))
	if s.editor == nil || s.editor.kind != skillsEditorConfirm || !strings.Contains(s.editor.title, "Uninstall tools@mkt") {
		t.Fatalf("del on a plugin item asks to uninstall the plugin: %+v", s.editor)
	}
	m = stepKey(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if s.editor != nil {
		t.Fatal("n cancels")
	}
	// Ctrl+D is delete inside the browser, never close-tab.
	mi, cmd = m.Update(pressKey('d', tea.ModCtrl))
	m = mi.(model)
	if m.mode != modeSkillsBrowser || s.editor == nil || s.editor.kind != skillsEditorConfirm {
		t.Fatalf("ctrl+d must open the uninstall confirm, not close the tab: mode=%v editor=%+v", m.mode, s.editor)
	}
	for _, out := range drainBatch(t, cmd) {
		if _, ok := out.(closeTabMsg); ok {
			t.Fatal("ctrl+d must not close the tab")
		}
	}
	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	m = stepKey(t, m, pressSpecial(tea.KeyDelete))
	mi, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	if _, ok := skillsRowsByTitle(m.skillsBrowser.rows())["skill:tools:xlsx"]; ok {
		t.Fatal("uninstalled plugin items must disappear")
	}
	if pf := plugin.ReadProjectFile(m.cwd); pf.Enabled["tools@mkt"] {
		t.Fatal("project file must drop the plugin")
	}
}

func TestSkillsBrowser_AddMarketplaceEditorAndSlash(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	home, _ := os.UserHomeDir()
	second := filepath.Join(home, "second-mkt")
	writeSkillsFixtureMarketplace(t, second, "second")
	m = m.openSkillsBrowser()
	s := m.skillsBrowser
	m = stepKey(t, m, pressKey('a', tea.ModCtrl))
	if s.editor == nil || s.editor.kind != skillsEditorAddMarketplace {
		t.Fatal("ctrl+a opens the add-marketplace editor")
	}
	mi, _ := m.Update(tea.PasteMsg{Content: second})
	m = mi.(model)
	if s.editor.draft != second {
		t.Fatalf("paste lands in the editor draft: %q", s.editor.draft)
	}
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if s.editor == nil || s.editor.kind != skillsEditorScope {
		t.Fatal("Enter asks for the scope")
	}
	mi, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	names := map[string]bool{}
	for _, mk := range m.skillsBrowser.marketplaces {
		names[mk.Name] = true
	}
	if !names["mkt"] || !names["second"] {
		t.Fatalf("marketplaces after add: %v", names)
	}

	third := filepath.Join(home, "third-mkt")
	writeSkillsFixtureMarketplace(t, third, "third")
	m = m.closeSkillsBrowser()
	mi, cmd = m.handleCommand("/skills add marketplace " + third + " project")
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	mk, ok := plugin.FindMarketplace(m.cwd, "third")
	if !ok || mk.Scope != plugin.ScopeProject {
		t.Fatalf("/skills add marketplace … project must register at project scope: %+v %v", mk, ok)
	}
	mi, cmd = m.handleCommand("/skills remove marketplace third")
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	if _, ok := plugin.FindMarketplace(m.cwd, "third"); ok {
		t.Fatal("/skills remove marketplace must unregister")
	}
	mi, cmd = m.handleCommand("/skills bogus")
	if cmd != nil || len(mi.(model).history) == 0 {
		t.Fatal("unknown subcommand prints usage")
	}
}

func TestSkillsBrowser_DeleteAndPublish(t *testing.T) {
	m, _, mkt := skillsBrowserFixture(t)
	m = m.openSkillsBrowser()
	s := m.skillsBrowser
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "notes" })
	m = stepKey(t, m, pressSpecial(tea.KeyDelete))
	if s.editor == nil || s.editor.kind != skillsEditorConfirm || !strings.Contains(s.editor.title, "Delete skill notes") {
		t.Fatalf("del asks for confirmation: %+v", s.editor)
	}
	mi, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	if _, ok := engine.FindSkill(m.cwd, "notes"); ok {
		t.Fatal("skill must be deleted")
	}

	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "critic" })
	m = stepKey(t, m, pressKey('p', tea.ModCtrl))
	s = m.skillsBrowser
	if s.editor == nil || s.editor.kind != skillsEditorPublish || len(s.editor.options) != 1 || s.editor.options[0] != "mkt" {
		t.Fatalf("ctrl+p offers the writable marketplaces: %+v", s.editor)
	}
	mi, cmd = m.Update(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	if _, err := os.Stat(filepath.Join(mkt, "plugins", "critic", "agents", "critic.md")); err != nil {
		t.Fatal("publish must copy the agent into the marketplace")
	}
	if row, ok := skillsRowsByTitle(m.skillsBrowser.rows())["skill:deploy"]; !ok || row.item == nil {
		t.Fatal("browser rebuilt after publish")
	}
	if s := m.skillsBrowser; len(s.catalog) != 3 {
		t.Fatalf("catalog must list the published plugin: %d", len(s.catalog))
	}
	// Workflows publish too.
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.kind == "workflow" })
	m = stepKey(t, m, pressKey('p', tea.ModCtrl))
	mi, cmd = m.Update(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	if _, err := os.Stat(filepath.Join(mkt, "plugins", "ship", "workflows", "ship.json")); err != nil {
		t.Fatal("publish must export the workflow")
	}
}

func TestSkillsBrowser_NewAndEditHandOffToAgent(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	m = m.openSkillsBrowser()
	m = stepKey(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.mode != modeSkillsBrowser || m.skillsBrowser.query != "n" {
		t.Fatalf("a plain letter types into the search, never fires an action: mode=%v query=%q", m.mode, m.skillsBrowser.query)
	}
	m = stepKey(t, m, pressKey('n', tea.ModCtrl))
	if m.mode != modeInput || !strings.Contains(m.input.Value(), "skill_create") {
		t.Fatalf("ctrl+n closes and pre-fills a skill_create prompt: %q", m.input.Value())
	}
	m.input.Reset()
	m = m.openSkillsBrowser()
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "deploy" })
	m = stepKey(t, m, pressKey('e', tea.ModCtrl))
	if m.mode != modeInput || !strings.Contains(m.input.Value(), `skill "deploy"`) || !strings.Contains(m.input.Value(), "skill_edit") {
		t.Fatalf("ctrl+e pre-fills a skill_edit prompt: %q", m.input.Value())
	}
	m.input.Reset()
	if _, err := plugin.InstallPlugin(context.Background(), m.cwd, plugin.Ref{Plugin: "tools", Marketplace: "mkt"}, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}
	engine.BumpSkillsGeneration()
	m = m.openSkillsBrowser()
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "tools:xlsx" })
	mi, cmd := m.Update(pressKey('e', tea.ModCtrl))
	m = mi.(model)
	if m.mode != modeSkillsBrowser || cmd == nil {
		t.Fatal("ctrl+e on a plugin skill stays in the browser and toasts")
	}
	mi, _ = m.Update(pressSpecial(tea.KeyDelete))
	m = mi.(model)
	if ed := m.skillsBrowser.editor; ed == nil || ed.kind != skillsEditorConfirm || !strings.Contains(ed.title, "Uninstall") {
		t.Fatalf("del on a plugin skill offers to uninstall the plugin, not delete the file: %+v", ed)
	}
	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	// Ctrl+P/N are actions here, never list navigation.
	before := m.skillsBrowser.cursor
	m = stepKey(t, m, pressKey('p', tea.ModCtrl))
	if m.skillsBrowser.cursor != before || m.skillsBrowser.editor != nil {
		t.Fatal("ctrl+p on a plugin item must neither move the cursor nor open an editor")
	}
}

func TestSkillsBrowser_OverlayGeometryAndPaste(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	if m.skillsBrowserOverlay(100, 30) != "" {
		t.Fatal("no overlay while closed")
	}
	m = m.openSkillsBrowser()
	overlay := m.skillsBrowserOverlay(100, 30)
	if overlay == "" {
		t.Fatal("overlay must render")
	}
	w, h := lipgloss.Width(overlay), lipgloss.Height(overlay)
	boxW, boxH, _, _, _ := modelPickerGeometry(100, 30)
	if w != boxW || h != boxH {
		t.Errorf("overlay %dx%d, want the picker geometry %dx%d", w, h, boxW, boxH)
	}
	if w <= h {
		t.Errorf("overlay must be wide-and-flat: %dx%d", w, h)
	}
	if !strings.Contains(overlay, "┌") || !strings.Contains(overlay, "│") {
		t.Error("square corners and the divider must be present")
	}
	mi, _ := m.Update(tea.PasteMsg{Content: "dep"})
	m = mi.(model)
	if m.skillsBrowser.query != "dep" {
		t.Errorf("paste lands in the query: %q", m.skillsBrowser.query)
	}
	if _, ok := m.skillsBrowser.current(); !ok {
		t.Error("cursor stays on a selectable row after filtering")
	}
	m = stepKey(t, m, pressSpecial(tea.KeyPgDown))
	if m.skillsBrowser.detailScroll != modelPickerScrollStep {
		t.Errorf("PgDn scrolls the detail pane: %d", m.skillsBrowser.detailScroll)
	}
}

func TestSkillsBrowser_ImportFromClaude(t *testing.T) {
	m, _, mkt := skillsBrowserFixture(t)
	home, _ := os.UserHomeDir()
	claude := filepath.Join(home, "claude-home")
	orig := plugin.ClaudeHome
	plugin.ClaudeHome = func() string { return claude }
	t.Cleanup(func() { plugin.ClaudeHome = orig })
	other := filepath.Join(home, "claude-mkt")
	writeSkillsFixtureMarketplace(t, other, "fromclaude")
	if err := os.MkdirAll(filepath.Join(claude, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = mkt
	if err := os.WriteFile(filepath.Join(claude, "plugins", "known_marketplaces.json"), []byte(`{"fromclaude":{"source":{"source":"directory","path":"`+other+`"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte(`{"enabledPlugins":{"tools@fromclaude":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m = m.openSkillsBrowser()
	s := m.skillsBrowser
	s.setLens(skillsLensMarketplace)
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowAction && r.action == skillsActionImportClaude })
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if s.editor == nil || s.editor.kind != skillsEditorImportClaude || !strings.Contains(s.editor.help, "1 marketplace(s)") {
		t.Fatalf("import editor must summarise Claude Code's state: %+v", s.editor)
	}
	mi, cmd := m.Update(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	if _, ok := plugin.FindMarketplace(m.cwd, "fromclaude"); !ok {
		t.Fatal("import must register Claude Code's marketplace")
	}
	if _, ok := plugin.FindInstalled(m.cwd, plugin.Ref{Plugin: "tools", Marketplace: "fromclaude"}); !ok {
		t.Fatal("import must install Claude Code's enabled plugin")
	}
	if _, err := os.Stat(filepath.Join(claude, "plugins", "installed_plugins.json")); err == nil {
		t.Fatal("import must not write into ~/.claude")
	}
}

func TestWorkflowSupplant_RefusesUnconfiguredProvider(t *testing.T) {
	isolateHome(t)
	resetWorkflowTrackerForTest()
	a := newSidebarTestApp(t, 1)
	a.tabs[0].toast = NewToastModel(80, 0)
	msg := spawnWorkflowTabMsg{
		OriginTabID: a.tabs[0].id,
		Cwd:         a.tabs[0].cwd,
		Workflow:    workflowDef{Name: "pipeline", Steps: []workflowStep{{Name: "s1", Provider: "vertex"}}},
		Source:      chatWorkflowSource(a.tabs[0].id, nil),
	}
	m1, cmd := a.Update(msg)
	a = asApp(t, m1)
	if a.tabs[0].workflowRun != nil {
		t.Fatal("a step on an unconfigured provider must not start")
	}
	if cmd == nil {
		t.Fatal("the refusal must toast")
	}
	toasted := false
	for _, out := range drainBatch(t, cmd) {
		if ts, ok := out.(toastShowMsg); ok {
			toasted = true
			if !strings.Contains(ts.text, `provider "vertex" is not configured`) || !strings.Contains(ts.text, "change this step's model") {
				t.Errorf("toast = %q", ts.text)
			}
		}
	}
	if !toasted {
		t.Fatal("expected a toastShowMsg")
	}
}

func TestExtensionsChangedMsg_BroadcastsToEveryTab(t *testing.T) {
	a := testAppWithTwoTabs(t)
	probes := 0
	for _, tab := range a.tabs {
		tab.provider.(*fakeProvider).probeInitFn = func(ProviderSessionArgs) tea.Cmd {
			probes++
			return nil
		}
	}
	newA, cmd := a.Update(extensionsChangedMsg{what: "skill"})
	_ = newA.(app)
	if cmd != nil {
		drainBatch(t, cmd)
	}
	if probes != 2 {
		t.Fatalf("every tab must re-probe its slash commands: %d", probes)
	}
}

func TestSkillsBrowser_PublishLinkUpdateAndPull(t *testing.T) {
	m, _, mkt := skillsBrowserFixture(t)
	m = m.openSkillsBrowser()
	s := m.skillsBrowser
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "deploy" })
	m = stepKey(t, m, pressKey('p', tea.ModCtrl))
	mi, cmd := m.Update(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	s = m.skillsBrowser

	// The local item is linked and in sync; the marketplace row is "yours".
	by := skillsRowsByTitle(s.rows())
	item := by["skill:deploy"].item
	if item.pub == nil || item.pub.Ref().String() != "deploy@mkt" || item.sync != plugin.SyncInSync || item.publishTag() != "↑ mkt" {
		t.Fatalf("published item: pub=%+v sync=%v tag=%q", item.pub, item.sync, item.publishTag())
	}
	if pf := plugin.ReadProjectFile(m.cwd); pf.Published["skill:deploy"].Version != "1.0.0" {
		t.Fatalf("project-scope publication recorded in .ask/plugins.json: %+v", pf.Published)
	}
	s.setLens(skillsLensMarketplace)
	by = skillsRowsByTitle(s.rows())
	row, ok := by["plugin:deploy@mkt"]
	if !ok || !row.plugin.yours || row.plugin.installed {
		t.Fatalf("published plugin must read as yours, not installed: %+v", row.plugin)
	}
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowPlugin && r.plugin.ref == "deploy@mkt" })
	mi, cmd = m.Update(pressKey('g', tea.ModCtrl))
	m = mi.(model)
	if m.skillsBrowser.editor != nil || cmd == nil {
		t.Fatal("ctrl+g on your own plugin must toast instead of installing")
	}
	if got := plugin.EnabledPlugins(m.cwd); len(got) != 0 {
		t.Fatalf("publishing must not install the plugin locally: %+v", got)
	}

	// Local edit → "local changes"; Ctrl+P updates in place (no chooser),
	// bumping the version.
	s.setLens(skillsLensInstalled)
	body := "2. ship faster"
	if _, err := engine.UpdateSkill(m.cwd, "deploy", engine.OriginProject, engine.SkillPatch{Body: &body}); err != nil {
		t.Fatal(err)
	}
	s.rebuild(m.cwd)
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "deploy" })
	if it := skillsRowsByTitle(s.rows())["skill:deploy"].item; it.sync != plugin.SyncLocalChanged {
		t.Fatalf("want local changes, got %v", it.sync)
	}
	mi, cmd = m.Update(pressKey('p', tea.ModCtrl))
	m = mi.(model)
	if m.skillsBrowser.editor != nil {
		t.Fatal("updating a published item must not ask for a marketplace again")
	}
	m, _ = runSkillsOp(t, m, cmd)
	s = m.skillsBrowser
	it := skillsRowsByTitle(s.rows())["skill:deploy"].item
	if it.sync != plugin.SyncInSync || it.pub.Version != "1.0.1" {
		t.Fatalf("after update: sync=%v version=%s", it.sync, it.pub.Version)
	}
	data, _ := os.ReadFile(filepath.Join(mkt, "plugins", "deploy", "skills", "deploy", "SKILL.md"))
	if !strings.Contains(string(data), "ship faster") {
		t.Fatal("marketplace copy must carry the update")
	}
	mi, cmd = m.Update(pressKey('p', tea.ModCtrl))
	m = mi.(model)
	if m.skillsBrowser.editor != nil || cmd == nil {
		t.Fatal("ctrl+p while in sync just toasts")
	}

	// Marketplace copy edited elsewhere → "marketplace newer"; Ctrl+U pulls.
	if err := os.WriteFile(filepath.Join(mkt, "plugins", "deploy", "skills", "deploy", "SKILL.md"), []byte("---\nname: deploy\ndescription: Deploy the service\n---\nfrom the marketplace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.rebuild(m.cwd)
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "deploy" })
	if it := skillsRowsByTitle(s.rows())["skill:deploy"].item; it.sync != plugin.SyncMarketplaceChanged {
		t.Fatalf("want marketplace newer, got %v", it.sync)
	}
	m = stepKey(t, m, pressKey('u', tea.ModCtrl))
	if s.editor == nil || s.editor.kind != skillsEditorConfirm || !strings.Contains(s.editor.title, "Pull deploy") {
		t.Fatalf("ctrl+u asks to confirm the pull: %+v", s.editor)
	}
	mi, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	local, _ := os.ReadFile(filepath.Join(m.cwd, ".ask", "skills", "deploy", "SKILL.md"))
	if !strings.Contains(string(local), "from the marketplace") {
		t.Fatalf("pull must replace the local copy:\n%s", local)
	}
	if it := skillsRowsByTitle(m.skillsBrowser.rows())["skill:deploy"].item; it.sync != plugin.SyncInSync {
		t.Fatalf("after pull: %v", it.sync)
	}

	// Diverged: Ctrl+P asks before overwriting.
	body = "local again"
	if _, err := engine.UpdateSkill(m.cwd, "deploy", engine.OriginProject, engine.SkillPatch{Body: &body}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mkt, "plugins", "deploy", "skills", "deploy", "SKILL.md"), []byte("---\nname: deploy\ndescription: Deploy the service\n---\nremote again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.skillsBrowser.rebuild(m.cwd)
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.name == "deploy" })
	m = stepKey(t, m, pressKey('p', tea.ModCtrl))
	if ed := m.skillsBrowser.editor; ed == nil || ed.kind != skillsEditorConfirm || !strings.Contains(ed.title, "Overwrite") {
		t.Fatalf("diverged publish must confirm: %+v", ed)
	}

	// Workflows link too, without leaving temp exports behind.
	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowItem && r.item.kind == "workflow" })
	m = stepKey(t, m, pressKey('p', tea.ModCtrl))
	mi, cmd = m.Update(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	wf := skillsRowsByTitle(m.skillsBrowser.rows())["workflow:ship"].item
	if wf.pub == nil || wf.sync != plugin.SyncInSync || wf.pub.File != "ship.json" {
		t.Fatalf("workflow publication: pub=%+v sync=%v", wf.pub, wf.sync)
	}
}
