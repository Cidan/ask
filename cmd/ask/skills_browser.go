package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/plugin"
	"github.com/Cidan/ask/pkg/tools"
)

// /skills (Ctrl+S) opens the skills browser: the same centered two-pane
// overlay as the model picker, with two lenses switched by Tab.
//
//   - Installed: every skill, agent, and workflow this session can use,
//     grouped by origin (Project, User, then one group per enabled
//     plugin). Enter on a skill drops its slash command into the input.
//   - Marketplace: one group per registered marketplace listing its
//     plugins (✓ when enabled); Enter drills into a plugin's contents;
//     the tail rows add a marketplace, import Claude Code's state, or
//     create a new marketplace.
//
// Every mutation (install, uninstall, add/remove marketplace, publish,
// delete, import, refresh) runs as an async tea.Cmd that resolves with
// skillsBrowserOpDoneMsg; the browser rebuilds from disk and an
// extensionsChangedMsg re-registers slash commands on every tab.

type skillsLens int

const (
	skillsLensInstalled skillsLens = iota
	skillsLensMarketplace
)

type skillsItem struct {
	kind     string
	name     string
	desc     string
	origin   engine.Origin
	path     string
	group    string
	skill    *engine.Skill
	agent    *subagentDef
	workflow *workflowDef
	warnings []string
	// pub links the local copy to the plugin it was published as; sync
	// says how the two compare right now.
	pub  *plugin.Publication
	sync plugin.SyncState
}

// publishTag is the row suffix for a published item.
func (it skillsItem) publishTag() string {
	if it.pub == nil {
		return ""
	}
	tag := "↑ " + it.pub.Marketplace
	if it.sync != plugin.SyncInSync {
		tag += " (" + it.sync.String() + ")"
	}
	return tag
}

func (it skillsItem) editable() bool {
	return it.kind != "workflow" && it.origin.Editable()
}

func (it skillsItem) filterText() string {
	return it.kind + " " + it.name + " " + it.desc + " " + it.origin.String()
}

// skillsPluginRow is one catalog entry in the marketplace lens.
type skillsPluginRow struct {
	ref         string
	marketplace plugin.Marketplace
	entry       plugin.Entry
	installed   bool
	scopes      []string
	missing     bool
	dir         string
	// yours marks a plugin published from a local copy here — it reads
	// as installed, but installing it would duplicate the local item.
	yours bool
}

func (p skillsPluginRow) filterText() string {
	return p.ref + " " + p.entry.Description + " " + p.entry.Category + " " + strings.Join(p.entry.Skills, " ")
}

type skillsRowKind int

const (
	skillsRowHeader skillsRowKind = iota
	skillsRowItem
	skillsRowPlugin
	skillsRowAction
	skillsRowNote
	skillsRowBlank
	skillsRowMCP
)

type skillsAction int

const (
	skillsActionNone skillsAction = iota
	skillsActionAddMarketplace
	skillsActionImportClaude
	skillsActionNewMarketplace
)

type skillsRow struct {
	kind   skillsRowKind
	title  string
	note   string
	item   *skillsItem
	plugin *skillsPluginRow
	action skillsAction
	mcp    *mcpBrowserRow
}

func (r skillsRow) selectable() bool {
	return r.kind == skillsRowItem || r.kind == skillsRowPlugin || r.kind == skillsRowAction || r.kind == skillsRowMCP
}

// mcpBrowserRow is one configured MCP server shown in the Installed lens.
type mcpBrowserRow struct {
	name      string
	origin    mcpServerOrigin
	plugin    string // when origin == plugin
	transport string // stdio | http | sse
	target    string // command or URL summary
	disabled  bool   // effective
	oauth     bool   // an http/sse server eligible for OAuth
	url       string // for authorize / sign out
}

func (r mcpBrowserRow) filterText() string {
	return "mcp " + r.name + " " + r.origin.String() + " " + r.plugin + " " + r.transport + " " + r.target
}

func (r mcpBrowserRow) sourceLabel() string {
	if r.origin == mcpOriginPlugin {
		return "plugin:" + r.plugin
	}
	return r.origin.String()
}

type skillsEditorKind int

const (
	skillsEditorNone skillsEditorKind = iota
	skillsEditorAddMarketplace
	skillsEditorNewMarketplace
	skillsEditorScope
	skillsEditorConfirm
	skillsEditorPublish
	skillsEditorImportClaude
)

// skillsEditor is the inline prompt drawn in the detail pane: a text
// field (marketplace source / directory), a scope choice, a yes/no
// confirm, or a marketplace choice for publish.
type skillsEditor struct {
	kind    skillsEditorKind
	title   string
	help    string
	draft   string
	cursor  int
	options []string
	row     skillsRow
	// confirm / publish carry the operation to run on Enter.
	run func(m *model, choice int) tea.Cmd
}

type skillsBrowserState struct {
	cwd          string
	lens         skillsLens
	query        string
	cursor       int
	detailScroll int
	items        []skillsItem
	plugins      []plugin.Installed
	marketplaces []plugin.Marketplace
	catalog      []skillsPluginRow
	drill        *skillsPluginRow
	drillItems   []skillsItem
	editor       *skillsEditor
	// busy names the async operation in flight; keys that mutate are
	// refused until it lands.
	busy      string
	descCache map[string]string
	// mcpServers is every configured MCP server from all sources (shown as
	// a group in the Installed lens); mcpStatus carries the live
	// connection/auth state seeded from this tab's session.
	mcpServers []mcpBrowserRow
	mcpStatus  map[string]tools.MCPStatus
}

// extensionsChangedMsg says a skill, agent, plugin, or marketplace
// changed on disk. tabID 0 means "every tab".
type extensionsChangedMsg struct {
	what  string
	tabID int
}

// skillsBrowserOpDoneMsg resolves an async browser operation.
type skillsBrowserOpDoneMsg struct {
	tabID int
	note  string
	err   error
}

// mcpServersChangedMsg says the MCP server set / enable-state changed (a
// browser toggle or an authorization). Every tab reconciles its live MCP
// manager and an open browser rebuilds.
type mcpServersChangedMsg struct{ tabID int }

// mcpStatusChangedMsg says a session's MCP connection/auth state changed, so
// an open browser refreshes its status column.
type mcpStatusChangedMsg struct{ tabID int }

func (m model) openSkillsBrowser() model {
	(&m).clearSelection()
	s := &skillsBrowserState{cwd: m.cwd}
	s.rebuild(m.cwd)
	s.resetCursorToFirst()
	m.skillsBrowser = s
	(&m).seedBrowserMCPStatus()
	m.mode = modeSkillsBrowser
	return m
}

// currentAgentSession returns this tab's live agent session, or nil.
func (m model) currentAgentSession() *agentSession {
	if m.proc == nil {
		return nil
	}
	s, _ := m.proc.payload.(*agentSession)
	return s
}

