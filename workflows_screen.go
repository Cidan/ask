package main

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// workflowsBuilderLevel names the navigation depth inside the
// workflows builder. Adding levels is a new constant; adding
// sub-modals (e.g. the provider picker overlay) just gates input
// behind a bool flag on workflowsBuilderState.
type workflowsBuilderLevel int

const (
	workflowsLevelList  workflowsBuilderLevel = iota // workflow list
	workflowsLevelSteps                              // step list for selected workflow
	workflowsLevelStep                               // step editor (4 fields)
)

// workflowsStepField is the focused field on the step-editor screen.
type workflowsStepField int

const (
	workflowsStepFieldName workflowsStepField = iota
	workflowsStepFieldProvider
	workflowsStepFieldModel
	workflowsStepFieldPrompt
)

// workflowsBuilderState bundles every per-tab editor surface so the
// model.struct stays compact. Each level has its own cursor; the
// inline name editor and prompt textarea are mutually exclusive
// (only one input at a time). Disk writes happen on every commit so
// "back" is never a save action — exit is just navigation.
type workflowsBuilderState struct {
	level workflowsBuilderLevel

	// cwd is the project root used for loadConfig/saveConfig calls.
	// Captured on screen entry and never changes for the lifetime of
	// the builder; the builder is bound to one project at a time.
	cwd string

	// listCursor is the row cursor on the workflow list; len(items)
	// itself targets the trailing "+ New workflow" row.
	listCursor int

	// selectedWorkflow indexes the currently-edited workflow on the
	// step list / step editor levels. Re-validated on every render
	// against len(items).
	selectedWorkflow int

	// stepsCursor is the row cursor on the step list; len(steps)
	// itself targets the trailing "+ New step" row.
	stepsCursor int

	// stepCursor is the focused field on the step editor.
	stepCursor workflowsStepField

	// renaming = "workflow" while the inline workflow name editor
	// is open, "step" while the inline step name editor is open,
	// "" otherwise. The draft buffer accumulates keystrokes; Enter
	// commits, Esc cancels.
	renaming    string
	renameDraft string

	// providerPicker / modelPicker flag the small overlays for
	// step.Provider and step.Model selection. Each carries its own
	// cursor; modelPickerOpts is rebuilt every time the provider
	// picker commits so the model list always matches the freshly-
	// selected provider's options.
	providerPicker  bool
	providerCursor  int
	modelPicker     bool
	modelCursor     int
	modelPickerOpts []string

	// prompt is the in-flight multi-line textarea editor for the
	// step prompt. Non-nil while open; Enter inserts a newline,
	// Ctrl+S commits, Esc cancels — same shape as the chat input
	// but in a dedicated editor frame.
	prompt *textarea.Model

	// confirming = "delete-workflow" or "delete-step" while a
	// confirm overlay is up; "" otherwise. The 0/1 cursor swaps
	// between Cancel and Delete; Enter on Delete commits.
	confirming    string
	confirmCursor int

	// toast carries a short status line ("workflow saved", "deleted",
	// edit-blocked) shown in the help row for one render cycle. Not
	// strictly required, but keeps the screen self-explaining.
	toast string

	// items is the local snapshot of cfg.Projects[cwd].Workflows.Items.
	// Refreshed from disk on screen entry and after every commit so
	// the screen always reflects the persisted state.
	items []workflowDef
}

// workflowsScreen is the screen-interface implementation. State lives
// on m.workflowsBuilder; the handler is stateless. Same pattern as
// askScreen and issuesScreen.
type workflowsScreen struct{}

func (workflowsScreen) id() screenID { return screenWorkflows }

