package main

import (
	"errors"
	"os"
	"sort"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	lipgloss "charm.land/lipgloss/v2"
)

// Ctrl+M opens the unified model picker (crush-style): a search input
// on top, a "Recently used" group first, then one section per
// registered provider listing its models dynamically queried from the API.

type providerKeySpec struct {
	id     string
	title  string
	envKey string
	config func(*askConfig) *apiProviderConfig
}

var providerKeySpecs = []providerKeySpec{}

func providerKeySpecByID(id string) (providerKeySpec, bool) {
	for _, s := range providerKeySpecs {
		if s.id == id {
			return s, true
		}
	}
	return providerKeySpec{}, false
}

func missingAPIKeyError(envKey string) error {
	picker := "the model picker"
	if k := keyHintFor(ActionProviderSwitch); k != "" {
		picker += " (" + k + ")"
	}
	return errors.New("no API key configured — add one via " + picker + ", or export " + envKey)
}

func providerNeedsAPIKey(cfg askConfig, providerID string) bool {
	spec, ok := providerKeySpecByID(providerID)
	if !ok {
		return false
	}
	if spec.envKey != "" && os.Getenv(spec.envKey) != "" {
		return false
	}
	if spec.config == nil {
		return false
	}
	conf := spec.config(&cfg)
	if conf == nil {
		return false
	}
	return conf.APIKey == ""
}

var catwalkProviderIDs = map[string]catwalk.InferenceProvider{
	vertexProviderID: catwalk.InferenceProviderVertexAI,
}

func friendlyModelName(providerID, modelID string) string {
	if cw, ok := catwalkProviderIDs[providerID]; ok {
		if mdl, ok := catalogModel(cw, modelID); ok && mdl.Name != "" {
			return strings.ReplaceAll(mdl.Name, "-", " ")
		}
	}
	return humanizeModelID(modelID)
}