// seedBrowserMCPStatus copies the live MCP statuses from this tab's session
// into the open browser so its detail pane reflects reality.
func (m *model) seedBrowserMCPStatus() {
	if m.skillsBrowser == nil {
		return
	}
	statuses := map[string]tools.MCPStatus{}
	if s := m.currentAgentSession(); s != nil && s.mcp != nil {
		for _, st := range s.mcp.Statuses() {
			statuses[st.Name] = st
		}
	}
	m.skillsBrowser.mcpStatus = statuses
}

func (m model) closeSkillsBrowser() model {
	m.mode = modeInput
	m.skillsBrowser = nil
	return m
}

// rebuild re-reads skills, agents, workflows, plugins, and marketplaces
// from disk, keeping the lens, query, and cursor.
func (s *skillsBrowserState) rebuild(cwd string) {
	s.cwd = cwd
	s.items = nil
	for _, sk := range discoverSkills(cwd) {
		sk := sk
		s.items = append(s.items, skillsItem{kind: "skill", name: sk.Name, desc: sk.Description, origin: sk.Origin, path: sk.Path, group: originGroup(sk.Origin), skill: &sk})
	}
	for _, ag := range discoverSubagents(cwd) {
		ag := ag
		s.items = append(s.items, skillsItem{kind: "agent", name: ag.Name, desc: ag.Description, origin: ag.Origin, path: ag.Source, group: originGroup(ag.Origin), agent: &ag})
	}
	for _, w := range listAllWorkflows(cwd) {
		w := w
		origin := workflowOrigin(w)
		s.items = append(s.items, skillsItem{kind: "workflow", name: w.Name, desc: w.Description, origin: origin, group: originGroup(origin), workflow: &w, warnings: workflowProviderWarnings(w)})
	}
	for i := range s.items {
		it := &s.items[i]
		if !it.editable() && it.kind != "workflow" {
			continue
		}
		if it.kind == "workflow" && it.origin.Scope == engine.OriginPlugin {
			continue
		}
		pub, ok := plugin.FindPublication(cwd, it.kind, it.name, "")
		if !ok {
			continue
		}
		it.pub = &pub
		it.sync = plugin.SyncMissing
		if target, err := tools.PublishTargetFor(cwd, it.kind, it.name); err == nil {
			it.sync = plugin.Status(cwd, pub, target.LocalHash)
		}
	}
	s.plugins = plugin.EnabledPlugins(cwd)
	s.marketplaces = plugin.ListMarketplaces(cwd)
	s.catalog = nil
	enabled := map[string]plugin.Installed{}
	for _, in := range s.plugins {
		enabled[in.Ref.String()] = in
	}
	published := map[string]bool{}
	for _, p := range plugin.Publications(cwd) {
		published[p.Ref().String()] = true
	}
	for _, mk := range s.marketplaces {
		if mk.Manifest == nil {
			continue
		}
		for _, e := range mk.Manifest.Plugins {
			row := skillsPluginRow{ref: e.Name + "@" + mk.Name, marketplace: mk, entry: e}
			if in, ok := enabled[row.ref]; ok {
				row.installed = true
				row.missing = in.Missing
				for _, sc := range in.Scopes {
					row.scopes = append(row.scopes, string(sc))
				}
				row.dir = in.Dir
			}
			if row.dir == "" {
				if d, ok := plugin.EntryLocalDir(mk, e); ok {
					row.dir = d
				}
			}
			row.yours = published[row.ref]
			s.catalog = append(s.catalog, row)
		}
	}
	if s.drill != nil {
		ref := s.drill.ref
		s.drill = nil
		for i := range s.catalog {
			if s.catalog[i].ref == ref {
				s.enterDrill(&s.catalog[i])
				break
			}
		}
	}
	s.mcpServers = buildMCPBrowserRows(cwd)
	if rows := s.rows(); s.cursor >= len(rows) {
		s.resetCursorToFirst()
	}
}

// buildMCPBrowserRows lists every configured MCP server from all sources for
// the Installed lens.
func buildMCPBrowserRows(cwd string) []mcpBrowserRow {
	cfg, err := loadConfig()
	if err != nil {
		return nil
	}
	var out []mcpBrowserRow
	for _, r := range listMCPServers(toPkgConfig(cfg), cwd) {
		row := mcpBrowserRow{
			name:      r.Name,
			origin:    r.Origin,
			plugin:    r.Plugin,
			transport: r.Config.EffectiveType(),
			disabled:  r.Disabled,
			url:       r.Config.URL,
		}
		if r.Config.Command != "" {
			row.target = r.Config.Command
		} else {
			row.target = r.Config.URL
		}
		row.oauth = row.transport != mcpServerTypeStdio
		out = append(out, row)
	}
	return out
}

func originGroup(o engine.Origin) string {
	switch o.Scope {
	case engine.OriginProject:
		return "Project"
	case engine.OriginUser:
		return "User"
	case engine.OriginPlugin:
		return o.Plugin
	}
	return string(o.Scope)
}

// workflowOrigin maps workflow scopes onto the browser's groups: repo
// is project-level, user and global are the user's, plugin is plugin.
func workflowOrigin(w workflowDef) engine.Origin {
	switch workflowScopeTag(w.Scope) {
	case workflowScopeRepo:
		return engine.Origin{Scope: engine.OriginProject}
	case workflowScopePlugin:
		return engine.Origin{Scope: engine.OriginPlugin, Plugin: w.Plugin}
	}
	return engine.Origin{Scope: engine.OriginUser}
}

func (s *skillsBrowserState) enterDrill(p *skillsPluginRow) {
	s.drill = p
	s.drillItems = nil
	if p.dir == "" {
		return
	}
	var man *plugin.PluginManifest
	man, _ = plugin.ReadPluginManifest(p.dir)
	entry := p.entry
	c := plugin.ResolveContents(p.dir, &entry, man)
	origin := engine.Origin{Scope: engine.OriginPlugin, Plugin: p.ref}
	prefix := p.entry.Name + ":"
	for _, d := range c.SkillDirs {
		fields, body, ok := engine.ParseMarkdownFrontmatter(filepath.Join(d, "SKILL.md"))
		name := filepath.Base(d)
		desc := ""
		if ok {
			if fields["name"] != "" {
				name = fields["name"]
			}
			desc = fields["description"]
		}
		_ = body
		s.drillItems = append(s.drillItems, skillsItem{kind: "skill", name: prefix + name, desc: desc, origin: origin, path: filepath.Join(d, "SKILL.md"), group: p.ref})
	}
	for _, f := range c.CommandFiles {
		fields, _, _ := engine.ParseMarkdownFrontmatter(f)
		s.drillItems = append(s.drillItems, skillsItem{kind: "skill", name: prefix + strings.TrimSuffix(filepath.Base(f), ".md"), desc: fields["description"], origin: origin, path: f, group: p.ref})
	}
	for _, f := range c.AgentFiles {
		fields, _, _ := engine.ParseMarkdownFrontmatter(f)
		name := fields["name"]
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(f), ".md")
		}
		s.drillItems = append(s.drillItems, skillsItem{kind: "agent", name: prefix + name, desc: fields["description"], origin: origin, path: f, group: p.ref})
	}
	for _, f := range c.WorkflowFiles {
		var w struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if data, err := os.ReadFile(f); err == nil {
			_ = json.Unmarshal(data, &w)
		}
		if w.Name == "" {
			w.Name = strings.TrimSuffix(filepath.Base(f), ".json")
		}
		s.drillItems = append(s.drillItems, skillsItem{kind: "workflow", name: w.Name, desc: w.Description, origin: origin, path: f, group: p.ref})
	}
}

