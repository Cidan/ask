package main

import (
	"slices"
	"strings"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
)

// configScreenState is the /config screen: sections down the left, the
// focused section's settings on the right, the focused setting's
// description beneath them. Rows are derived from the saved config on every
// frame (configSections); this holds only where the user is and what they
// are in the middle of.
type configScreenState struct {
	section int
	// cursor indexes the current page: the section's rows, or every
	// matching row while query filters.
	cursor int
	query  string

	// editing is the id of the text row whose inline editor is open.
	editing string
	input   textinput.Model
	// capturing is set while a keybinding row waits for its new key.
	capturing bool
	// choosing is the id of the select row whose list is open; choice is
	// the list's cursor and themeBackup the theme to restore on Esc.
	choosing    string
	choice      int
	themeBackup string

	// listing is the id of the list row whose items are open under it;
	// item is their cursor, where one past the last item is the add row.
	// While entering, input edits the focused item (a new one on the add
	// row), matches are its path completions and match their cursor.
	listing  string
	item     int
	entering bool
	matches  []pathMatch
	match    int

	// notice is the last action's outcome, shown on the filter line.
	notice    string
	noticeErr bool
}

// configPageRow is a row on the current page and the section it is from.
type configPageRow struct {
	row     configRow
	section int
}

func (m model) startConfigModal() model {
	(&m).clearSelection()
	m.mode = modeConfig
	m.config = &configScreenState{}
	return m
}

func (m model) clearConfigModal() model {
	m.mode = modeInput
	m.config = nil
	return m
}

// page is what the right pane lists: the current section's rows, or with a
// query, every row whose label, group, section, or description matches.
func (s *configScreenState) page(secs []configSection) []configPageRow {
	q := strings.ToLower(strings.TrimSpace(s.query))
	if q == "" {
		sec := secs[clampIndex(s.section, len(secs))]
		out := make([]configPageRow, len(sec.rows))
		for i, r := range sec.rows {
			out[i] = configPageRow{row: r, section: s.section}
		}
		return out
	}
	var out []configPageRow
	for si, sec := range secs {
		for _, r := range sec.rows {
			hay := strings.ToLower(r.label + " " + r.group + " " + sec.title + " " + r.desc + " " + r.value)
			if strings.Contains(hay, q) {
				out = append(out, configPageRow{row: r, section: si})
			}
		}
	}
	return out
}