func (workflowsScreen) updateKey(m model, msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	if m.workflowsBuilder == nil {
		m.workflowsBuilder = newWorkflowsBuilderState(m.cwd)
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id), true
	}
	b := m.workflowsBuilder
	// Mode/overlay precedence: confirm > rename > prompt > pickers
	// > level. Each mode owns the keyboard until it dismisses, so
	// stray keys can't leak into the level beneath.
	if b.confirming != "" {
		return m.workflowsBuilderUpdateConfirm(msg)
	}
	if b.renaming != "" {
		return m.workflowsBuilderUpdateRename(msg)
	}
	if b.prompt != nil {
		return m.workflowsBuilderUpdatePrompt(msg)
	}
	if b.providerPicker {
		return m.workflowsBuilderUpdateProviderPicker(msg)
	}
	if b.modelPicker {
		return m.workflowsBuilderUpdateModelPicker(msg)
	}
	switch b.level {
	case workflowsLevelList:
		return m.workflowsBuilderUpdateList(msg)
	case workflowsLevelSteps:
		return m.workflowsBuilderUpdateSteps(msg)
	case workflowsLevelStep:
		return m.workflowsBuilderUpdateStep(msg)
	}
	return m, nil, true
}

func (workflowsScreen) view(m model) string {
	if m.workflowsBuilder == nil {
		return ""
	}
	return m.workflowsBuilder.render(m.width, m.height)
}

// newWorkflowsBuilderState seeds a fresh builder pinned to cwd,
// hydrating the local items snapshot from disk. Always non-nil.
func newWorkflowsBuilderState(cwd string) *workflowsBuilderState {
	b := &workflowsBuilderState{cwd: cwd}
	b.refreshItems()
	return b
}

// refreshItems re-reads the workflow list from disk so the builder
// always reflects the persisted state. Cursor positions are clamped
// to the new list length so an out-of-range cursor (e.g. after
// delete) snaps to the closest valid row.
func (b *workflowsBuilderState) refreshItems() {
	b.items = projectWorkflows(b.cwd)
	if b.listCursor > len(b.items) {
		b.listCursor = len(b.items)
	}
	if b.selectedWorkflow >= len(b.items) {
		b.selectedWorkflow = len(b.items) - 1
		if b.selectedWorkflow < 0 {
			b.selectedWorkflow = 0
		}
	}
	if b.selectedWorkflow >= 0 && b.selectedWorkflow < len(b.items) {
		steps := b.items[b.selectedWorkflow].Steps
		if b.stepsCursor > len(steps) {
			b.stepsCursor = len(steps)
		}
	}
}

// commitItems writes b.items back to disk under cwd's project entry,
// then re-hydrates so any normalisation the persistence layer applies
// is reflected in the local copy.
func (b *workflowsBuilderState) commitItems() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	pc := loadProjectConfig(cfg, b.cwd)
	if len(b.items) == 0 {
		pc.Workflows.Items = nil
	} else {
		pc.Workflows.Items = append([]workflowDef(nil), b.items...)
	}
	cfg = upsertProjectConfig(cfg, b.cwd, pc)
	if err := saveConfig(cfg); err != nil {
		return err
	}
	b.refreshItems()
	return nil
}

// uniqueWorkflowName returns a name not already used by any
// workflow in b.items. Used by "+ New workflow" / "Duplicate" so the
// list never has two rows that collide on Name (the runtime / picker
// look up by Name).
func (b *workflowsBuilderState) uniqueWorkflowName(seed string) string {
	if seed == "" {
		seed = "untitled"
	}
	taken := make(map[string]struct{}, len(b.items))
	for _, w := range b.items {
		taken[w.Name] = struct{}{}
	}
	if _, clash := taken[seed]; !clash {
		return seed
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", seed, i)
		if _, clash := taken[candidate]; !clash {
			return candidate
		}
	}
}

// uniqueStepName returns a step name not already used inside the
// workflow at b.selectedWorkflow. Same shape as uniqueWorkflowName,
// just scoped per-workflow.
func (b *workflowsBuilderState) uniqueStepName(seed string) string {
	if seed == "" {
		seed = "step"
	}
	if b.selectedWorkflow < 0 || b.selectedWorkflow >= len(b.items) {
		return seed
	}
	taken := make(map[string]struct{})
	for _, s := range b.items[b.selectedWorkflow].Steps {
		taken[s.Name] = struct{}{}
	}
	if _, clash := taken[seed]; !clash {
		return seed
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", seed, i)
		if _, clash := taken[candidate]; !clash {
			return candidate
		}
	}
}