func (s *skillsBrowserState) rows() []skillsRow {
	if s.lens == skillsLensMarketplace {
		if s.drill != nil {
			return s.drillRows()
		}
		return s.marketplaceRows()
	}
	return s.installedRows()
}

func (s *skillsBrowserState) installedRows() []skillsRow {
	groups := []string{"Project", "User"}
	pluginNotes := map[string]string{}
	for _, in := range s.plugins {
		groups = append(groups, in.Ref.String())
		var parts []string
		if in.Version != "" {
			parts = append(parts, in.Version)
		}
		for _, sc := range in.Scopes {
			parts = append(parts, string(sc))
		}
		if in.Missing {
			parts = append(parts, "not fetched")
		}
		pluginNotes[in.Ref.String()] = strings.Join(parts, " · ")
	}
	byGroup := map[string][]*skillsItem{}
	for i := range s.items {
		it := &s.items[i]
		if !modelPickerFuzzyMatch(s.query, it.filterText()) {
			continue
		}
		byGroup[it.group] = append(byGroup[it.group], it)
	}
	var rows []skillsRow
	for _, g := range groups {
		items := byGroup[g]
		isPlugin := g != "Project" && g != "User"
		if len(items) == 0 && (!isPlugin || s.query != "") {
			continue
		}
		if len(rows) > 0 {
			rows = append(rows, skillsRow{kind: skillsRowBlank})
		}
		rows = append(rows, skillsRow{kind: skillsRowHeader, title: g, note: pluginNotes[g]})
		if len(items) == 0 {
			rows = append(rows, skillsRow{kind: skillsRowNote, title: "(nothing installed from this plugin)"})
		}
		for _, it := range items {
			rows = append(rows, skillsRow{kind: skillsRowItem, item: it})
		}
	}
	var mcpRows []skillsRow
	for i := range s.mcpServers {
		mr := &s.mcpServers[i]
		if !modelPickerFuzzyMatch(s.query, mr.filterText()) {
			continue
		}
		mcpRows = append(mcpRows, skillsRow{kind: skillsRowMCP, mcp: mr})
	}
	if len(mcpRows) > 0 {
		if len(rows) > 0 {
			rows = append(rows, skillsRow{kind: skillsRowBlank})
		}
		rows = append(rows, skillsRow{kind: skillsRowHeader, title: "MCP Servers", note: fmt.Sprintf("%d", len(s.mcpServers))})
		rows = append(rows, mcpRows...)
	}
	if len(rows) == 0 && s.query == "" && len(s.mcpServers) == 0 {
		rows = append(rows, skillsRow{kind: skillsRowNote, title: "no skills, agents, or workflows yet — press n to create one, or tab for the marketplace"})
	}
	return rows
}

func (s *skillsBrowserState) marketplaceRows() []skillsRow {
	var rows []skillsRow
	for _, mk := range s.marketplaces {
		var matched []*skillsPluginRow
		for i := range s.catalog {
			p := &s.catalog[i]
			if p.marketplace.Name == mk.Name && modelPickerFuzzyMatch(s.query, p.filterText()) {
				matched = append(matched, p)
			}
		}
		if len(matched) == 0 && s.query != "" {
			continue
		}
		if len(rows) > 0 {
			rows = append(rows, skillsRow{kind: skillsRowBlank})
		}
		note := string(mk.Scope)
		if mk.Manifest != nil {
			note = fmt.Sprintf("%d plugins · %s", len(mk.Manifest.Plugins), mk.Scope)
		} else if mk.Err != "" {
			note = mk.Err
		}
		rows = append(rows, skillsRow{kind: skillsRowHeader, title: mk.Name, note: note})
		if len(matched) == 0 {
			rows = append(rows, skillsRow{kind: skillsRowNote, title: "(no plugins listed)"})
		}
		for _, p := range matched {
			rows = append(rows, skillsRow{kind: skillsRowPlugin, plugin: p})
		}
	}
	if s.query != "" {
		if len(rows) == 0 {
			rows = append(rows, skillsRow{kind: skillsRowNote, title: "(no matches)"})
		}
		return rows
	}
	if len(rows) > 0 {
		rows = append(rows, skillsRow{kind: skillsRowBlank})
	}
	rows = append(rows, skillsRow{kind: skillsRowHeader, title: "Marketplaces"})
	rows = append(rows, skillsRow{kind: skillsRowAction, action: skillsActionAddMarketplace, title: "+ Add marketplace"})
	rows = append(rows, skillsRow{kind: skillsRowAction, action: skillsActionImportClaude, title: "Import from Claude Code"})
	rows = append(rows, skillsRow{kind: skillsRowAction, action: skillsActionNewMarketplace, title: "+ New marketplace"})
	return rows
}

func (s *skillsBrowserState) drillRows() []skillsRow {
	p := s.drill
	note := "not installed"
	if p.installed {
		note = "installed · " + strings.Join(p.scopes, ", ")
	}
	rows := []skillsRow{{kind: skillsRowHeader, title: p.ref, note: note}}
	if p.dir == "" {
		rows = append(rows, skillsRow{kind: skillsRowNote, title: "(contents are fetched on install — press i)"})
		return rows
	}
	n := 0
	for i := range s.drillItems {
		it := &s.drillItems[i]
		if !modelPickerFuzzyMatch(s.query, it.filterText()) {
			continue
		}
		rows = append(rows, skillsRow{kind: skillsRowItem, item: it})
		n++
	}
	if n == 0 {
		rows = append(rows, skillsRow{kind: skillsRowNote, title: "(no skills, agents, or workflows)"})
	}
	return rows
}

func (s *skillsBrowserState) firstSelectable(rows []skillsRow) int {
	for i, r := range rows {
		if r.selectable() {
			return i
		}
	}
	return -1
}

func (s *skillsBrowserState) resetCursorToFirst() {
	s.cursor = max(s.firstSelectable(s.rows()), 0)
	s.detailScroll = 0
}

func (s *skillsBrowserState) moveCursor(rows []skillsRow, delta int) {
	var idxs []int
	pos := -1
	for i, r := range rows {
		if r.selectable() {
			if i == s.cursor {
				pos = len(idxs)
			}
			idxs = append(idxs, i)
		}
	}
	s.detailScroll = 0
	if len(idxs) == 0 {
		s.cursor = 0
		return
	}
	if pos < 0 {
		s.cursor = idxs[0]
		return
	}
	s.cursor = idxs[(pos+delta+len(idxs))%len(idxs)]
}