func clampIndex(i, n int) int {
	if i >= n {
		i = n - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}

// current returns the focused row.
func (s *configScreenState) current(secs []configSection) (configPageRow, bool) {
	page := s.page(secs)
	if len(page) == 0 {
		return configPageRow{}, false
	}
	return page[clampIndex(s.cursor, len(page))], true
}

// rowByID finds a row anywhere on the screen.
func rowByID(secs []configSection, id string) (configRow, bool) {
	for _, sec := range secs {
		for _, r := range sec.rows {
			if r.id == id {
				return r, true
			}
		}
	}
	return configRow{}, false
}

// move steps the cursor by delta. Unfiltered, stepping past either end of
// a section flows into the neighbouring one.
func (s *configScreenState) move(secs []configSection, delta int) {
	if s.query != "" {
		n := len(s.page(secs))
		if n > 0 {
			s.cursor = listNavWrap(clampIndex(s.cursor, n), delta, n)
		}
		return
	}
	next := s.cursor + delta
	switch n := len(secs[s.section].rows); {
	case next >= n:
		s.section = (s.section + 1) % len(secs)
		s.cursor = 0
	case next < 0:
		s.section = (s.section - 1 + len(secs)) % len(secs)
		s.cursor = len(secs[s.section].rows) - 1
	default:
		s.cursor = next
	}
}

func (s *configScreenState) jumpSection(secs []configSection, delta int) {
	if s.query != "" {
		if cur, ok := s.current(secs); ok {
			s.section = cur.section
		}
		s.query = ""
	}
	s.section = (s.section + delta + len(secs)) % len(secs)
	s.cursor = 0
}

func (s *configScreenState) say(text string, isErr bool) {
	s.notice, s.noticeErr = text, isErr
}

func (m model) updateConfigModal(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.config
	if s == nil {
		return m.clearConfigModal(), nil
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	switch {
	case s.editing != "":
		return m.updateConfigEditor(msg)
	case s.capturing:
		return m.updateConfigCapture(msg)
	case s.choosing != "":
		return m.updateConfigChooser(msg)
	case s.listing != "":
		return m.updateConfigList(msg)
	}
	secs := m.configSections()
	s.section = clampIndex(s.section, len(secs))
	cur, hasRow := s.current(secs)
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c':
		return m.clearConfigModal(), nil
	case msg.Code == tea.KeyEsc:
		if s.query != "" {
			s.query, s.cursor = "", 0
			return m, nil
		}
		return m.clearConfigModal(), nil
	case msg.Code == tea.KeyTab && msg.Mod == tea.ModShift:
		s.jumpSection(secs, -1)
	case msg.Code == tea.KeyTab:
		s.jumpSection(secs, +1)
	case listNavPrev(msg):
		s.move(secs, -1)
	case listNavNext(msg):
		s.move(secs, +1)
	case msg.Code == tea.KeyPgUp:
		s.cursor = max(s.cursor-10, 0)
	case msg.Code == tea.KeyPgDown:
		s.cursor = clampIndex(s.cursor+10, len(s.page(secs)))
	case msg.Code == tea.KeyHome:
		s.cursor = 0
	case msg.Code == tea.KeyEnd:
		s.cursor = len(s.page(secs)) - 1
	case msg.Code == tea.KeyLeft && msg.Mod == 0:
		if hasRow {
			return m.configStep(cur.row, -1)
		}
	case msg.Code == tea.KeyRight && msg.Mod == 0:
		if hasRow {
			return m.configStep(cur.row, +1)
		}
	case msg.Code == tea.KeyEnter:
		if hasRow {
			return m.configActivate(cur.row)
		}
	case msg.Code == tea.KeyDelete:
		if hasRow {
			return m.configClear(cur.row)
		}
	case msg.Mod == tea.ModCtrl && msg.Code == 'r':
		if hasRow && cur.row.kind == configKey {
			return m.configResetKey(cur.row)
		}
	case msg.Code == tea.KeyBackspace:
		if s.query != "" {
			r := []rune(s.query)
			s.query, s.cursor = string(r[:len(r)-1]), 0
		}
	default:
		if configTextInputKey(msg) {
			s.query += msg.Text
			s.cursor = 0
		}
	}
	s.cursor = clampIndex(s.cursor, len(s.page(secs)))
	return m, nil
}

// configStep moves a toggle, choice, or select row one value in dir.
func (m model) configStep(row configRow, dir int) (tea.Model, tea.Cmd) {
	switch row.kind {
	case configToggle:
		return m.flipConfigToggle(row.id)
	case configChoice, configSelect:
		if row.id == "issueProvider" {
			return m.cycleIssueProviderDir(dir)
		}
		n := len(row.options)
		if n == 0 {
			return m, nil
		}
		return m.setConfigChoice(row, (row.current+dir+n)%n)
	}
	return m, nil
}

// configActivate is Enter on a row.
func (m model) configActivate(row configRow) (tea.Model, tea.Cmd) {
	s := m.config
	switch row.kind {
	case configToggle, configChoice:
		return m.configStep(row, +1)
	case configSelect:
		s.choosing, s.choice = row.id, row.current
		if row.id == "theme" {
			s.themeBackup = m.themeName
		}
		return m, nil
	case configText:
		return m.openConfigEditor(row), nil
	case configKey:
		s.capturing = true
		s.say("", false)
		return m, nil
	case configAction:
		return m.runConfigAction(row.id)
	case configList:
		s.listing, s.item, s.entering = row.id, 0, false
		s.say("", false)
		return m, nil
	}
	return m, nil
}

func (m model) runConfigAction(id string) (tea.Model, tea.Cmd) {
	switch id {
	case "deslopModel":
		return m.openDeslopModelPicker()
	case "memoryModel":
		return m.openMemoryModelPicker()
	case "memoryBrowse":
		m = m.clearConfigModal()
		return m.openMemoryBrowser()
	case "workflows":
		m = m.clearConfigModal()
		m = m.switchScreen(screenWorkflows)
		if m.workflowsBuilder == nil {
			m.workflowsBuilder = newWorkflowsBuilderState(m.cwd)
		} else {
			m.workflowsBuilder.refreshItems()
		}
		return m, nil
	}
	return m, nil
}

// saveConfigWith runs mutate on the saved config under the lock and
// returns what was saved.
func saveConfigWith(mutate func(*askConfig)) (askConfig, error) {
	var saved askConfig
	err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		mutate(&cfg)
		saved = cfg
		return saveConfig(cfg)
	})
	return saved, err
}