// activeWorkflowGuard returns the toast string when destructive
// edits are blocked because the current workflow is running. Empty
// when no guard applies.
func (b *workflowsBuilderState) activeWorkflowGuard() string {
	if b.selectedWorkflow < 0 || b.selectedWorkflow >= len(b.items) {
		return ""
	}
	name := b.items[b.selectedWorkflow].Name
	active := workflowTracker().activeWorkflowNames()
	if _, running := active[name]; running {
		return "blocked: workflow is running"
	}
	return ""
}

// ----- Level 0: workflow list -----

func (m model) workflowsBuilderUpdateList(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		// Esc on level 0: leave the screen back to ask.
		m.workflowsBuilder = nil
		return m.switchScreen(screenAsk), nil, true
	case msg.Code == tea.KeyUp:
		if b.listCursor > 0 {
			b.listCursor--
		}
		return m, nil, true
	case msg.Code == tea.KeyDown:
		max := len(b.items)
		if b.listCursor < max {
			b.listCursor++
		}
		return m, nil, true
	case msg.Code == tea.KeyEnter:
		if b.listCursor == len(b.items) {
			// "+ New workflow" — create with a unique seed name and
			// drill into Level 1 immediately so the user can start
			// adding steps.
			b.items = append(b.items, workflowDef{Name: b.uniqueWorkflowName("untitled")})
			if err := b.commitItems(); err != nil {
				b.toast = "save failed: " + err.Error()
				return m, nil, true
			}
			b.selectedWorkflow = len(b.items) - 1
			b.stepsCursor = 0
			b.level = workflowsLevelSteps
			return m, nil, true
		}
		b.selectedWorkflow = b.listCursor
		b.stepsCursor = 0
		b.level = workflowsLevelSteps
		return m, nil, true
	case msg.Mod == 0 && msg.Code == 'r':
		// Inline rename of the focused workflow row.
		if b.listCursor < 0 || b.listCursor >= len(b.items) {
			return m, nil, true
		}
		if guard := b.activeWorkflowGuardForCursor(b.listCursor); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		b.renaming = "workflow"
		b.renameDraft = b.items[b.listCursor].Name
		return m, nil, true
	case msg.Mod == 0 && msg.Code == 'd':
		// Confirm delete of the focused workflow row.
		if b.listCursor < 0 || b.listCursor >= len(b.items) {
			return m, nil, true
		}
		if guard := b.activeWorkflowGuardForCursor(b.listCursor); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		b.confirming = "delete-workflow"
		b.confirmCursor = 0
		return m, nil, true
	}
	return m, nil, true
}

// activeWorkflowGuardForCursor is the same as activeWorkflowGuard
// but evaluated against an arbitrary list cursor, not b.selectedWorkflow.
// Used by the list-level rename/delete which targets the row under
// the cursor rather than the drilled-in workflow.
func (b *workflowsBuilderState) activeWorkflowGuardForCursor(idx int) string {
	if idx < 0 || idx >= len(b.items) {
		return ""
	}
	name := b.items[idx].Name
	active := workflowTracker().activeWorkflowNames()
	if _, running := active[name]; running {
		return "blocked: workflow is running"
	}
	return ""
}

// ----- Level 1: step list (workflow's steps) -----