func (s *skillsBrowserState) current() (skillsRow, bool) {
	rows := s.rows()
	if s.cursor < 0 || s.cursor >= len(rows) || !rows[s.cursor].selectable() {
		return skillsRow{}, false
	}
	return rows[s.cursor], true
}

func (s *skillsBrowserState) setLens(l skillsLens) {
	if s.lens == l {
		return
	}
	s.lens = l
	s.drill = nil
	s.resetCursorToFirst()
}

// writableMarketplaces are the publish targets.
func (s *skillsBrowserState) writableMarketplaces() []plugin.Marketplace {
	var out []plugin.Marketplace
	for _, mk := range s.marketplaces {
		if mk.Writable() && mk.Fetched() {
			out = append(out, mk)
		}
	}
	return out
}

func (m model) updateSkillsBrowser(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.skillsBrowser
	if s == nil {
		return m.closeSkillsBrowser(), nil
	}
	if s.editor != nil {
		return m.updateSkillsBrowserEditor(msg)
	}
	rows := s.rows()
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		if s.drill != nil {
			ref := s.drill.ref
			s.drill = nil
			s.seedCursorToPlugin(ref)
			return m, nil
		}
		return m.closeSkillsBrowser(), nil
	case msg.Mod == 0 && msg.Code == tea.KeyTab:
		if s.lens == skillsLensInstalled {
			s.setLens(skillsLensMarketplace)
		} else {
			s.setLens(skillsLensInstalled)
		}
		return m, nil
	case msg.Mod == tea.ModCtrl && msg.Code == 'r':
		return m.startSkillsBrowserRefresh()
	// Arrows only: every plain letter types into the search, and the
	// Ctrl+letter set below is the action surface, so the Ctrl+P/N
	// list-nav aliases the other pickers accept are not offered here.
	case msg.Mod == 0 && msg.Code == tea.KeyUp:
		s.moveCursor(rows, -1)
		return m, nil
	case msg.Mod == 0 && msg.Code == tea.KeyDown:
		s.moveCursor(rows, +1)
		return m, nil
	case msg.Code == tea.KeyPgUp:
		s.detailScroll = max(s.detailScroll-modelPickerScrollStep, 0)
		return m, nil
	case msg.Code == tea.KeyPgDown:
		s.detailScroll += modelPickerScrollStep
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.skillsBrowserEnter()
	case msg.Code == tea.KeyBackspace:
		if s.query != "" {
			r := []rune(s.query)
			s.query = string(r[:len(r)-1])
			s.resetCursorToFirst()
		}
		return m, nil
	}
	// MCP rows own space (toggle), ^o (authorize), and ^d (sign out) before
	// the shared Ctrl+letter and type-to-search handling below.
	if row, ok := s.current(); ok && row.kind == skillsRowMCP {
		switch {
		case msg.Mod == 0 && msg.Text == " ":
			return m.toggleMCPServer(*row.mcp)
		case msg.Mod == tea.ModCtrl && msg.Code == 'o':
			return m.authorizeMCPRow(*row.mcp)
		case msg.Mod == tea.ModCtrl && msg.Code == 'd':
			return m.signOutMCPRow(*row.mcp)
		case msg.Mod == 0 && msg.Code == tea.KeyDelete:
			return m.signOutMCPRow(*row.mcp)
		}
	}
	// Actions are Ctrl+letter so type-to-search owns every plain key.
	// Ctrl+I (Tab) and Ctrl+M (Enter) are off limits. Unlike the other
	// overlays, Ctrl+D is delete here, not close-tab — Esc closes.
	if msg.Mod == tea.ModCtrl {
		switch msg.Code {
		case 'd':
			return m.skillsBrowserRemove()
		case 'g':
			if cmd, handled := m.skillsBrowserInstall(); handled {
				return m, cmd
			}
		case 'n':
			return m.skillsBrowserNew()
		case 'e':
			if nm, cmd, handled := m.skillsBrowserEdit(); handled {
				return nm, cmd
			}
		case 'p':
			if cmd, handled := m.skillsBrowserPublish(); handled {
				return m, cmd
			}
		case 'a':
			s.openEditor(skillsEditorAddMarketplace, skillsRow{})
			return m, nil
		case 'u':
			if cmd, handled := m.skillsBrowserPull(); handled {
				return m, cmd
			}
		case 'x':
			if cmd, handled := m.skillsBrowserRemoveMarketplace(); handled {
				return m, cmd
			}
		}
		return m, nil
	}
	if msg.Mod == 0 && msg.Code == tea.KeyDelete {
		return m.skillsBrowserRemove()
	}
	if configTextInputKey(msg) {
		s.query += msg.Text
		s.resetCursorToFirst()
		return m, nil
	}
	return m, nil
}

func (s *skillsBrowserState) seedCursorToPlugin(ref string) {
	rows := s.rows()
	for i, r := range rows {
		if r.kind == skillsRowPlugin && r.plugin.ref == ref {
			s.cursor = i
			s.detailScroll = 0
			return
		}
	}
	s.resetCursorToFirst()
}

func (m model) applySkillsBrowserPaste(text string) (tea.Model, tea.Cmd) {
	s := m.skillsBrowser
	if s == nil || text == "" {
		return m, nil
	}
	if s.editor != nil {
		if s.editor.kind == skillsEditorAddMarketplace || s.editor.kind == skillsEditorNewMarketplace {
			s.editor.draft += text
		}
		return m, nil
	}
	s.query += text
	s.resetCursorToFirst()
	return m, nil
}

// skillsBrowserEnter is the Enter action: insert a skill's slash command
// (installed lens), drill into a plugin, or open an action's editor.
func (m model) skillsBrowserEnter() (tea.Model, tea.Cmd) {
	s := m.skillsBrowser
	row, ok := s.current()
	if !ok {
		return m, nil
	}
	switch row.kind {
	case skillsRowItem:
		if s.lens == skillsLensMarketplace {
			// Inside a plugin's contents the primary action is installing it.
			if s.drill != nil && s.drill.installed {
				return m, m.toast.show(s.drill.ref + " is already installed")
			}
			if cmd, handled := m.skillsBrowserInstall(); handled {
				return m, cmd
			}
			return m, nil
		}
		it := row.item
		switch it.kind {
		case "skill":
			if it.skill == nil || !it.skill.UserInvocable {
				return m, m.toast.show("this skill is model-invoked only (user-invocable: false)")
			}
			m = m.closeSkillsBrowser()
			m.input.SetValue("/" + it.name + " ")
			return m, nil
		case "agent":
			m = m.closeSkillsBrowser()
			m.input.SetValue("Use the " + it.name + " agent to ")
			return m, nil
		case "workflow":
			return m, m.toast.show("run workflows with " + hintOr(keyHintFor(ActionChatWorkflow), "Ctrl+F") + " on a chat, or f on an issue")
		}
	case skillsRowPlugin:
		s.enterDrill(row.plugin)
		s.resetCursorToFirst()
		return m, nil
	case skillsRowAction:
		switch row.action {
		case skillsActionAddMarketplace:
			s.openEditor(skillsEditorAddMarketplace, row)
		case skillsActionImportClaude:
			s.openEditor(skillsEditorImportClaude, row)
		case skillsActionNewMarketplace:
			s.openEditor(skillsEditorNewMarketplace, row)
		}
		return m, nil
	case skillsRowMCP:
		return m.toggleMCPServer(*row.mcp)
	}
	return m, nil
}

