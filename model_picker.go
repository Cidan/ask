package main

import (
	"errors"
	"sort"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	lipgloss "charm.land/lipgloss/v2"
)

// Ctrl+M opens the unified model picker (crush-style): a search input
// on top, a "Recently used" group first, then one section per
// registered provider listing its models with human-friendly names
// from catwalk's catalog. ↑/↓ (and Ctrl+P/Ctrl+N) walk the model rows
// skipping section headers; Enter applies the pick to the current tab
// and persists it as the provider's default model (what /model used to
// do). The persisted *default provider* for new tabs is untouched —
// that stays in /config → Default Provider. Picking a model whose
// provider has no API key configured drops into an inline key prompt
// first; the key is saved to ~/.config/ask/ask.json and the pick
// proceeds.

// providerKeySpec describes where one in-process API provider's key
// lives: the config block in askConfig and the conventional
// environment fallback. Providers without a spec (tests, future
// keyless backends) never gate on a key.
type providerKeySpec struct {
	id     string
	title  string
	envKey string
	config func(*askConfig) *apiProviderConfig
}

var providerKeySpecs = []providerKeySpec{
	{anthropicProviderID, "Anthropic", anthropicEnvAPIKey,
		func(c *askConfig) *apiProviderConfig { return &c.Anthropic }},
	{openaiProviderID, "OpenAI", openaiEnvAPIKey,
		func(c *askConfig) *apiProviderConfig { return &c.OpenAI }},
	{deepseekProviderID, "DeepSeek", deepseekEnvAPIKey,
		func(c *askConfig) *apiProviderConfig { return &c.DeepSeek }},
}

func providerKeySpecByID(id string) (providerKeySpec, bool) {
	for _, s := range providerKeySpecs {
		if s.id == id {
			return s, true
		}
	}
	return providerKeySpec{}, false
}

// missingAPIKeyError is the fail-fast session-start error for a
// provider with no key anywhere. Names the model picker (with its
// live binding when bound) since that's where the inline key prompt
// lives now that /config no longer carries per-provider key rows.
func missingAPIKeyError(envKey string) error {
	picker := "the model picker"
	if k := keyHintFor(ActionProviderSwitch); k != "" {
		picker += " (" + k + ")"
	}
	return errors.New("no API key configured — add one via " + picker + ", or export " + envKey)
}

// providerNeedsAPIKey reports whether the provider requires an API key
// that is currently missing (config field and environment both empty).
func providerNeedsAPIKey(cfg askConfig, providerID string) bool {
	spec, ok := providerKeySpecByID(providerID)
	if !ok {
		return false
	}
	return resolveAPIProviderKey(*spec.config(&cfg), spec.envKey) == ""
}

// catwalkProviderIDs maps ask provider ids onto catwalk's catalog ids
// so model rows can show the published display names.
var catwalkProviderIDs = map[string]catwalk.InferenceProvider{
	anthropicProviderID: catwalk.InferenceProviderAnthropic,
	openaiProviderID:    catwalk.InferenceProviderOpenAI,
	deepseekProviderID:  catwalk.InferenceProviderDeepSeek,
}

// friendlyModelName resolves a model id to a human-friendly display
// name: the catwalk catalog name when known (dashes swapped for
// spaces — "DeepSeek-V4-Pro" → "DeepSeek V4 Pro"), a humanized id
// otherwise.
func friendlyModelName(providerID, modelID string) string {
	if cw, ok := catwalkProviderIDs[providerID]; ok {
		if mdl, ok := catalogModel(cw, modelID); ok && mdl.Name != "" {
			return strings.ReplaceAll(mdl.Name, "-", " ")
		}
	}
	return humanizeModelID(modelID)
}

// humanizeModelID is the fallback display transform for ids the
// catalog doesn't know: dash/underscore-separated words, each
// title-cased ("deepseek-v4-pro" → "Deepseek V4 Pro").
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

// modelPickerCustomRowLabel is the per-provider free-entry affordance.
const modelPickerCustomRowLabel = "Enter your own…"

// modelPickerEntry is one selectable model row.
type modelPickerEntry struct {
	providerID   string
	providerName string
	modelID      string
	display      string
}