func (m model) workflowsBuilderUpdateSteps(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	if b.selectedWorkflow < 0 || b.selectedWorkflow >= len(b.items) {
		b.level = workflowsLevelList
		return m, nil, true
	}
	steps := b.items[b.selectedWorkflow].Steps
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		b.level = workflowsLevelList
		return m, nil, true
	case msg.Code == tea.KeyUp:
		if b.stepsCursor > 0 {
			b.stepsCursor--
		}
		return m, nil, true
	case msg.Code == tea.KeyDown:
		if b.stepsCursor < len(steps) {
			b.stepsCursor++
		}
		return m, nil, true
	case msg.Code == tea.KeyEnter:
		if b.stepsCursor == len(steps) {
			if guard := b.activeWorkflowGuard(); guard != "" {
				b.toast = guard
				return m, nil, true
			}
			defaultProvider := ""
			if len(providerRegistry) > 0 {
				defaultProvider = providerRegistry[0].ID()
			}
			b.items[b.selectedWorkflow].Steps = append(steps, workflowStep{
				Name:     b.uniqueStepName("step"),
				Provider: defaultProvider,
			})
			if err := b.commitItems(); err != nil {
				b.toast = "save failed: " + err.Error()
				return m, nil, true
			}
			b.stepsCursor = len(b.items[b.selectedWorkflow].Steps) - 1
			b.stepCursor = workflowsStepFieldName
			b.level = workflowsLevelStep
			return m, nil, true
		}
		b.stepCursor = workflowsStepFieldName
		b.level = workflowsLevelStep
		return m, nil, true
	case msg.Mod == 0 && msg.Code == 'r':
		// 'r' on the step list renames the parent workflow (the
		// workflow name is rendered in the title; rename is the
		// only way to change it from this level). Step rename is on
		// the step-editor level.
		if guard := b.activeWorkflowGuard(); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		b.renaming = "workflow"
		b.renameDraft = b.items[b.selectedWorkflow].Name
		return m, nil, true
	case msg.Mod == 0 && msg.Code == 'd':
		if b.stepsCursor < 0 || b.stepsCursor >= len(steps) {
			return m, nil, true
		}
		if guard := b.activeWorkflowGuard(); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		b.confirming = "delete-step"
		b.confirmCursor = 0
		return m, nil, true
	}
	return m, nil, true
}

// ----- Level 2: step editor -----

func (m model) workflowsBuilderUpdateStep(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	if b.selectedWorkflow < 0 || b.selectedWorkflow >= len(b.items) {
		b.level = workflowsLevelList
		return m, nil, true
	}
	steps := b.items[b.selectedWorkflow].Steps
	if b.stepsCursor < 0 || b.stepsCursor >= len(steps) {
		b.level = workflowsLevelSteps
		return m, nil, true
	}
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		b.level = workflowsLevelSteps
		return m, nil, true
	case msg.Code == tea.KeyUp:
		if b.stepCursor > 0 {
			b.stepCursor--
		}
		return m, nil, true
	case msg.Code == tea.KeyDown:
		if b.stepCursor < workflowsStepFieldPrompt {
			b.stepCursor++
		}
		return m, nil, true
	case msg.Code == tea.KeyEnter:
		if guard := b.activeWorkflowGuard(); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		switch b.stepCursor {
		case workflowsStepFieldName:
			b.renaming = "step"
			b.renameDraft = steps[b.stepsCursor].Name
		case workflowsStepFieldProvider:
			b.providerPicker = true
			b.providerCursor = indexOfRegisteredProvider(steps[b.stepsCursor].Provider)
		case workflowsStepFieldModel:
			step := steps[b.stepsCursor]
			b.modelPickerOpts = modelOptionsForProvider(step.Provider)
			b.modelPicker = true
			b.modelCursor = indexOfModel(b.modelPickerOpts, step.Model)
		case workflowsStepFieldPrompt:
			ta := newPromptTextarea(steps[b.stepsCursor].Prompt)
			b.prompt = &ta
		}
		return m, nil, true
	}
	return m, nil, true
}

// indexOfRegisteredProvider returns the registry index for `id`, or
// 0 when no match. Used by the step provider picker to seed the
// cursor on the current value.
func indexOfRegisteredProvider(id string) int {
	for i, p := range providerRegistry {
		if p.ID() == id {
			return i
		}
	}
	return 0
}

// modelOptionsForProvider returns the option strings the step model
// picker should show. Wraps the existing modelPickerOptions helper.
// Empty when the provider has no model picker.
func modelOptionsForProvider(id string) []string {
	for _, p := range providerRegistry {
		if p.ID() == id {
			return modelPickerOptions(p.ModelPicker())
		}
	}
	return nil
}

// indexOfModel returns the slice index whose label equals `model`,
// or 0 when no match. Mirrors indexOfRegisteredProvider for the
// model row.
func indexOfModel(opts []string, model string) int {
	for i, o := range opts {
		if o == model {
			return i
		}
	}
	return 0
}