func (m model) configSaveFailed(err error) (tea.Model, tea.Cmd) {
	debugLog("config save: %v", err)
	m.config.say("✗ save failed: "+err.Error(), true)
	return m, nil
}

// flipConfigToggle flips one on/off setting and applies it to the tab.
func (m model) flipConfigToggle(id string) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var err error
	label := id
	switch id {
	case "quiet":
		m.quietMode = !m.quietMode
		v := m.quietMode
		_, err = saveConfigWith(func(c *askConfig) { c.UI.QuietMode = &v })
		(&m).refreshHistory()
		label = "Quiet mode " + onOff(v)
	case "cursorBlink":
		m.cursorBlink = !m.cursorBlink
		applyCursorBlink(&m.input, m.cursorBlink)
		v := m.cursorBlink
		_, err = saveConfigWith(func(c *askConfig) { c.UI.CursorBlink = &v })
		if v {
			cmd = cursor.Blink
		}
		label = "Cursor blink " + onOff(v)
	case "renderDiffs":
		m.renderDiffs = !m.renderDiffs
		v := m.renderDiffs
		_, err = saveConfigWith(func(c *askConfig) { c.UI.RenderDiffs = &v })
		(&m).refreshHistory()
		label = "Render diffs " + onOff(v)
	case "skipAllPermissions":
		m.skipAllPermissions = !m.skipAllPermissions
		v := m.skipAllPermissions
		_, err = saveConfigWith(func(c *askConfig) { c.UI.SkipAllPermissions = &v })
		m.killProc()
		label = "Skip all permissions " + onOff(v) + " — session restarts"
	case "worktree":
		var saved askConfig
		saved, err = saveConfigWith(func(c *askConfig) {
			v := c.UI.Worktree == nil || !*c.UI.Worktree
			c.UI.Worktree = &v
		})
		if err == nil {
			m = m.applyEffectiveWorktree(saved)
			label = "Worktree " + onOff(*saved.UI.Worktree)
		}
	case "autoCompact":
		var saved askConfig
		saved, err = saveConfigWith(func(c *askConfig) {
			v := c.AutoCompact != nil && !*c.AutoCompact
			c.AutoCompact = &v
		})
		label = "Auto-compact " + onOff(saved.AutoCompact == nil || *saved.AutoCompact)
	case "deslop":
		var saved askConfig
		saved, err = saveConfigWith(func(c *askConfig) {
			v := c.Deslop.Enabled == nil || !*c.Deslop.Enabled
			c.Deslop.Enabled = &v
		})
		label = "Deslop output " + onOff(saved.Deslop.Enabled != nil && *saved.Deslop.Enabled)
	case "memoryGate":
		var saved askConfig
		saved, err = saveConfigWith(func(c *askConfig) {
			v := !config.MemoryExtractGateEnabled(*c)
			c.Memory.ExtractGate = &v
		})
		label = "Skip empty turns " + onOff(config.MemoryExtractGateEnabled(saved))
	case "memoryFilter":
		var saved askConfig
		saved, err = saveConfigWith(func(c *askConfig) {
			v := !config.MemoryRecallFilterEnabled(*c)
			c.Memory.RecallFilter = &v
		})
		label = "Filter recalled memory " + onOff(config.MemoryRecallFilterEnabled(saved))
	default:
		return m, nil
	}
	if err != nil {
		return m.configSaveFailed(err)
	}
	if m.config != nil {
		m.config.say("✓ "+label, false)
	}
	return m, cmd
}