// toggleMCPServer opens a user/project scope chooser, then writes an
// enable/disable override for the server at that scope and reconciles live
// sessions.
func (m model) toggleMCPServer(r mcpBrowserRow) (tea.Model, tea.Cmd) {
	s := m.skillsBrowser
	want := !r.disabled // desired new "disabled" value
	verb := "disable"
	if !want {
		verb = "enable"
	}
	s.editor = &skillsEditor{
		kind:    skillsEditorScope,
		title:   verb + " MCP server " + r.name,
		help:    "Apply for this user (all projects) or just this project (committed in .ask project config). Project overrides win over user.",
		options: []string{"user", "project"},
		run: func(m *model, choice int) tea.Cmd {
			project := choice == 1
			cwd := m.cwd
			name := r.name
			return m.startSkillsBrowserOp(verb+"ing "+name+"…", func() (string, error) {
				if err := setMCPDisabledOverride(cwd, name, want, project); err != nil {
					return "", err
				}
				scope := "user"
				if project {
					scope = "project"
				}
				return fmt.Sprintf("✓ %sd %s (%s scope)", verb, name, scope), nil
			})
		},
	}
	return m, nil
}

// authorizeMCPRow runs the interactive OAuth flow for an http/sse server.
func (m model) authorizeMCPRow(r mcpBrowserRow) (tea.Model, tea.Cmd) {
	if !r.oauth || r.url == "" {
		return m, m.toast.show(r.name + " is not an OAuth http/sse server")
	}
	srv := tools.MCPServer{Name: r.name, Cfg: tools.MCPServerConfig{Type: r.transport, URL: r.url, OAuth: true}}
	prompter := mcpAuthPrompter(m.id, r.name)
	return m, m.startSkillsBrowserOp("authorizing "+r.name+" — copy the link or check your browser…", func() (string, error) {
		if err := authorizeMCPServer(context.Background(), srv, prompter); err != nil {
			return "", err
		}
		return "✓ authorized " + r.name, nil
	})
}

// signOutMCPRow deletes the stored OAuth token for a server.
func (m model) signOutMCPRow(r mcpBrowserRow) (tea.Model, tea.Cmd) {
	if !r.oauth || r.url == "" {
		return m, nil
	}
	name, u := r.name, r.url
	return m, m.startSkillsBrowserOp("signing out "+name+"…", func() (string, error) {
		if err := forgetMCPServerAuth(u); err != nil {
			return "", err
		}
		return "✓ signed out " + name, nil
	})
}

// setMCPDisabledOverride writes cfg.MCPDisabled[name]=disabled at user scope,
// or ProjectConfig.MCPDisabled[name] at project scope.
func setMCPDisabledOverride(cwd, name string, disabled, project bool) error {
	if project {
		pc, err := config.LoadProject(cwd)
		if err != nil {
			return err
		}
		if pc.MCPDisabled == nil {
			pc.MCPDisabled = map[string]bool{}
		}
		pc.MCPDisabled[name] = disabled
		return config.SaveProject(cwd, pc)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.MCPDisabled == nil {
		cfg.MCPDisabled = map[string]bool{}
	}
	cfg.MCPDisabled[name] = disabled
	return config.Save(cfg)
}

func hintOr(hint, fallback string) string {
	if hint != "" {
		return hint
	}
	return fallback
}

func (s *skillsBrowserState) openEditor(kind skillsEditorKind, row skillsRow) {
	ed := &skillsEditor{kind: kind, row: row}
	switch kind {
	case skillsEditorAddMarketplace:
		ed.title = "Add marketplace"
		ed.help = "GitHub owner/repo, a git URL, a local directory, or a URL to a marketplace.json. Enter to continue, Esc to cancel."
	case skillsEditorNewMarketplace:
		ed.title = "New marketplace"
		ed.help = "Directory to create the marketplace in (it is git-initialised and registered as a local marketplace you can publish into). Enter to create, Esc to cancel."
		ed.draft = "~/ask-marketplace"
	case skillsEditorImportClaude:
		ed.title = "Import from Claude Code"
		st := plugin.ReadClaudeState(s.cwd)
		if st.Empty() {
			ed.help = "Nothing to import: Claude Code has no marketplaces or enabled plugins under ~/.claude."
		} else {
			ed.help = fmt.Sprintf("Registers Claude Code's %d marketplace(s) and installs its %d enabled plugin(s) into ask's own store (user scope). ~/.claude is not modified. Enter to import, Esc to cancel.", len(st.Marketplaces), len(st.EnabledRefs()))
			ed.run = func(m *model, _ int) tea.Cmd {
				cwd := m.cwd
				return m.startSkillsBrowserOp("importing from Claude Code…", func() (string, error) {
					rep := plugin.ImportFromClaude(context.Background(), cwd, plugin.ReadClaudeState(cwd), plugin.ScopeUser)
					return rep.Summary(), nil
				})
			}
		}
	}
	s.editor = ed
}

func (m model) updateSkillsBrowserEditor(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.skillsBrowser
	ed := s.editor
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		s.editor = nil
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.submitSkillsBrowserEditor()
	}
	switch ed.kind {
	case skillsEditorAddMarketplace, skillsEditorNewMarketplace:
		switch {
		case msg.Code == tea.KeyBackspace:
			if r := []rune(ed.draft); len(r) > 0 {
				ed.draft = string(r[:len(r)-1])
			}
		case configTextInputKey(msg):
			ed.draft += msg.Text
		}
	case skillsEditorScope, skillsEditorConfirm, skillsEditorPublish:
		switch {
		case listNavPrev(msg), msg.Code == tea.KeyLeft:
			ed.cursor = listNavWrap(ed.cursor, -1, len(ed.options))
		case listNavNext(msg), msg.Code == tea.KeyRight, msg.Mod == 0 && msg.Code == tea.KeyTab:
			ed.cursor = listNavWrap(ed.cursor, +1, len(ed.options))
		case msg.Mod == 0 && (msg.Text == "y" || msg.Text == "Y") && ed.kind == skillsEditorConfirm:
			ed.cursor = 0
			return m.submitSkillsBrowserEditor()
		case msg.Mod == 0 && (msg.Text == "n" || msg.Text == "N") && ed.kind == skillsEditorConfirm:
			s.editor = nil
		case msg.Mod == 0 && msg.Text == "u" && ed.kind == skillsEditorScope:
			ed.cursor = 0
			return m.submitSkillsBrowserEditor()
		case msg.Mod == 0 && msg.Text == "p" && ed.kind == skillsEditorScope:
			ed.cursor = 1
			return m.submitSkillsBrowserEditor()
		}
	}
	return m, nil
}