// newPromptTextarea spins up a multi-line textarea seeded with the
// current step prompt. Layout is computed by the renderer; this
// helper just bakes the keymap (Enter inserts newline, Ctrl+S
// commits is handled by the screen, not the bubble) and the value.
func newPromptTextarea(seed string) textarea.Model {
	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.DynamicHeight = false
	ta.MinHeight = 6
	ta.SetHeight(12)
	ta.SetWidth(60)
	// Enter inserts a newline; Ctrl+S commits (handled by the
	// screen update). This matches the chat-input UX on
	// Shift+Enter — for a dedicated multiline editor it makes more
	// sense for plain Enter to be the newline.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("enter", "shift+enter", "ctrl+j"),
	)
	ta.SetValue(seed)
	ta.Focus()
	applyInputTheme(&ta)
	return ta
}

// ----- Sub-modal: provider picker -----

func (m model) workflowsBuilderUpdateProviderPicker(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		b.providerPicker = false
		return m, nil, true
	case msg.Code == tea.KeyUp:
		if b.providerCursor > 0 {
			b.providerCursor--
		}
		return m, nil, true
	case msg.Code == tea.KeyDown:
		if b.providerCursor < len(providerRegistry)-1 {
			b.providerCursor++
		}
		return m, nil, true
	case msg.Code == tea.KeyEnter:
		if b.providerCursor < 0 || b.providerCursor >= len(providerRegistry) {
			b.providerPicker = false
			return m, nil, true
		}
		newID := providerRegistry[b.providerCursor].ID()
		if b.selectedWorkflow >= 0 && b.selectedWorkflow < len(b.items) &&
			b.stepsCursor >= 0 && b.stepsCursor < len(b.items[b.selectedWorkflow].Steps) {
			step := &b.items[b.selectedWorkflow].Steps[b.stepsCursor]
			if step.Provider != newID {
				step.Provider = newID
				step.Model = "" // reset to provider default; user picks a fresh one if they want
			}
			if err := b.commitItems(); err != nil {
				b.toast = "save failed: " + err.Error()
			}
		}
		b.providerPicker = false
		return m, nil, true
	}
	return m, nil, true
}

// ----- Sub-modal: model picker -----

func (m model) workflowsBuilderUpdateModelPicker(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		b.modelPicker = false
		return m, nil, true
	case msg.Code == tea.KeyUp:
		if b.modelCursor > 0 {
			b.modelCursor--
		}
		return m, nil, true
	case msg.Code == tea.KeyDown:
		if b.modelCursor < len(b.modelPickerOpts)-1 {
			b.modelCursor++
		}
		return m, nil, true
	case msg.Code == tea.KeyEnter:
		if b.modelCursor < 0 || b.modelCursor >= len(b.modelPickerOpts) {
			b.modelPicker = false
			return m, nil, true
		}
		picked := b.modelPickerOpts[b.modelCursor]
		if b.selectedWorkflow >= 0 && b.selectedWorkflow < len(b.items) &&
			b.stepsCursor >= 0 && b.stepsCursor < len(b.items[b.selectedWorkflow].Steps) {
			b.items[b.selectedWorkflow].Steps[b.stepsCursor].Model = picked
			if err := b.commitItems(); err != nil {
				b.toast = "save failed: " + err.Error()
			}
		}
		b.modelPicker = false
		return m, nil, true
	}
	return m, nil, true
}

// ----- Sub-modal: rename (workflow or step) -----