// setConfigChoice sets a choice or select row to its idx'th value.
func (m model) setConfigChoice(row configRow, idx int) (tea.Model, tea.Cmd) {
	if idx < 0 || idx >= len(row.options) {
		return m, nil
	}
	value := row.options[idx]
	if idx < len(row.values) {
		value = row.values[idx]
	}
	label := row.label + " → " + row.options[idx]
	var err error
	switch row.id {
	case "theme":
		m = m.previewTheme(idx)
		theme := m.themeName
		_, err = saveConfigWith(func(c *askConfig) { c.UI.Theme = theme })
		(&m).refreshHistory()
	case "toolOutput":
		m.toolOutputMode = toolOutputMode(value)
		_, err = saveConfigWith(func(c *askConfig) { c.UI.ToolOutput = value })
		(&m).refreshHistory()
	case "provider":
		_, err = saveConfigWith(func(c *askConfig) { c.Provider = value })
		label += " (applies to new tabs)"
	case "decisionModel":
		p, mid, _ := strings.Cut(value, "/")
		_, err = saveConfigWith(func(c *askConfig) { c.Decisions = config.DecisionsConfig{Provider: p, Model: mid} })
	case "githubAuth":
		_, err = saveConfigWith(func(c *askConfig) {
			pc := loadProjectConfig(*c, m.cwd)
			pc.MCP.GitHub.Auth = value
			*c = upsertProjectConfig(*c, m.cwd, pc)
		})
		m.killProc()
		label += " — session restarts"
	case "projectWorktree":
		var saved askConfig
		saved, err = saveConfigWith(func(c *askConfig) {
			pc := loadProjectConfig(*c, m.cwd)
			switch value {
			case "on":
				v := true
				pc.Worktree = &v
			case "off":
				v := false
				pc.Worktree = &v
			default:
				pc.Worktree = nil
			}
			*c = upsertProjectConfig(*c, m.cwd, pc)
		})
		if err == nil {
			m = m.applyEffectiveWorktree(saved)
		}
	default:
		return m, nil
	}
	if err != nil {
		return m.configSaveFailed(err)
	}
	if m.config != nil {
		m.config.say("✓ "+label, false)
	}
	return m, nil
}

// cycleIssueProviderDir moves the project's issue provider to the next
// entry in dir whose credentials are set ("none" always is), persists it,
// and reports which providers it skipped.
func (m model) cycleIssueProviderDir(dir int) (tea.Model, tea.Cmd) {
	var next IssueProvider
	var skipped []string
	_, err := saveConfigWith(func(c *askConfig) {
		pc := loadProjectConfig(*c, m.cwd)
		cur := 0
		for i, p := range issueProviderRegistry {
			if p.ID() == pc.Issues.Provider {
				cur = i
				break
			}
		}
		n := len(issueProviderRegistry)
		for step := 1; step <= n; step++ {
			cand := issueProviderRegistry[((cur+dir*step)%n+n)%n]
			if !providerActivationReady(cand, pc) {
				skipped = append(skipped, cand.DisplayName())
				continue
			}
			next = cand
			break
		}
		pc.Issues.Provider = next.ID()
		*c = upsertProjectConfig(*c, m.cwd, pc)
	})
	if err != nil {
		return m.configSaveFailed(err)
	}
	text := "✓ Issue provider → " + next.DisplayName()
	if len(skipped) > 0 {
		text += " (skipped, no credentials: " + strings.Join(skipped, ", ") + ")"
	}
	if m.config != nil {
		m.config.say(text, false)
	}
	return m, nil
}

func (m model) openConfigEditor(row configRow) model {
	s := m.config
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = row.field.Hint
	if row.field.Secret {
		ti.EchoMode = textinput.EchoPassword
	}
	ti.SetValue(row.stored)
	ti.CursorEnd()
	ti.SetWidth(max(configValueWidth(m.width)-2, 10))
	ti.Focus()
	s.editing, s.input = row.id, ti
	s.say("", false)
	return m
}