func (m model) submitSkillsBrowserEditor() (tea.Model, tea.Cmd) {
	s := m.skillsBrowser
	ed := s.editor
	switch ed.kind {
	case skillsEditorAddMarketplace:
		src := strings.TrimSpace(ed.draft)
		if src == "" {
			return m, nil
		}
		s.editor = &skillsEditor{
			kind:    skillsEditorScope,
			title:   "Add marketplace " + src,
			help:    "Register for this user only, or for this project (committed in .ask/plugins.json so the team gets it).",
			options: []string{"user", "project"},
			run: func(m *model, choice int) tea.Cmd {
				scope := plugin.ScopeUser
				if choice == 1 {
					scope = plugin.ScopeProject
				}
				cwd := m.cwd
				return m.startSkillsBrowserOp("adding marketplace "+src+"…", func() (string, error) {
					mk, err := plugin.AddMarketplace(context.Background(), cwd, src, scope)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("✓ added marketplace %s (%d plugins, %s scope)", mk.Name, len(mk.Manifest.Plugins), scope), nil
				})
			},
		}
		return m, nil
	case skillsEditorNewMarketplace:
		dir, _ := expandTilde(strings.TrimSpace(ed.draft))
		if dir == "" {
			return m, nil
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(m.cwd, dir)
		}
		name := plugin.ParseSourceString(filepath.Base(dir)).Path
		if err := plugin.ValidateName(name); err != nil {
			return m, m.toast.show("marketplace directory name must be kebab-case: " + err.Error())
		}
		cwd := m.cwd
		owner := os.Getenv("USER")
		if owner == "" {
			owner = "ask"
		}
		s.editor = nil
		return m, m.startSkillsBrowserOp("creating marketplace "+name+"…", func() (string, error) {
			if err := plugin.InitMarketplace(context.Background(), dir, name, owner); err != nil {
				return "", err
			}
			if _, err := plugin.AddMarketplace(context.Background(), cwd, dir, plugin.ScopeUser); err != nil {
				return "", err
			}
			return "✓ created marketplace " + name + " at " + dir + " — publish into it with p", nil
		})
	case skillsEditorScope, skillsEditorConfirm, skillsEditorPublish, skillsEditorImportClaude:
		if ed.run == nil {
			s.editor = nil
			return m, nil
		}
		run := ed.run
		choice := ed.cursor
		s.editor = nil
		return m, run(&m, choice)
	}
	s.editor = nil
	return m, nil
}

// startSkillsBrowserOp runs fn off the UI thread; the result lands as
// skillsBrowserOpDoneMsg on this tab.
func (m *model) startSkillsBrowserOp(label string, fn func() (string, error)) tea.Cmd {
	if m.skillsBrowser != nil {
		m.skillsBrowser.busy = label
	}
	tabID := m.id
	return func() tea.Msg {
		note, err := fn()
		return skillsBrowserOpDoneMsg{tabID: tabID, note: note, err: err}
	}
}

func (m model) finishSkillsBrowserOp(msg skillsBrowserOpDoneMsg) (tea.Model, tea.Cmd) {
	engine.BumpSkillsGeneration()
	if m.skillsBrowser != nil {
		m.skillsBrowser.busy = ""
		m.skillsBrowser.rebuild(m.cwd)
	}
	var cmds []tea.Cmd
	if msg.err != nil {
		cmds = append(cmds, m.toast.show("✗ "+msg.err.Error()))
	} else if msg.note != "" {
		cmds = append(cmds, m.toast.show(msg.note))
	}
	cmds = append(cmds, func() tea.Msg { return extensionsChangedMsg{what: "browser"} })
	cmds = append(cmds, func() tea.Msg { return mcpServersChangedMsg{} })
	return m, tea.Batch(cmds...)
}

func (m model) startSkillsBrowserRefresh() (tea.Model, tea.Cmd) {
	s := m.skillsBrowser
	if s.busy != "" {
		return m, nil
	}
	cwd := m.cwd
	return m, m.startSkillsBrowserOp("refreshing marketplaces…", func() (string, error) {
		ctx := context.Background()
		errs := plugin.RefreshAll(ctx, cwd)
		rep := plugin.SyncProject(ctx, cwd)
		errs = append(errs, rep.Errors...)
		if len(errs) > 0 {
			return "", fmt.Errorf("refresh: %v", errs[0])
		}
		note := "✓ marketplaces refreshed"
		if len(rep.Installed) > 0 {
			note += fmt.Sprintf(", %d plugin(s) fetched for this project", len(rep.Installed))
		}
		return note, nil
	})
}

func (m model) skillsBrowserBusyToast() tea.Cmd {
	return m.toast.show("busy: " + m.skillsBrowser.busy)
}

// skillsBrowserInstall handles i: install the selected marketplace
// plugin (or the drilled plugin) after a scope choice.
func (m model) skillsBrowserInstall() (tea.Cmd, bool) {
	s := m.skillsBrowser
	var p *skillsPluginRow
	if s.drill != nil {
		p = s.drill
	} else if row, ok := s.current(); ok && row.kind == skillsRowPlugin {
		p = row.plugin
	}
	if p == nil {
		return nil, false
	}
	if p.yours {
		return m.toast.show(p.ref + " is published from your local copy — edit it under Installed and ctrl+p to update"), true
	}
	if s.busy != "" {
		return m.skillsBrowserBusyToast(), true
	}
	ref := p.ref
	s.editor = &skillsEditor{
		kind:    skillsEditorScope,
		title:   "Install " + ref,
		help:    "Enable for this user only, or for this project (everyone on the repo gets it via .ask/plugins.json).",
		options: []string{"user", "project"},
		run: func(m *model, choice int) tea.Cmd {
			scope := plugin.ScopeUser
			if choice == 1 {
				scope = plugin.ScopeProject
			}
			cwd := m.cwd
			return m.startSkillsBrowserOp("installing "+ref+"…", func() (string, error) {
				r, err := plugin.ParseRef(ref)
				if err != nil {
					return "", err
				}
				in, err := plugin.InstallPlugin(context.Background(), cwd, r, scope)
				if err != nil {
					return "", err
				}
				c := in.Contents()
				return fmt.Sprintf("✓ installed %s (%s): %d skill(s), %d agent(s), %d workflow(s), %d MCP server(s)", ref, scope, len(c.SkillDirs)+len(c.CommandFiles), len(c.AgentFiles), len(c.WorkflowFiles), tools.PluginContentsMCPCount(c)), nil
			})
		},
	}
	return nil, true
}