func humanizeModelID(id string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		r := []rune(w)
		if len(r) > 0 && unicode.IsLetter(r[0]) {
			r[0] = unicode.ToUpper(r[0])
		}
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

const modelPickerCustomRowLabel = "Enter your own…"

type modelPickerEntry struct {
	providerID   string
	providerName string
	modelID      string
	display      string
}

type modelPickerGroup struct {
	id          string
	name        string
	entries     []modelPickerEntry
	allowCustom bool
	needsKey    bool
}

type modelPickerRowKind int

const (
	modelPickerRowHeader modelPickerRowKind = iota
	modelPickerRowEntry
	modelPickerRowCustom
	modelPickerRowBlank
)

type modelPickerRow struct {
	kind   modelPickerRowKind
	title  string
	note   string
	entry  modelPickerEntry
	recent bool
}

type modelPickerKeyEntry struct {
	pending modelPickerEntry
	spec    providerKeySpec
	draft   string
}

type modelPickerCustomEntry struct {
	providerID   string
	providerName string
	draft        string
}

type modelPickerState struct {
	query       string
	cursor      int
	groups      []modelPickerGroup
	keyEntry    *modelPickerKeyEntry
	customEntry *modelPickerCustomEntry
}

func buildModelPickerState(cfg askConfig) *modelPickerState {
	s := &modelPickerState{}

	providerGroups := make([]modelPickerGroup, 0, len(providerRegistry))
	for _, p := range providerRegistry {
		picker := p.ModelPicker()
		g := modelPickerGroup{
			id:          p.ID(),
			name:        p.DisplayName(),
			allowCustom: picker.AllowCustom,
			needsKey:    providerNeedsAPIKey(cfg, p.ID()),
		}
		for _, id := range picker.Options {
			g.entries = append(g.entries, modelPickerEntry{
				providerID:   p.ID(),
				providerName: p.DisplayName(),
				modelID:      id,
				display:      friendlyModelName(p.ID(), id),
			})
		}
		providerGroups = append(providerGroups, g)
	}

	var recent modelPickerGroup
	recent.name = "Recently used"
	for _, ref := range cfg.RecentModels {
		var prov *modelPickerGroup
		for i := range providerGroups {
			if providerGroups[i].id == ref.Provider {
				prov = &providerGroups[i]
				break
			}
		}
		if prov == nil || ref.Model == "" {
			continue
		}
		entry := modelPickerEntry{
			providerID:   prov.id,
			providerName: prov.name,
			modelID:      ref.Model,
			display:      friendlyModelName(prov.id, ref.Model),
		}
		recent.entries = append(recent.entries, entry)
	}
	if len(recent.entries) > 0 {
		s.groups = append(s.groups, recent)
	}
	s.groups = append(s.groups, providerGroups...)
	return s
}

func modelPickerFuzzyMatch(query, target string) bool {
	q := []rune(strings.ToLower(strings.ReplaceAll(query, " ", "")))
	if len(q) == 0 {
		return true
	}
	i := 0
	for _, r := range strings.ToLower(target) {
		if i < len(q) && r == q[i] {
			i++
			if i == len(q) {
				return true
			}
		}
	}
	return false
}

func (e modelPickerEntry) filterText() string {
	return e.providerName + " " + e.display + " " + e.modelID
}

func (s *modelPickerState) rows() []modelPickerRow {
	var rows []modelPickerRow
	for gi := range s.groups {
		g := &s.groups[gi]
		var matched []modelPickerEntry
		for _, e := range g.entries {
			if modelPickerFuzzyMatch(s.query, e.filterText()) {
				matched = append(matched, e)
			}
		}
		showCustom := g.allowCustom && g.id != "" && s.query == ""
		if len(matched) == 0 && !showCustom {
			continue
		}
		note := ""
		if g.id != "" && g.needsKey {
			note = "no API key"
		}
		if len(rows) > 0 {
			rows = append(rows, modelPickerRow{kind: modelPickerRowBlank})
		}
		rows = append(rows, modelPickerRow{kind: modelPickerRowHeader, title: g.name, note: note})
		for _, e := range matched {
			rows = append(rows, modelPickerRow{kind: modelPickerRowEntry, entry: e, recent: g.id == ""})
		}
		if showCustom {
			rows = append(rows, modelPickerRow{
				kind:  modelPickerRowCustom,
				entry: modelPickerEntry{providerID: g.id, providerName: g.name},
			})
		}
	}
	return rows
}

func (r modelPickerRow) selectable() bool {
	return r.kind == modelPickerRowEntry || r.kind == modelPickerRowCustom
}

func firstSelectableRow(rows []modelPickerRow) int {
	for i, r := range rows {
		if r.selectable() {
			return i
		}
	}
	return -1
}

func (s *modelPickerState) moveCursor(rows []modelPickerRow, delta int) {
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

func (s *modelPickerState) seedCursor(providerID, modelID string) {
	rows := s.rows()
	matchIdx := -1
	for i, r := range rows {
		if r.kind != modelPickerRowEntry || r.entry.providerID != providerID {
			continue
		}
		if modelID == "" || strings.EqualFold(r.entry.modelID, modelID) {
			matchIdx = i
			break
		}
	}
	if matchIdx < 0 {
		matchIdx = firstSelectableRow(rows)
	}
	if matchIdx < 0 {
		matchIdx = 0
	}
	s.cursor = matchIdx
}

func (m model) openModelPicker() model {
	(&m).clearSelection()
	cfg, _ := loadConfig()
	s := buildModelPickerState(cfg)
	curProv := ""
	if m.provider != nil {
		curProv = m.provider.ID()
	}
	s.seedCursor(curProv, m.providerModel)
	m.modelPicker = s
	m.mode = modeModelPicker
	return m
}

func (m model) closeModelPicker() model {
	m.mode = modeInput
	m.modelPicker = nil
	return m
}

func (m model) updateModelPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	s := m.modelPicker
	if s == nil {
		return m.closeModelPicker(), nil
	}
	if s.keyEntry != nil {
		return m.updateModelPickerKeyEntry(msg)
	}
	if s.customEntry != nil {
		return m.updateModelPickerCustomEntry(msg)
	}
	rows := s.rows()
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		return m.closeModelPicker(), nil
	case listNavPrev(msg):
		s.moveCursor(rows, -1)
		return m, nil
	case listNavNext(msg):
		s.moveCursor(rows, +1)
		return m, nil
	case msg.Code == tea.KeyEnter:
		if s.cursor < 0 || s.cursor >= len(rows) {
			return m, nil
		}
		row := rows[s.cursor]
		switch row.kind {
		case modelPickerRowEntry:
			return m.dispatchModelPick(row.entry)
		case modelPickerRowCustom:
			s.customEntry = &modelPickerCustomEntry{
				providerID:   row.entry.providerID,
				providerName: row.entry.providerName,
			}
			return m, nil
		}
		return m, nil
	case msg.Code == tea.KeyBackspace:
		if s.query != "" {
			r := []rune(s.query)
			s.query = string(r[:len(r)-1])
			s.resetCursorToFirst()
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		s.query += msg.Text
		s.resetCursorToFirst()
		return m, nil
	}
	return m, nil
}

func (s *modelPickerState) resetCursorToFirst() {
	if idx := firstSelectableRow(s.rows()); idx >= 0 {
		s.cursor = idx
	} else {
		s.cursor = 0
	}
}

func (m model) dispatchModelPick(entry modelPickerEntry) (tea.Model, tea.Cmd) {
	cfg, _ := loadConfig()
	if providerNeedsAPIKey(cfg, entry.providerID) {
		spec, _ := providerKeySpecByID(entry.providerID)
		m.modelPicker.keyEntry = &modelPickerKeyEntry{pending: entry, spec: spec}
		return m, nil
	}
	return m.applyModelPickerEntry(entry)
}

func (m model) applyModelPickerEntry(entry modelPickerEntry) (tea.Model, tea.Cmd) {
	recordRecentModel(entry.providerID, entry.modelID)
	modelID := entry.modelID
	if strings.EqualFold(modelID, "default") {
		modelID = ""
	}
	var prov Provider
	for _, p := range providerRegistry {
		if p.ID() == entry.providerID {
			prov = p
			break
		}
	}
	if prov == nil {
		return m.closeModelPicker(), nil
	}
	return m.applyProviderModelSwitch(prov, modelID)
}

func (m model) updateModelPickerKeyEntry(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.modelPicker
	ke := s.keyEntry
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		s.keyEntry = nil
		return m, nil
	case msg.Code == tea.KeyEnter:
		key := strings.TrimSpace(ke.draft)
		if key == "" {
			return m, nil
		}
		if err := withConfigLock(func() error {
			cfg, _ := loadConfig()
			ke.spec.config(&cfg).APIKey = key
			return saveConfig(cfg)
		}); err != nil {
			debugLog("%s API key saveConfig: %v", ke.spec.id, err)
			s.keyEntry = nil
			return m, m.toast.show(ke.spec.title + ": save key: " + err.Error())
		}
		pending := ke.pending
		s.keyEntry = nil
		return m.applyModelPickerEntry(pending)
	case msg.Code == tea.KeyBackspace:
		if r := []rune(ke.draft); len(r) > 0 {
			ke.draft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		ke.draft += msg.Text
		return m, nil
	}
	return m, nil
}

func (m model) updateModelPickerCustomEntry(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.modelPicker
	ce := s.customEntry
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		s.customEntry = nil
		return m, nil
	case msg.Code == tea.KeyEnter:
		id := strings.TrimSpace(ce.draft)
		if id == "" {
			return m, nil
		}
		entry := modelPickerEntry{
			providerID:   ce.providerID,
			providerName: ce.providerName,
			modelID:      id,
			display:      friendlyModelName(ce.providerID, id),
		}
		s.customEntry = nil
		return m.dispatchModelPick(entry)
	case msg.Code == tea.KeyBackspace:
		if r := []rune(ce.draft); len(r) > 0 {
			ce.draft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		ce.draft += msg.Text
		return m, nil
	}
	return m, nil
}

func (m model) applyModelPickerPaste(text string) (tea.Model, tea.Cmd) {
	s := m.modelPicker
	if s == nil || text == "" {
		return m, nil
	}
	switch {
	case s.keyEntry != nil:
		s.keyEntry.draft += text
	case s.customEntry != nil:
		s.customEntry.draft += text
	default:
		s.query += text
		s.resetCursorToFirst()
	}
	return m, nil
}

func (m model) applyProviderModelSwitch(newProv Provider, model string) (tea.Model, tea.Cmd) {
	newSettings := newProv.LoadSettings()
	sameProvider := m.provider != nil && m.provider.ID() == newProv.ID()
	var oldProvName string
	if m.provider != nil {
		oldProvName = m.provider.DisplayName()
	}

	m.killProc()

	m.provider = newProv
	m.providerModel = model
	m.providerEffort = newSettings.Effort
	m.providerSlashCmds = newSettings.SlashCommands

	if newSettings.Model != model {
		newSettings.Model = model
		if err := newProv.SaveSettings(newSettings); err != nil {
			debugLog("SaveSettings err: %v", err)
		}
	}

	m.lastUsageTokens = 0
	m.modelForContext = ""
	var historyCmd tea.Cmd
	if !sameProvider {
		m.sessionID = ""
		m.sessionMinted = false
		m.resumeCwd = ""
		if m.virtualSessionID != "" {
			historyCmd = m.applyVSProviderSwap(oldProvName, newProv)
		}
	}

	var msg string
	switch {
	case sameProvider && model != "":
		msg = "✓ " + newProv.DisplayName() + " model → " + model
	case sameProvider:
		msg = "✓ " + newProv.DisplayName() + " model cleared (provider default)"
	case model != "":
		msg = "✓ switched to " + newProv.DisplayName() + " (" + model + ")"
	default:
		msg = "✓ switched to " + newProv.DisplayName()
	}
	m.appendHistory(outputStyle.Render(promptStyle.Render(msg)))
	m = m.closeModelPicker()

	probe := newProv.ProbeInit(m.sessionArgs())
	if historyCmd != nil {
		return m, tea.Batch(probe, historyCmd)
	}
	return m, probe
}

func (m *model) applyVSProviderSwap(oldProvName string, newProv Provider) tea.Cmd {
	_ = oldProvName
	store, err := loadVirtualSessions()
	if err != nil {
		debugLog("applyVSProviderSwap load: %v", err)
		return nil
	}
	vs := store.findByID(m.virtualSessionID)
	if vs == nil {
		return nil
	}

	if ref, ok := vs.ProviderSessions[newProv.ID()]; ok && ref.SessionID != "" &&
		vs.LastProvider == newProv.ID() {
		m.sessionID = ref.SessionID
		m.resumeCwd = ref.Cwd
		m.worktreeName = worktreeNameFromCwd(ref.Cwd)
		m.history = nil
		opts := HistoryOpts{
			RenderDiffs: m.renderDiffs,
			ToolOutput:  m.toolOutputMode,
			QuietMode:   m.quietMode,
		}
		return loadHistoryCmd(m.id, newProv, ref.SessionID, vs.ID, opts, false)
	}
	turns := neutralTurnsFromHistory(m.history)
	if len(turns) == 0 {
		return nil
	}
	m.testBusy = true
	m.status = "translating session…"

	worktreeName := m.worktreeName
	if worktreeName == "" {
		worktreeName = worktreeNameFromVS(vs, vs.LastProvider)
	}
	return translateVSCmd(translateVSReq{
		tabID:       m.id,
		target:      newProv,
		vsID:        vs.ID,
		workspace:   m.cwd,
		nativeCwd:   nativeCwdForUpsert(m.cwd, worktreeName),
		directTurns: turns,
	})
}

func worktreeNameFromVS(vs *VirtualSession, preferredProviderID string) string {
	if vs == nil {
		return ""
	}
	if preferredProviderID != "" {
		if ref, ok := vs.ProviderSessions[preferredProviderID]; ok {
			if name := worktreeNameFromCwd(ref.Cwd); name != "" || ref.Cwd != "" {
				return name
			}
		}
	}
	ids := make([]string, 0, len(vs.ProviderSessions))
	for id := range vs.ProviderSessions {
		if id != preferredProviderID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if name := worktreeNameFromCwd(vs.ProviderSessions[id].Cwd); name != "" {
			return name
		}
	}
	return ""
}

const (
	modelPickerMinWidth = 60
	modelPickerMaxWidth = 84
	modelPickerMaxRows  = 12
)

func (m model) viewModelPicker() string {
	s := m.modelPicker
	if s == nil {
		return ""
	}
	if s.keyEntry != nil {
		return m.viewModelPickerKeyEntry(s.keyEntry)
	}
	if s.customEntry != nil {
		return m.viewModelPickerCustomEntry(s.customEntry)
	}

	innerW := modelPickerMinWidth
	rows := s.rows()
	for _, r := range rows {
		if w := lipgloss.Width(modelPickerRowPlain(r, m)); w+2 > innerW {
			innerW = w + 2
		}
	}
	if innerW > modelPickerMaxWidth {
		innerW = modelPickerMaxWidth
	}
	if m.width > 0 && innerW > m.width-8 {
		innerW = m.width - 8
	}
	if innerW < 24 {
		innerW = 24
	}

	title := themePickerTitleStyle.Render("Switch Model")
	searchLine := configPromptStyle.Render("> ") + filterPromptLine(s.query, "Type to filter")

	maxList := modelPickerMaxRows
	if m.height > 0 && m.height-10 < maxList {
		maxList = m.height - 10
	}
	if maxList < 4 {
		maxList = 4
	}
	start := 0
	if len(rows) > maxList && s.cursor >= maxList {
		start = s.cursor - maxList + 1
		if start > len(rows)-maxList {
			start = len(rows) - maxList
		}
	}
	end := start + maxList
	if end > len(rows) {
		end = len(rows)
	}

	lines := make([]string, 0, maxList+1)
	if len(rows) == 0 {
		lines = append(lines, dimStyle.Render("  (no matches)"))
	}
	for i := start; i < end; i++ {
		lines = append(lines, m.renderModelPickerRow(rows[i], innerW, i == s.cursor))
	}

	help := themePickerHelpStyle.Render("↑↓ choose · enter select · esc cancel")
	body := strings.Join([]string{
		title,
		"",
		searchLine,
		"",
		strings.Join(lines, "\n"),
		"",
		help,
	}, "\n")
	return themePickerBoxStyle.Width(m.modelPickerBoxWidth(innerW)).Render(body)
}

func (m model) modelPickerBoxWidth(rowW int) int {
	w := rowW + themePickerBoxStyle.GetHorizontalPadding()
	if m.width > 0 && w > m.width-6 {
		w = m.width - 6
	}
	if w < 20 {
		w = 20
	}
	return w
}

func modelPickerRowPlain(r modelPickerRow, m model) string {
	switch r.kind {
	case modelPickerRowHeader:
		t := r.title
		if r.note != "" {
			t += "  (" + r.note + ")"
		}
		return t
	case modelPickerRowEntry:
		line := "  " + r.entry.display
		if r.recent {
			line += " · " + r.entry.providerName
		}
		if m.modelPickerEntryIsCurrent(r.entry) {
			line += " ✓"
		}
		return line
	case modelPickerRowCustom:
		return "  " + modelPickerCustomRowLabel
	}
	return ""
}

func (m model) modelPickerEntryIsCurrent(e modelPickerEntry) bool {
	if m.provider == nil || m.provider.ID() != e.providerID {
		return false
	}
	if m.providerModel != "" {
		return strings.EqualFold(m.providerModel, e.modelID)
	}
	opts := m.provider.ModelPicker().Options
	return len(opts) > 0 && strings.EqualFold(opts[0], e.modelID)
}

func (m model) renderModelPickerRow(r modelPickerRow, width int, selected bool) string {
	switch r.kind {
	case modelPickerRowBlank:
		return ""
	case modelPickerRowHeader:
		line := configKeyDimStyle.Bold(true).Render(r.title)
		if r.note != "" {
			line += "  " + errStyle.Render("("+r.note+")")
		}
		return line
	}

	plain := modelPickerRowPlain(r, m)
	if selected {
		line := "▸ " + strings.TrimPrefix(plain, "  ")
		if pad := width - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		return themePickerRowStyle.Render(line)
	}
	if r.kind == modelPickerRowCustom {
		return dimStyle.Render(plain)
	}
	if r.recent {
		return "  " + r.entry.display + dimStyle.Render(" · "+r.entry.providerName) +
			modelPickerCurrentMark(m, r.entry)
	}
	return "  " + r.entry.display + modelPickerCurrentMark(m, r.entry)
}

func modelPickerCurrentMark(m model, e modelPickerEntry) string {
	if m.modelPickerEntryIsCurrent(e) {
		return promptStyle.Render(" ✓")
	}
	return ""
}

func (m model) viewModelPickerKeyEntry(ke *modelPickerKeyEntry) string {
	title := themePickerTitleStyle.Render(ke.spec.title + " API key")
	sub := configHelpStyle.Render(
		ke.spec.title + " has no API key configured. Paste or type one to continue —\n" +
			"it is stored in ~/.config/ask/ask.json (0600). $" + ke.spec.envKey + " also works.")
	pick := dimStyle.Render("model: " + ke.pending.display)
	input := configPromptStyle.Render("> ") + ke.draft + configCaretStyle.Render("▏")
	help := themePickerHelpStyle.Render("enter save & switch · esc back")
	body := strings.Join([]string{title, "", sub, "", pick, input, "", help}, "\n")
	return themePickerBoxStyle.Width(m.modelPickerBoxWidth(modelPickerMinWidth)).Render(body)
}

func (m model) viewModelPickerCustomEntry(ce *modelPickerCustomEntry) string {
	title := themePickerTitleStyle.Render("Custom " + ce.providerName + " model")
	sub := configHelpStyle.Render("Type the exact model id to use with " + ce.providerName + ".")
	input := configPromptStyle.Render("> ") + ce.draft + configCaretStyle.Render("▏")
	help := themePickerHelpStyle.Render("enter select · esc back")
	body := strings.Join([]string{title, "", sub, "", input, "", help}, "\n")
	return themePickerBoxStyle.Width(m.modelPickerBoxWidth(modelPickerMinWidth)).Render(body)
}