func (m model) updateConfigEditor(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.config
	switch {
	case msg.Code == tea.KeyEsc, msg.Mod == tea.ModCtrl && msg.Code == 'c':
		s.editing = ""
		return m, nil
	case msg.Code == tea.KeyEnter:
		row, ok := rowByID(m.configSections(), s.editing)
		if !ok {
			s.editing = ""
			return m, nil
		}
		return m.commitConfigText(row, s.input.Value())
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	return m, cmd
}

// applyConfigPaste routes a paste into the open editor, or the filter.
func (m model) applyConfigPaste(text string) (tea.Model, tea.Cmd) {
	s := m.config
	if s == nil {
		return m, nil
	}
	text = strings.TrimRight(text, "\r\n")
	if s.editing != "" {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(tea.PasteMsg{Content: text})
		return m, cmd
	}
	if s.listing != "" {
		if !s.entering {
			return m, nil
		}
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(tea.PasteMsg{Content: text})
		if row, ok := rowByID(m.configSections(), s.listing); ok {
			s.refreshListMatches(row)
		}
		return m, cmd
	}
	if !strings.ContainsAny(text, "\n\r") {
		s.query += text
		s.cursor = 0
	}
	return m, nil
}

// commitConfigText validates and saves a text row's new value. A failed
// validation keeps the editor open on the draft.
func (m model) commitConfigText(row configRow, draft string) (tea.Model, tea.Cmd) {
	s := m.config
	draft = strings.TrimSpace(draft)
	if row.field.Validate != nil {
		if err := row.field.Validate(draft); err != nil {
			s.say("✗ "+row.label+": "+err.Error(), true)
			return m, nil
		}
	}
	if _, err := saveConfigWith(func(c *askConfig) { row.save(c, draft) }); err != nil {
		s.editing = ""
		return m.configSaveFailed(err)
	}
	s.editing = ""
	if row.onSaved != nil {
		m = row.onSaved(m)
	}
	verb := "saved"
	if draft == "" {
		verb = "cleared"
	}
	s.say("✓ "+row.tag+": "+row.label+" "+verb, false)
	return m, nil
}

// configClear is Delete: a text row's stored value is removed (falling back
// to its env or default), a keybinding is unbound.
func (m model) configClear(row configRow) (tea.Model, tea.Cmd) {
	switch row.kind {
	case configText:
		if row.stored == "" {
			return m, nil
		}
		return m.commitConfigText(row, "")
	case configKey:
		if err := persistKeyBinding(row.action, KeyBinding{}); err != nil {
			return m.configSaveFailed(err)
		}
		invalidateKeyMapCache()
		m.config.say("✓ "+row.label+" unbound", false)
	}
	return m, nil
}

func (m model) configResetKey(row configRow) (tea.Model, tea.Cmd) {
	def := defaultKeyBindings[row.action]
	if err := persistKeyBinding(row.action, def); err != nil {
		return m.configSaveFailed(err)
	}
	invalidateKeyMapCache()
	m.config.say("✓ "+row.label+" → "+displayBinding(def)+" (default)", false)
	return m, nil
}

// updateConfigCapture records the next key as the focused row's binding.
// Esc and Ctrl+C cancel; every other key, Enter and Tab included, is a
// valid binding.
func (m model) updateConfigCapture(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.config
	if msg.Code == tea.KeyEsc || (msg.Mod == tea.ModCtrl && msg.Code == 'c') {
		s.capturing = false
		return m, nil
	}
	cur, ok := s.current(m.configSections())
	if !ok || cur.row.kind != configKey {
		s.capturing = false
		return m, nil
	}
	binding := KeyBinding{Mod: stripKeyLockModifiers(msg.Mod), Code: msg.Code}
	if binding.String() == "" {
		s.say("✗ could not record that key — try a different combination", true)
		return m, nil
	}
	if err := persistKeyBinding(cur.row.action, binding); err != nil {
		s.capturing = false
		return m.configSaveFailed(err)
	}
	invalidateKeyMapCache()
	s.capturing = false
	s.say("✓ "+cur.row.label+" → "+binding.String(), false)
	return m, nil
}

// updateConfigChooser drives an open select list. The theme list previews
// each theme as the cursor lands on it; Esc puts the old one back.
func (m model) updateConfigChooser(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.config
	row, ok := rowByID(m.configSections(), s.choosing)
	if !ok {
		s.choosing = ""
		return m, nil
	}
	n := len(row.options)
	switch {
	case msg.Code == tea.KeyEsc, msg.Mod == tea.ModCtrl && msg.Code == 'c':
		if row.id == "theme" {
			m = m.previewTheme(themeIndexByName(s.themeBackup))
		}
		s.choosing = ""
		return m, nil
	case listNavPrev(msg):
		s.choice = listNavWrap(s.choice, -1, n)
	case listNavNext(msg):
		s.choice = listNavWrap(s.choice, +1, n)
	case msg.Code == tea.KeyHome:
		s.choice = 0
	case msg.Code == tea.KeyEnd:
		s.choice = n - 1
	case msg.Code == tea.KeyEnter:
		s.choosing = ""
		return m.setConfigChoice(row, s.choice)
	default:
		return m, nil
	}
	if row.id == "theme" {
		m = m.previewTheme(s.choice)
	}
	return m, nil
}

// updateConfigList drives an open list row: ↑↓ walk its items and the add
// row, Enter edits the focused item or starts a new one, Delete removes
// the focused item, Esc closes the list.
func (m model) updateConfigList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.config
	row, ok := rowByID(m.configSections(), s.listing)
	if !ok {
		s.listing, s.entering, s.matches = "", false, nil
		return m, nil
	}
	if s.entering {
		return m.updateConfigListEntry(row, msg)
	}
	n := len(row.items) + 1
	s.item = clampIndex(s.item, n)
	switch {
	case msg.Code == tea.KeyEsc, msg.Mod == tea.ModCtrl && msg.Code == 'c':
		s.listing = ""
	case listNavPrev(msg):
		s.item = listNavWrap(s.item, -1, n)
	case listNavNext(msg):
		s.item = listNavWrap(s.item, +1, n)
	case msg.Code == tea.KeyHome:
		s.item = 0
	case msg.Code == tea.KeyEnd:
		s.item = n - 1
	case msg.Code == tea.KeyEnter:
		value := ""
		if s.item < len(row.items) {
			value = row.items[s.item]
		}
		return m.openConfigListEntry(row, value), nil
	case msg.Code == tea.KeyDelete:
		if s.item < len(row.items) {
			removed := row.items[s.item]
			return m.saveConfigList(row, slices.Delete(slices.Clone(row.items), s.item, s.item+1), "removed "+clipPathLeft(removed, 28))
		}
	}
	return m, nil
}

