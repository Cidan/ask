package main

import (
	"errors"
	"sort"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/providers"
)

// Ctrl+M opens the model picker: a centered overlay drawn over the whole
// terminal (sidebar included, see app.View) — one square-cornered box with
// the searchable provider/model list on the left, a thin divider, and the
// selected model's details on the right. The list is seeded from whatever
// the catalog cache already holds and refreshed when the async load lands.

func missingAPIKeyError(envKey string) error {
	picker := "the model picker"
	if k := keyHintFor(ActionProviderSwitch); k != "" {
		picker += " (" + k + ")"
	}
	return errors.New("no API key configured — add one via " + picker + ", or export " + envKey)
}

// providerNeedsAPIKey reports whether picking providerID should first ask
// for its credential: the provider declares a Secret setting and neither
// config nor its env fallback supplies one. Providers without a secret
// (Vertex authenticates with ADC) never prompt.
func providerNeedsAPIKey(cfg askConfig, providerID string) bool {
	p, ok := providers.Get(providerID)
	if !ok {
		return false
	}
	f, ok := providers.SecretField(p)
	if !ok {
		return false
	}
	return providers.SettingValue(cfg.ProviderConfig(providerID), f) == ""
}

func friendlyModelName(providerID, modelID string) string {
	if meta, ok := modelMetaFor(providerID, modelID); ok && meta.Name != "" {
		return meta.Name
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
	note        string
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
	field   providers.SettingField
	draft   string
}

// title is the prompt's heading: "OpenRouter API Key".
func (ke *modelPickerKeyEntry) title() string {
	return ke.pending.providerName + " " + ke.field.Title
}

type modelPickerCustomEntry struct {
	providerID   string
	providerName string
	draft        string
}

type modelPickerState struct {
	query        string
	cursor       int
	groups       []modelPickerGroup
	keyEntry     *modelPickerKeyEntry
	customEntry  *modelPickerCustomEntry
	detailScroll int
	// loading is true while a catalog load dispatched for this picker is in
	// flight; the list shows whatever is cached meanwhile.
	loading bool
	// descCache holds rendered descriptions keyed by provider, model, and
	// pane width — glamour runs once per model, not once per keystroke.
	descCache map[string]string
	// stepTarget retargets the picker for the workflows builder: when it is
	// set, choosing a model writes step.Provider/step.Model on
	// m.workflowsBuilder and returns to the builder, instead of switching the
	// live tab's provider/model. nil ⇒ the Ctrl+M behavior. See
	// openModelPickerForStep.
	stepTarget *stepTarget
}

func buildModelPickerState(cfg askConfig) *modelPickerState {
	s := &modelPickerState{}
	notes := modelCatalogNotes()

	providerGroups := make([]modelPickerGroup, 0, len(providerRegistry))
	for _, p := range providerRegistry {
		picker := p.ModelPicker()
		g := modelPickerGroup{
			id:          p.ID(),
			name:        p.DisplayName(),
			allowCustom: picker.AllowCustom,
			needsKey:    providerNeedsAPIKey(cfg, p.ID()),
			note:        notes[p.ID()],
		}
		for _, id := range picker.Options {
			g.entries = append(g.entries, modelPickerEntry{
				providerID:   p.ID(),
				providerName: p.DisplayName(),
				modelID:      id,
				display:      friendlyModelName(p.ID(), id),
			})
		}
		sortModelPickerEntries(g.entries)
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
		recent.entries = append(recent.entries, modelPickerEntry{
			providerID:   prov.id,
			providerName: prov.name,
			modelID:      ref.Model,
			display:      friendlyModelName(prov.id, ref.Model),
		})
	}
	if len(recent.entries) > 0 {
		s.groups = append(s.groups, recent)
	}
	s.groups = append(s.groups, providerGroups...)
	return s
}

// sortModelPickerEntries orders a provider's models naturally by display
// name (so "Gemini 2.5" < "Gemini 3" < "Gemini 3.1" < "Gemini 10"), ids
// breaking ties. Recents keep recency order and are never sorted.
func sortModelPickerEntries(entries []modelPickerEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := strings.ToLower(entries[i].display), strings.ToLower(entries[j].display)
		if a != b {
			return naturalLess(a, b)
		}
		return naturalLess(strings.ToLower(entries[i].modelID), strings.ToLower(entries[j].modelID))
	})
}