func (m model) workflowsBuilderUpdateRename(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		b.renaming = ""
		b.renameDraft = ""
		return m, nil, true
	case msg.Code == tea.KeyEnter:
		draft := strings.TrimSpace(b.renameDraft)
		if draft == "" {
			b.toast = "name cannot be empty"
			return m, nil, true
		}
		switch b.renaming {
		case "workflow":
			idx := b.listCursor
			if b.level == workflowsLevelSteps || b.level == workflowsLevelStep {
				idx = b.selectedWorkflow
			}
			if idx < 0 || idx >= len(b.items) {
				b.renaming = ""
				return m, nil, true
			}
			// Reject name collisions to keep the picker / runtime
			// lookup stable.
			for i, w := range b.items {
				if i != idx && w.Name == draft {
					b.toast = "another workflow already uses that name"
					return m, nil, true
				}
			}
			b.items[idx].Name = draft
		case "step":
			if b.selectedWorkflow < 0 || b.selectedWorkflow >= len(b.items) {
				b.renaming = ""
				return m, nil, true
			}
			steps := b.items[b.selectedWorkflow].Steps
			if b.stepsCursor < 0 || b.stepsCursor >= len(steps) {
				b.renaming = ""
				return m, nil, true
			}
			for i, s := range steps {
				if i != b.stepsCursor && s.Name == draft {
					b.toast = "another step in this workflow already uses that name"
					return m, nil, true
				}
			}
			steps[b.stepsCursor].Name = draft
		}
		if err := b.commitItems(); err != nil {
			b.toast = "save failed: " + err.Error()
		}
		b.renaming = ""
		b.renameDraft = ""
		return m, nil, true
	case msg.Code == tea.KeyBackspace:
		if r := []rune(b.renameDraft); len(r) > 0 {
			b.renameDraft = string(r[:len(r)-1])
		}
		return m, nil, true
	}
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		b.renameDraft += msg.Text
		return m, nil, true
	}
	return m, nil, true
}

// ----- Sub-modal: prompt textarea -----

func (m model) workflowsBuilderUpdatePrompt(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	if b.prompt == nil {
		return m, nil, true
	}
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		b.prompt = nil
		return m, nil, true
	case msg.Mod == tea.ModCtrl && msg.Code == 's':
		// Commit the prompt to the current step.
		val := b.prompt.Value()
		if b.selectedWorkflow >= 0 && b.selectedWorkflow < len(b.items) &&
			b.stepsCursor >= 0 && b.stepsCursor < len(b.items[b.selectedWorkflow].Steps) {
			b.items[b.selectedWorkflow].Steps[b.stepsCursor].Prompt = val
			if err := b.commitItems(); err != nil {
				b.toast = "save failed: " + err.Error()
			}
		}
		b.prompt = nil
		return m, nil, true
	}
	upd, cmd := b.prompt.Update(msg)
	*b.prompt = upd
	return m, cmd, true
}

// ----- Sub-modal: confirm delete -----

func (m model) workflowsBuilderUpdateConfirm(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		b.confirming = ""
		b.confirmCursor = 0
		return m, nil, true
	case msg.Code == tea.KeyLeft:
		if b.confirmCursor > 0 {
			b.confirmCursor--
		}
		return m, nil, true
	case msg.Code == tea.KeyRight:
		if b.confirmCursor < 1 {
			b.confirmCursor++
		}
		return m, nil, true
	case msg.Code == tea.KeyTab:
		b.confirmCursor = 1 - b.confirmCursor
		return m, nil, true
	case msg.Code == tea.KeyEnter:
		if b.confirmCursor != 1 {
			b.confirming = ""
			return m, nil, true
		}
		switch b.confirming {
		case "delete-workflow":
			idx := b.listCursor
			if idx < 0 || idx >= len(b.items) {
				b.confirming = ""
				return m, nil, true
			}
			b.items = append(b.items[:idx], b.items[idx+1:]...)
		case "delete-step":
			if b.selectedWorkflow < 0 || b.selectedWorkflow >= len(b.items) {
				b.confirming = ""
				return m, nil, true
			}
			steps := b.items[b.selectedWorkflow].Steps
			if b.stepsCursor < 0 || b.stepsCursor >= len(steps) {
				b.confirming = ""
				return m, nil, true
			}
			b.items[b.selectedWorkflow].Steps = append(steps[:b.stepsCursor], steps[b.stepsCursor+1:]...)
		}
		if err := b.commitItems(); err != nil {
			b.toast = "save failed: " + err.Error()
		}
		b.confirming = ""
		b.confirmCursor = 0
		return m, nil, true
	}
	return m, nil, true
}