func (m model) openConfigListEntry(row configRow, value string) model {
	s := m.config
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "type a path · tab completes"
	ti.SetValue(value)
	ti.CursorEnd()
	ti.SetWidth(max(configValueWidth(m.width)-4, 10))
	ti.Focus()
	s.input, s.entering = ti, true
	s.refreshListMatches(row)
	s.say("", false)
	return m
}

func (s *configScreenState) refreshListMatches(row configRow) {
	s.matches, s.match = nil, 0
	if row.complete != nil {
		s.matches = row.complete(s.input.Value())
	}
}

// updateConfigListEntry drives the input of an item being entered: Tab
// takes the highlighted completion (a directory keeps a trailing slash to
// go on into it), ↑↓ move the highlight, Enter stores what is typed.
func (m model) updateConfigListEntry(row configRow, msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.config
	switch {
	case msg.Code == tea.KeyEsc, msg.Mod == tea.ModCtrl && msg.Code == 'c':
		s.entering, s.matches = false, nil
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.commitConfigListEntry(row, strings.TrimSpace(s.input.Value()))
	case msg.Code == tea.KeyTab:
		if len(s.matches) > 0 {
			pick := s.matches[clampIndex(s.match, len(s.matches))]
			value := pick.path
			if pick.dir {
				value += "/"
			}
			s.input.SetValue(value)
			s.input.CursorEnd()
			s.refreshListMatches(row)
		}
		return m, nil
	case listNavPrev(msg):
		if len(s.matches) > 0 {
			s.match = listNavWrap(s.match, -1, len(s.matches))
		}
		return m, nil
	case listNavNext(msg):
		if len(s.matches) > 0 {
			s.match = listNavWrap(s.match, +1, len(s.matches))
		}
		return m, nil
	}
	before := s.input.Value()
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	if s.input.Value() != before {
		s.refreshListMatches(row)
	}
	return m, cmd
}