// rebuild re-reads the registry and cache (after a catalog load) while
// keeping the query and the cursor on the entry the user was on.
func (s *modelPickerState) rebuild(cfg askConfig) {
	var cur *modelPickerEntry
	if rows := s.rows(); s.cursor >= 0 && s.cursor < len(rows) && rows[s.cursor].kind == modelPickerRowEntry {
		e := rows[s.cursor].entry
		cur = &e
	}
	fresh := buildModelPickerState(cfg)
	s.groups = fresh.groups
	if cur != nil {
		s.seedCursor(cur.providerID, cur.modelID)
		return
	}
	s.resetCursorToFirst()
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
		switch {
		case g.id != "" && g.needsKey:
			note = "no API key"
		case g.note != "":
			note = g.note
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
	s.detailScroll = 0
}

func (s *modelPickerState) resetCursorToFirst() {
	if idx := firstSelectableRow(s.rows()); idx >= 0 {
		s.cursor = idx
	} else {
		s.cursor = 0
	}
	s.detailScroll = 0
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

// modelPickerLoadCmd kicks off a catalog load for the open picker unless one
// is already running or (without force) already succeeded.
func (m *model) modelPickerLoadCmd(force bool) tea.Cmd {
	cmd := modelCatalogRefreshCmd(force)
	if cmd != nil && m.modelPicker != nil {
		m.modelPicker.loading = true
	}
	return cmd
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
	case msg.Mod == tea.ModCtrl && msg.Code == 'r':
		return m, m.modelPickerLoadCmd(true)
	case listNavPrev(msg):
		s.moveCursor(rows, -1)
		return m, nil
	case listNavNext(msg):
		s.moveCursor(rows, +1)
		return m, nil
	case msg.Code == tea.KeyPgUp:
		s.detailScroll -= modelPickerScrollStep
		if s.detailScroll < 0 {
			s.detailScroll = 0
		}
		return m, nil
	case msg.Code == tea.KeyPgDown:
		s.detailScroll += modelPickerScrollStep
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

func (m model) dispatchModelPick(entry modelPickerEntry) (tea.Model, tea.Cmd) {
	cfg, _ := loadConfig()
	if providerNeedsAPIKey(cfg, entry.providerID) {
		p, _ := providers.Get(entry.providerID)
		f, _ := providers.SecretField(p)
		m.modelPicker.keyEntry = &modelPickerKeyEntry{pending: entry, field: f}
		return m, nil
	}
	return m.applyModelPickerEntry(entry)
}

func (m model) applyModelPickerEntry(entry modelPickerEntry) (tea.Model, tea.Cmd) {
	recordRecentModel(entry.providerID, entry.modelID)
	if m.modelPicker != nil && m.modelPicker.stepTarget != nil {
		return m.applyModelPickerToStep(*m.modelPicker.stepTarget, entry)
	}
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
		id := ke.pending.providerID
		if err := withConfigLock(func() error {
			cfg, _ := loadConfig()
			cfg.SetProviderConfig(id, cfg.ProviderConfig(id).WithField(ke.field.Key, key))
			return saveConfig(cfg)
		}); err != nil {
			debugLog("%s %s saveConfig: %v", id, ke.field.Key, err)
			s.keyEntry = nil
			return m, m.toast.show(ke.title() + ": save key: " + err.Error())
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

	// A Ctrl+M pick becomes the default provider for all new tabs and the next
	// launch — the picked provider's model was just persisted above, and the
	// startup path reads cfg.Provider + that provider's saved model.
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.Provider = newProv.ID()
		return saveConfig(cfg)
	}); err != nil {
		debugLog("persist default provider err: %v", err)
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
		m.transcript = nil
		m.responseActive = false
		return loadHistoryCmd(m.id, newProv, ref.SessionID, vs.ID, false)
	}
	turns := neutralTurnsFromTranscript(m.transcript)
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