// ----- Render -----

func (b *workflowsBuilderState) render(width, height int) string {
	switch {
	case b.confirming != "":
		return b.renderConfirm(width, height)
	case b.renaming != "":
		return b.renderRename(width, height)
	case b.prompt != nil:
		return b.renderPromptEditor(width, height)
	case b.providerPicker:
		return b.renderProviderPicker(width, height)
	case b.modelPicker:
		return b.renderModelPicker(width, height)
	}
	switch b.level {
	case workflowsLevelList:
		return b.renderList(width, height)
	case workflowsLevelSteps:
		return b.renderSteps(width, height)
	case workflowsLevelStep:
		return b.renderStep(width, height)
	}
	return ""
}

func (b *workflowsBuilderState) renderList(width, height int) string {
	rows := make([]configItem, 0, len(b.items)+1)
	for _, w := range b.items {
		desc := fmt.Sprintf("%d step", len(w.Steps))
		if len(w.Steps) != 1 {
			desc = fmt.Sprintf("%d steps", len(w.Steps))
		}
		rows = append(rows, configItem{name: w.Name, key: desc})
	}
	rows = append(rows, configItem{name: "+ New workflow", key: ""})
	help := "↑/↓ navigate · enter open · r rename · d delete · esc back"
	if b.toast != "" {
		help = b.toast + " · " + help
		b.toast = ""
	}
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      width,
		height:     height,
		title:      "Workflows",
		promptLine: configPromptStyle.Render("> ") + dimStyle.Render("Select or create a workflow"),
		items:      rows,
		cursor:     b.listCursor,
		helpText:   help,
	})
}

func (b *workflowsBuilderState) renderSteps(width, height int) string {
	if b.selectedWorkflow < 0 || b.selectedWorkflow >= len(b.items) {
		return b.renderList(width, height)
	}
	wf := b.items[b.selectedWorkflow]
	rows := make([]configItem, 0, len(wf.Steps)+1)
	for _, s := range wf.Steps {
		desc := s.Provider
		if s.Model != "" {
			desc += " · " + s.Model
		}
		rows = append(rows, configItem{name: s.Name, key: desc})
	}
	rows = append(rows, configItem{name: "+ New step", key: ""})
	help := "↑/↓ navigate · enter edit · r rename workflow · d delete step · esc back"
	if b.toast != "" {
		help = b.toast + " · " + help
		b.toast = ""
	}
	title := "Workflow · " + wf.Name
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      width,
		height:     height,
		title:      title,
		promptLine: configPromptStyle.Render("> ") + dimStyle.Render(fmt.Sprintf("%d step(s) — runs sequentially", len(wf.Steps))),
		items:      rows,
		cursor:     b.stepsCursor,
		helpText:   help,
	})
}

func (b *workflowsBuilderState) renderStep(width, height int) string {
	if b.selectedWorkflow < 0 || b.selectedWorkflow >= len(b.items) {
		return b.renderList(width, height)
	}
	steps := b.items[b.selectedWorkflow].Steps
	if b.stepsCursor < 0 || b.stepsCursor >= len(steps) {
		return b.renderSteps(width, height)
	}
	step := steps[b.stepsCursor]
	promptPreview := step.Prompt
	if len(promptPreview) > 50 {
		promptPreview = promptPreview[:47] + "…"
	}
	if promptPreview == "" {
		promptPreview = "(empty)"
	}
	promptPreview = strings.ReplaceAll(promptPreview, "\n", " ⏎ ")
	provDisplay := step.Provider
	if provDisplay == "" {
		provDisplay = "(none)"
	}
	modelDisplay := step.Model
	if modelDisplay == "" {
		modelDisplay = "(provider default)"
	}
	rows := []configItem{
		{name: "Name", key: step.Name},
		{name: "Provider", key: provDisplay},
		{name: "Model", key: modelDisplay},
		{name: "Prompt", key: promptPreview},
	}
	help := "↑/↓ navigate · enter edit · esc back"
	if b.toast != "" {
		help = b.toast + " · " + help
		b.toast = ""
	}
	title := fmt.Sprintf("Step · %s", step.Name)
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      width,
		height:     height,
		title:      title,
		promptLine: configPromptStyle.Render("> ") + dimStyle.Render(b.items[b.selectedWorkflow].Name),
		items:      rows,
		cursor:     int(b.stepCursor),
		helpText:   help,
	})
}