// skillsBrowserRemove handles Delete / Ctrl+D: a user/project skill or
// agent is deleted; anything that comes from a plugin uninstalls the
// plugin instead.
func (m model) skillsBrowserRemove() (tea.Model, tea.Cmd) {
	s := m.skillsBrowser
	if row, ok := s.current(); ok && row.kind == skillsRowItem && row.item.origin.Scope != engine.OriginPlugin {
		if cmd, handled := m.skillsBrowserDelete(); handled {
			return m, cmd
		}
		return m, nil
	}
	if cmd, handled := m.skillsBrowserUninstall(); handled {
		return m, cmd
	}
	return m, nil
}

// skillsBrowserUninstall removes the plugin behind the selected row in
// each scope that enables it.
func (m model) skillsBrowserUninstall() (tea.Cmd, bool) {
	s := m.skillsBrowser
	ref := ""
	var scopes []string
	if s.drill != nil && s.drill.installed {
		ref, scopes = s.drill.ref, s.drill.scopes
	} else if row, ok := s.current(); ok {
		switch {
		case row.kind == skillsRowPlugin && row.plugin.installed:
			ref, scopes = row.plugin.ref, row.plugin.scopes
		case row.kind == skillsRowItem && row.item.origin.Scope == engine.OriginPlugin:
			ref = row.item.origin.Plugin
			for _, in := range s.plugins {
				if in.Ref.String() == ref {
					for _, sc := range in.Scopes {
						scopes = append(scopes, string(sc))
					}
				}
			}
		}
	}
	if ref == "" {
		return nil, false
	}
	if s.busy != "" {
		return m.skillsBrowserBusyToast(), true
	}
	if len(scopes) == 0 {
		scopes = []string{string(plugin.ScopeUser)}
	}
	s.editor = &skillsEditor{
		kind:    skillsEditorConfirm,
		title:   "Uninstall " + ref + "?",
		help:    "Removes it from " + strings.Join(scopes, " and ") + " scope and deletes the cached copy. y / n.",
		options: []string{"yes", "no"},
		run: func(m *model, choice int) tea.Cmd {
			if choice != 0 {
				return nil
			}
			cwd := m.cwd
			return m.startSkillsBrowserOp("uninstalling "+ref+"…", func() (string, error) {
				r, err := plugin.ParseRef(ref)
				if err != nil {
					return "", err
				}
				for _, sc := range scopes {
					if err := plugin.UninstallPlugin(cwd, r, plugin.Scope(sc)); err != nil {
						return "", err
					}
				}
				return "✓ uninstalled " + ref, nil
			})
		},
	}
	return nil, true
}

// skillsBrowserNew handles n: hand the creation to the agent with the
// extension tools, pre-filling the input so the user adds the intent.
func (m model) skillsBrowserNew() (tea.Model, tea.Cmd) {
	m = m.closeSkillsBrowser()
	m.input.SetValue("Create a skill from this conversation with skill_create (search_tools for it): ")
	return m, nil
}

// skillsBrowserEdit handles e: hand the edit to the agent with the
// extension tools, pre-filling the input with the target.
func (m model) skillsBrowserEdit() (model, tea.Cmd, bool) {
	s := m.skillsBrowser
	row, ok := s.current()
	if !ok || row.kind != skillsRowItem {
		return m, nil, false
	}
	it := row.item
	switch {
	case it.kind == "workflow":
		return m, m.toast.show("edit workflows in the builder (" + hintOr(keyHintFor(ActionScreenWorkflows), "/workflows") + ")"), true
	case !it.editable():
		return m, m.toast.show(it.name + " comes from " + it.origin.String() + " and is read-only — ctrl+n creates your own"), true
	}
	tool := "skill_edit"
	if it.kind == "agent" {
		tool = "agent_edit"
	}
	m = m.closeSkillsBrowser()
	m.input.SetValue(fmt.Sprintf("Update the %s %q with %s (search_tools for it): ", it.kind, it.name, tool))
	return m, nil, true
}

func (m model) skillsBrowserDelete() (tea.Cmd, bool) {
	s := m.skillsBrowser
	row, ok := s.current()
	if !ok || row.kind != skillsRowItem {
		return nil, false
	}
	it := row.item
	switch {
	case it.kind == "workflow":
		return m.toast.show("delete workflows in the builder (" + hintOr(keyHintFor(ActionScreenWorkflows), "/workflows") + ")"), true
	case !it.editable():
		return m.toast.show(it.name + " comes from " + it.origin.String() + " — ^d uninstalls the plugin"), true
	}
	if s.busy != "" {
		return m.skillsBrowserBusyToast(), true
	}
	name, kind, scope := it.name, it.kind, it.origin.Scope
	s.editor = &skillsEditor{
		kind:    skillsEditorConfirm,
		title:   fmt.Sprintf("Delete %s %s?", kind, name),
		help:    "Removes " + it.path + ". y / n.",
		options: []string{"yes", "no"},
		run: func(m *model, choice int) tea.Cmd {
			if choice != 0 {
				return nil
			}
			cwd := m.cwd
			return m.startSkillsBrowserOp("deleting "+name+"…", func() (string, error) {
				var err error
				if kind == "agent" {
					err = engine.DeleteAgent(cwd, name, scope)
				} else {
					err = engine.DeleteSkill(cwd, name, scope)
				}
				if err != nil {
					return "", err
				}
				return "✓ deleted " + kind + " " + name, nil
			})
		},
	}
	return nil, true
}

func (m model) skillsBrowserPublish() (tea.Cmd, bool) {
	s := m.skillsBrowser
	row, ok := s.current()
	if !ok || row.kind != skillsRowItem || s.lens != skillsLensInstalled {
		return nil, false
	}
	it := row.item
	if it.origin.Scope == engine.OriginPlugin {
		return m.toast.show(it.name + " already comes from a plugin"), true
	}
	if s.busy != "" {
		return m.skillsBrowserBusyToast(), true
	}
	name, kind := it.name, it.kind
	publishTo := func(m *model, mk plugin.Marketplace) tea.Cmd {
		cwd := m.cwd
		return m.startSkillsBrowserOp("publishing "+name+" to "+mk.Name+"…", func() (string, error) {
			target, err := tools.PublishTargetFor(cwd, kind, name)
			if err != nil {
				return "", err
			}
			res, pub, err := tools.PublishItem(context.Background(), cwd, mk, target, "", "", "", "", false)
			if err != nil {
				return "", err
			}
			status := "written"
			switch {
			case res.Pushed:
				status = "committed and pushed"
			case res.Committed:
				status = "committed"
			case res.Note != "":
				status = res.Note
			}
			return fmt.Sprintf("✓ published %s as %s v%s (%s)", name, pub.Ref(), res.Version, status), nil
		})
	}
	// Already published: this is an update to the same plugin.
	if it.pub != nil {
		mk, ok := plugin.FindMarketplace(m.cwd, it.pub.Marketplace)
		if !ok || !mk.Writable() {
			return m.toast.show(it.pub.Marketplace + " is not writable here — refresh or re-add it"), true
		}
		switch it.sync {
		case plugin.SyncInSync:
			return m.toast.show(name + " is already in sync with " + it.pub.Ref().String()), true
		case plugin.SyncMarketplaceChanged, plugin.SyncDiverged:
			s.editor = &skillsEditor{
				kind:    skillsEditorConfirm,
				title:   fmt.Sprintf("Overwrite %s in %s?", it.pub.Plugin, it.pub.Marketplace),
				help:    "The marketplace copy changed since you last published (" + it.sync.String() + "). Publishing replaces it with your local copy; ctrl+u pulls it instead. y / n.",
				options: []string{"yes", "no"},
				run: func(m *model, choice int) tea.Cmd {
					if choice != 0 {
						return nil
					}
					return publishTo(m, mk)
				},
			}
			return nil, true
		}
		return publishTo(&m, mk), true
	}
	targets := s.writableMarketplaces()
	if len(targets) == 0 {
		return m.toast.show("no writable marketplace — add a local or git-clone marketplace first (ctrl+a, or + New marketplace)"), true
	}
	names := make([]string, len(targets))
	for i, mk := range targets {
		names[i] = mk.Name
	}
	s.editor = &skillsEditor{
		kind:    skillsEditorPublish,
		title:   fmt.Sprintf("Publish %s %s to…", kind, name),
		help:    "Copies it into the marketplace as a plugin (git-backed marketplaces are committed and pushed). Your local copy stays the source of truth; ctrl+p again publishes updates. ←/→ choose, Enter publish.",
		options: names,
		run: func(m *model, choice int) tea.Cmd {
			if choice < 0 || choice >= len(targets) {
				return nil
			}
			return publishTo(m, targets[choice])
		},
	}
	return nil, true
}