// commitConfigListEntry stores an entered item in place of the focused one,
// or after the rest from the add row. An empty or unchanged entry just
// closes the input; a refused or duplicate one keeps it open.
func (m model) commitConfigListEntry(row configRow, value string) (tea.Model, tea.Cmd) {
	s := m.config
	editing := s.item < len(row.items)
	if value == "" || (editing && value == row.items[s.item]) {
		s.entering, s.matches = false, nil
		return m, nil
	}
	if row.accept != nil {
		accepted, err := row.accept(value)
		if err != nil {
			s.say("✗ "+row.label+": "+err.Error(), true)
			return m, nil
		}
		value = accepted
	}
	if i := slices.Index(row.items, value); i >= 0 && !(editing && i == s.item) {
		s.say("✗ "+row.label+": "+value+" is already in the list", true)
		return m, nil
	}
	items := slices.Clone(row.items)
	verb := "updated "
	if editing {
		items[s.item] = value
	} else {
		items = append(items, value)
		verb = "added "
	}
	s.entering, s.matches = false, nil
	return m.saveConfigList(row, items, verb+clipPathLeft(value, 28))
}

func (m model) saveConfigList(row configRow, items []string, what string) (tea.Model, tea.Cmd) {
	if len(items) == 0 {
		items = nil
	}
	if _, err := saveConfigWith(func(c *askConfig) { row.saveList(c, items) }); err != nil {
		return m.configSaveFailed(err)
	}
	if row.onSaved != nil {
		m = row.onSaved(m)
	}
	m.config.say("✓ "+row.tag+": "+row.label+" "+what, false)
	return m, nil
}

func configTextInputKey(msg tea.KeyPressMsg) bool {
	return msg.Text != "" && stripKeyLockModifiers(msg.Mod)&^tea.ModShift == 0
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// applyEffectiveWorktree re-derives the tab's worktree flag from the
// saved config after the global or project toggle moved. A change
// that leaves the effective value alone (a global flip shadowed by a
// project override) must not restart the session.
func (m model) applyEffectiveWorktree(cfg askConfig) model {
	next := worktreeEnabled(cfg, m.cwd)
	if next == m.worktree {
		return m
	}
	m.worktree = next
	if next {
		ensureWorktreeGitignore()
	} else {
		m.worktreeName = ""
	}
	m.killProc()
	return m
}

// invalidateThemedRender drops every entry's cached glamour/wrap output
// so the next layout pass re-renders with the freshly-applied theme.
// Every entry is touched up front because a theme change must affect the
// visible window immediately; the glamour cost is still paid lazily, only
// for entries the chat view wraps. m.renderer is dropped because glamour
// bakes the theme into its style at construction.
func (m *model) invalidateThemedRender() {
	for i := range m.history {
		switch m.history[i].kind {
		case histResponse, histUser, histWorkflowDone:
			invalidateEntryRender(&m.history[i])
		}
	}
	m.renderer = nil
	m.rendererWidth = 0
	m.lastContentFP = ""
	m.fc = &frameCache{}
}

func (m model) previewTheme(idx int) model {
	if idx < 0 || idx >= len(themeRegistry) {
		return m
	}
	t := themeRegistry[idx]
	m.themeName = t.name
	applyTheme(t)
	(&m).invalidateThemedRender()
	return m
}

func themeIndexByName(name string) int {
	for i, t := range themeRegistry {
		if t.name == name {
			return i
		}
	}
	return 0
}