func (b *workflowsBuilderState) renderProviderPicker(width, height int) string {
	rows := make([]configItem, 0, len(providerRegistry))
	for _, p := range providerRegistry {
		rows = append(rows, configItem{name: p.DisplayName(), key: p.ID()})
	}
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      width,
		height:     height,
		title:      "Step Provider",
		promptLine: configPromptStyle.Render("> ") + dimStyle.Render("Pick the agent CLI for this step"),
		items:      rows,
		cursor:     b.providerCursor,
		helpText:   "↑/↓ navigate · enter pick · esc cancel",
	})
}

func (b *workflowsBuilderState) renderModelPicker(width, height int) string {
	rows := make([]configItem, 0, len(b.modelPickerOpts))
	for _, o := range b.modelPickerOpts {
		rows = append(rows, configItem{name: o, key: ""})
	}
	if len(rows) == 0 {
		rows = append(rows, configItem{name: "(no model picker for this provider)", key: ""})
	}
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      width,
		height:     height,
		title:      "Step Model",
		promptLine: configPromptStyle.Render("> ") + dimStyle.Render("Pick the model for this step"),
		items:      rows,
		cursor:     b.modelCursor,
		helpText:   "↑/↓ navigate · enter pick · esc cancel",
	})
}

func (b *workflowsBuilderState) renderRename(width, height int) string {
	title := "Rename"
	hint := "Type a new name; enter to save, esc to cancel"
	if b.renaming == "step" {
		title = "Rename step"
	} else if b.renaming == "workflow" {
		title = "Rename workflow"
	}
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      width,
		height:     height,
		title:      title,
		promptLine: filterPromptLine(b.renameDraft, hint),
		items:      nil,
		cursor:     0,
		helpText:   "enter save · esc cancel",
	})
}

func (b *workflowsBuilderState) renderPromptEditor(width, height int) string {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(activeTheme.accent).
		Padding(1, 2)
	title := configTitleStyle.Render("Step prompt")
	hint := dimStyle.Render("ctrl+s save · esc cancel · enter newline")
	w := width - 8
	if w < 40 {
		w = 40
	}
	if w > 100 {
		w = 100
	}
	b.prompt.SetWidth(w)
	body := strings.Join([]string{
		title,
		"",
		b.prompt.View(),
		"",
		hint,
	}, "\n")
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box.Render(body))
}

func (b *workflowsBuilderState) renderConfirm(width, height int) string {
	target := "this entry"
	switch b.confirming {
	case "delete-workflow":
		if b.listCursor >= 0 && b.listCursor < len(b.items) {
			target = "workflow \"" + b.items[b.listCursor].Name + "\""
		}
	case "delete-step":
		if b.selectedWorkflow >= 0 && b.selectedWorkflow < len(b.items) &&
			b.stepsCursor >= 0 && b.stepsCursor < len(b.items[b.selectedWorkflow].Steps) {
			target = "step \"" + b.items[b.selectedWorkflow].Steps[b.stepsCursor].Name + "\""
		}
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(activeTheme.warn).
		Padding(1, 2)
	body := fmt.Sprintf("Delete %s?", target)
	cancelLabel := "  Cancel  "
	deleteLabel := "  Delete  "
	if b.confirmCursor == 0 {
		cancelLabel = themePickerRowStyle.Render("▸ Cancel  ")
	} else {
		deleteLabel = lipgloss.NewStyle().Background(activeTheme.warn).Foreground(activeTheme.darkFG).Render("▸ Delete  ")
	}
	rendered := strings.Join([]string{
		body,
		"",
		cancelLabel + "    " + deleteLabel,
		"",
		dimStyle.Render("←/→/tab choose · enter confirm · esc cancel"),
	}, "\n")
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box.Render(rendered))
}