// skillsBrowserPull handles Ctrl+U: replace the local copy of a
// published item with the marketplace copy.
func (m model) skillsBrowserPull() (tea.Cmd, bool) {
	s := m.skillsBrowser
	row, ok := s.current()
	if !ok || row.kind != skillsRowItem || s.lens != skillsLensInstalled {
		return nil, false
	}
	it := row.item
	if it.pub == nil {
		return m.toast.show(it.name + " was not published from here — nothing to pull"), true
	}
	if s.busy != "" {
		return m.skillsBrowserBusyToast(), true
	}
	if it.sync == plugin.SyncInSync {
		return m.toast.show(it.name + " is already in sync with " + it.pub.Ref().String()), true
	}
	name, kind, ref := it.name, it.kind, it.pub.Ref().String()
	s.editor = &skillsEditor{
		kind:    skillsEditorConfirm,
		title:   fmt.Sprintf("Pull %s from %s?", name, ref),
		help:    "Replaces your local copy with the marketplace copy (" + it.sync.String() + "). Local edits are lost. y / n.",
		options: []string{"yes", "no"},
		run: func(m *model, choice int) tea.Cmd {
			if choice != 0 {
				return nil
			}
			cwd := m.cwd
			return m.startSkillsBrowserOp("pulling "+name+"…", func() (string, error) {
				if _, err := tools.PullItem(cwd, kind, name); err != nil {
					return "", err
				}
				return "✓ pulled " + name + " from " + ref, nil
			})
		},
	}
	return nil, true
}

func (m model) skillsBrowserRemoveMarketplace() (tea.Cmd, bool) {
	s := m.skillsBrowser
	if s.lens != skillsLensMarketplace {
		return nil, false
	}
	var mk *plugin.Marketplace
	if s.drill != nil {
		mk = &s.drill.marketplace
	} else if row, ok := s.current(); ok && row.kind == skillsRowPlugin {
		mk = &row.plugin.marketplace
	}
	if mk == nil {
		return nil, false
	}
	if s.busy != "" {
		return m.skillsBrowserBusyToast(), true
	}
	name, scope := mk.Name, mk.Scope
	s.editor = &skillsEditor{
		kind:    skillsEditorConfirm,
		title:   "Remove marketplace " + name + "?",
		help:    fmt.Sprintf("Unregisters it from %s scope. Installed plugins stay installed. y / n.", scope),
		options: []string{"yes", "no"},
		run: func(m *model, choice int) tea.Cmd {
			if choice != 0 {
				return nil
			}
			cwd := m.cwd
			return m.startSkillsBrowserOp("removing marketplace "+name+"…", func() (string, error) {
				if err := plugin.RemoveMarketplace(cwd, name, scope); err != nil {
					return "", err
				}
				return "✓ removed marketplace " + name, nil
			})
		},
	}
	return nil, true
}

// handleSkillsCommand is the /skills slash command: bare opens the
// browser; "add marketplace <src>", "remove marketplace <name>",
// "import claude", and "refresh" run without the browser.
func (m model) handleSkillsCommand(args string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		if m.modalOpen() {
			return m, nil
		}
		return m.openSkillsBrowser(), nil
	}
	cwd := m.cwd
	switch {
	case len(fields) >= 3 && fields[0] == "add" && fields[1] == "marketplace":
		src := strings.Join(fields[2:], " ")
		scope := plugin.ScopeUser
		if n := len(fields); n >= 4 && (fields[n-1] == "--project" || fields[n-1] == "project") {
			scope = plugin.ScopeProject
			src = strings.Join(fields[2:n-1], " ")
		}
		return m, m.startSkillsBrowserOp("adding marketplace…", func() (string, error) {
			mk, err := plugin.AddMarketplace(context.Background(), cwd, src, scope)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("✓ added marketplace %s (%d plugins, %s scope)", mk.Name, len(mk.Manifest.Plugins), scope), nil
		})
	case len(fields) >= 3 && fields[0] == "remove" && fields[1] == "marketplace":
		name := fields[2]
		return m, m.startSkillsBrowserOp("removing marketplace…", func() (string, error) {
			mk, ok := plugin.FindMarketplace(cwd, name)
			if !ok {
				return "", fmt.Errorf("marketplace %q is not registered", name)
			}
			if err := plugin.RemoveMarketplace(cwd, name, mk.Scope); err != nil {
				return "", err
			}
			return "✓ removed marketplace " + name, nil
		})
	case len(fields) >= 2 && fields[0] == "import" && fields[1] == "claude":
		return m, m.startSkillsBrowserOp("importing from Claude Code…", func() (string, error) {
			rep := plugin.ImportFromClaude(context.Background(), cwd, plugin.ReadClaudeState(cwd), plugin.ScopeUser)
			return rep.Summary(), nil
		})
	case fields[0] == "refresh":
		return m.startSkillsBrowserRefresh()
	}
	m.appendHistory(outputStyle.Render(errStyle.Render(
		"usage: /skills · /skills add marketplace <owner/repo|url|dir> [project] · /skills remove marketplace <name> · /skills import claude · /skills refresh")))
	return m, nil
}

// sortedScopes renders a plugin's scopes for the detail pane.
func sortedScopes(in plugin.Installed) string {
	var out []string
	for _, sc := range in.Scopes {
		out = append(out, string(sc))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