type modelPickerGroup struct {
	id          string // provider id; empty for "Recently used"
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

// modelPickerRow is one rendered line of the flat list, derived from
// the groups + filter on demand (the groups snapshot is the single
// source of truth; rows are a pure projection).
type modelPickerRow struct {
	kind  modelPickerRowKind
	title string // header text
	note  string // dim header suffix ("no API key")
	entry modelPickerEntry
	// recent marks entries rendered under "Recently used", which
	// carry a dim provider suffix so same-named models stay
	// distinguishable outside their provider section.
	recent bool
}

// modelPickerKeyEntry is the inline API-key prompt sub-state: the pick
// that triggered it waits on pending while the user types the key.
type modelPickerKeyEntry struct {
	pending modelPickerEntry
	spec    providerKeySpec
	draft   string
}

// modelPickerCustomEntry is the inline free-text model-id editor.
type modelPickerCustomEntry struct {
	providerID   string
	providerName string
	draft        string
}

// modelPickerState lives on the model while modeModelPicker is up.
// Groups are snapshotted at open; query + cursor are the only moving
// parts. keyEntry/customEntry switch the modal into its inline-editor
// faces.
type modelPickerState struct {
	query       string
	cursor      int // index into rows()
	groups      []modelPickerGroup
	keyEntry    *modelPickerKeyEntry
	customEntry *modelPickerCustomEntry
}

// buildModelPickerState snapshots the registry + recents into picker
// groups. Recents come first (crush-style); each entry there carries a
// dim provider suffix at render time. A recent ref whose model id is
// no longer in the provider's list (a custom id) is synthesized so the
// user can re-pick it; refs to unknown providers are dropped.
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

// modelPickerFuzzyMatch reports whether every query rune appears in
// order inside target (case-insensitive; spaces in the query are
// ignored, same as crush's filter).
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

// rows projects groups + query into the flat render list. Headers and
// blank separators are not selectable; selectable rows are entries and
// the per-provider custom affordance (which only shows with an empty
// query — it isn't a model to match against).
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

// firstSelectable returns the index of the first selectable row, or -1.
func firstSelectableRow(rows []modelPickerRow) int {
	for i, r := range rows {
		if r.selectable() {
			return i
		}
	}
	return -1
}

// moveCursor advances the cursor delta selectable rows, wrapping.
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

// seedCursor lands the cursor on the row matching the current tab's
// provider + model. An empty model means "provider default", which the
// providers list with their default id first — match the provider's
// head row in that case. Falls back to the first selectable row.
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

// openModelPicker enters the unified picker, seeded on the current
// tab's provider/model.
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

// dispatchModelPick routes a confirmed pick: straight to the switch
// when the provider has a usable key, into the inline key prompt
// otherwise.
func (m model) dispatchModelPick(entry modelPickerEntry) (tea.Model, tea.Cmd) {
	cfg, _ := loadConfig()
	if providerNeedsAPIKey(cfg, entry.providerID) {
		spec, _ := providerKeySpecByID(entry.providerID)
		m.modelPicker.keyEntry = &modelPickerKeyEntry{pending: entry, spec: spec}
		return m, nil
	}
	return m.applyModelPickerEntry(entry)
}

// applyModelPickerEntry records the recent pick and swaps the current
// tab to the entry's provider/model.
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

// applyModelPickerPaste appends pasted text to whichever input is
// active — API keys especially are pasted far more often than typed.
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

// applyProviderModelSwitch swaps the current tab to prov with the
// given model, kills the active proc, and reloads the target
// provider's saved effort/slash-command defaults. Same-provider swaps
// (model-only changes) preserve sessionID/resumeCwd so the next
// ensureProc picks up where the conversation left off. The model is
// persisted as the provider's default; the default-provider config is
// untouched.
func (m model) applyProviderModelSwitch(newProv Provider, model string) (tea.Model, tea.Cmd) {
	newSettings := newProv.LoadSettings()
	sameProvider := m.provider != nil && m.provider.ID() == newProv.ID()
	var oldProvName string
	if m.provider != nil {
		oldProvName = m.provider.DisplayName()
	}

	// Kill the active proc — even on a same-provider swap, the new
	// model only takes effect after a fresh fork.
	m.killProc()

	m.provider = newProv
	m.providerModel = model
	m.providerEffort = newSettings.Effort
	m.providerSlashCmds = newSettings.SlashCommands
	// Persist the model as the provider's default (what /model used to
	// do) so fresh tabs and restarts inherit the pick. The *default
	// provider* (cfg.Provider) is deliberately untouched — that knob
	// stays in /config → Default Provider.
	if newSettings.Model != model {
		newSettings.Model = model
		if err := newProv.SaveSettings(newSettings); err != nil {
			debugLog("SaveSettings err: %v", err)
		}
	}
	// Zero all usage telemetry so the chip never shows stale numbers
	// from the previous provider. Both cross-provider and same-provider
	// swaps clear — a model change for the same provider still drops
	// session context, and the new session's first stream events will
	// re-populate as needed.
	m.lastUsageTokens = 0
	m.modelForContext = ""
	var historyCmd tea.Cmd
	if !sameProvider {
		// Cross-provider swap: if the tab is inside a virtual session,
		// route the new provider's resume context through the VS store.
		// A native mapping → resume it on the new provider (UI reloads
		// via loadHistoryCmd). No mapping → materialize a synthetic
		// native session file on the new provider from the current
		// m.history and resume from that.
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
	// Refresh slash commands from the new provider so /resume, /effort,
	// etc. match. Same-provider swaps still re-probe so any cached
	// commands reflect whatever the new model unlocks.
	probe := newProv.ProbeInit(m.sessionArgs())
	if historyCmd != nil {
		return m, tea.Batch(probe, historyCmd)
	}
	return m, probe
}

// applyVSProviderSwap routes a cross-provider swap through the VS
// store. When the new provider already has a native mapping, sets
// sessionID/resumeCwd and returns loadHistoryCmd so the UI reflects
// the new provider's own file. When no mapping exists, materializes
// a synthetic native session file from the current m.history turns
// and schedules the new provider to resume it. Returns nil when the
// store is unreachable or the VS has vanished between tabs.
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
	// Reuse the cached native id only when the new provider was also
	// the last writer. Any other LastProvider means the cached
	// mapping predates newer turns on a different backend, so the
	// canonical state lives in m.history (which we just had rendered
	// by the provider we're leaving) — translate from those turns.
	if ref, ok := vs.ProviderSessions[newProv.ID()]; ok && ref.SessionID != "" &&
		vs.LastProvider == newProv.ID() {
		m.sessionID = ref.SessionID
		m.resumeCwd = ref.Cwd
		// Realign m.worktreeName with where the resumed session
		// actually lived. A worktree-rooted ref hands over its name so
		// any second swap before the first fork can translate into the
		// same worktree; a project-root ref clears any stale name from
		// the prior tab so we don't accidentally fork at a worktree
		// the resumed session was never written to.
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
	m.busy = true
	m.status = "translating session…"
	// Recover a worktree name from the last-writer ref in the VS so
	// the translated session lands where the canonical conversation
	// already lived. Only fall back to some other provider's worktree
	// when the authoritative ref has lost its cwd entirely; an
	// explicit project-root ref stays project-root.
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

// worktreeNameFromVS resolves the worktree assignment recorded on vs.
// When preferredProviderID has a ref, that ref is authoritative: a
// worktree path returns its name, an explicit project-root cwd
// returns "", and only a missing/empty cwd falls through to other
// refs. Fallback scanning is deterministic (sorted provider ids) so
// mixed/ref-corrupted VS rows don't produce random worktree choices.
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

// ---- rendering ----

// The picker leans wide-and-flat (house style for design popups): a
// generous min/max width and a tighter visible-row cap so the modal
// reads as a landscape rectangle rather than a tower.
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

	// Visible window: keep the cursor in view when the list outgrows
	// the terminal.
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

// modelPickerBoxWidth converts a row width into the lipgloss block
// width (content + the box style's horizontal padding) and clamps it
// to the terminal. Shared by the list view and the inline editors so
// the modal keeps one silhouette across its faces.
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

// modelPickerRowPlain is the unstyled text of a row, used for width
// measurement and as the base for highlight rendering.
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

// modelPickerEntryIsCurrent reports whether the entry matches the
// current tab's provider+model (empty tab model = provider default =
// the provider's head row).
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

	// Selectable rows.
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

// modelPickerCurrentMark renders the ✓ suffix for the current
// provider+model entry.
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
