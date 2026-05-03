package main

import (
	"fmt"
	"image"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	xansi "github.com/charmbracelet/x/ansi"
)

// workflowsBuilderFocus names which pane currently owns the cursor.
// Tab swaps between the two; Enter on the left auto-shifts to right.
type workflowsBuilderFocus int

const (
	workflowsBuilderFocusLeft workflowsBuilderFocus = iota
	workflowsBuilderFocusRight
)

// workflowsBuilderRightMode names what the right pane is rendering.
// Driven by the left cursor (cursor on a workflow → steps mode;
// cursor on the +New row → empty mode) plus an explicit promotion
// to step mode when the user opens a step from the right pane.
type workflowsBuilderRightMode int

const (
	workflowsBuilderRightEmpty workflowsBuilderRightMode = iota
	workflowsBuilderRightSteps
	workflowsBuilderRightStep
)

// workflowsStepField is the focused field on the step-detail pane.
type workflowsStepField int

const (
	workflowsStepFieldName workflowsStepField = iota
	workflowsStepFieldProvider
	workflowsStepFieldModel
	workflowsStepFieldPrompt
)

// workflowsBuilderState bundles every per-tab editor surface so the
// model.struct stays compact. The data model is fully reactive: the
// left cursor drives the right pane's content. Disk writes happen
// on every commit so "back" is never a save action — exit is just
// navigation.
type workflowsBuilderState struct {
	// cwd is the project root used for loadConfig / saveConfig.
	// Captured on screen entry and never changes for the lifetime
	// of the builder; the builder is bound to one project at a time.
	cwd string

	// items is the local snapshot of cfg.Projects[cwd].Workflows.Items.
	// Refreshed from disk on screen entry and after every commit so
	// the screen always reflects persisted state.
	items []workflowDef

	// focus is the active pane.
	focus workflowsBuilderFocus

	// rightMode is what the right pane renders. Empty/steps is
	// derived from the left cursor; step requires an explicit Enter
	// from steps mode.
	rightMode workflowsBuilderRightMode

	// listCursor is the row cursor on the left pane. Row 0 is the
	// "+ New workflow" affordance; rows 1..len(items) are the
	// workflows in disk order.
	listCursor int

	// stepsCursor is the row cursor on the right(steps) pane. Row 0
	// is the "+ New step" affordance; rows 1..len(steps) are the
	// steps of the selected workflow.
	stepsCursor int

	// stepFieldCursor is the focused field on the right(step) pane.
	stepFieldCursor workflowsStepField

	// renaming is "workflow" / "step" / "" — the inline rename
	// editor's mode flag. While non-empty, keys feed renameDraft.
	renaming    string
	renameDraft string

	// providerPicker / modelPicker flag the small overlays for
	// step.Provider and step.Model selection.
	providerPicker  bool
	providerCursor  int
	modelPicker     bool
	modelCursor     int
	modelPickerOpts []string

	// prompt is the in-flight multi-line textarea editor for the
	// step prompt. Non-nil while open; Enter inserts a newline,
	// Ctrl+S commits, Esc cancels.
	prompt *textarea.Model

	// confirming is "delete-workflow" / "delete-step" / "" — the
	// destructive confirm overlay's mode flag.
	confirming    string
	confirmCursor int

	// toast carries a short status line shown on the next render.
	toast string
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
	// Overlay precedence: confirm > rename > prompt > pickers > pane focus.
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
	switch b.focus {
	case workflowsBuilderFocusLeft:
		return m.workflowsBuilderUpdateLeft(msg)
	case workflowsBuilderFocusRight:
		switch b.rightMode {
		case workflowsBuilderRightSteps:
			return m.workflowsBuilderUpdateRightSteps(msg)
		case workflowsBuilderRightStep:
			return m.workflowsBuilderUpdateRightStep(msg)
		default:
			// rightMode==empty + focus==right shouldn't normally
			// happen (left is the only entry point that promotes
			// rightMode), but the defensive fall-through to the
			// left handler keeps the screen recoverable.
			b.focus = workflowsBuilderFocusLeft
			return m.workflowsBuilderUpdateLeft(msg)
		}
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
// hydrating the local items snapshot from disk and parking the
// cursor on "+ New workflow" for first-time visitors.
func newWorkflowsBuilderState(cwd string) *workflowsBuilderState {
	b := &workflowsBuilderState{cwd: cwd, focus: workflowsBuilderFocusLeft}
	b.refreshItems()
	b.syncRightFromLeft()
	return b
}

// refreshItems re-reads the workflow list from disk and clamps every
// cursor to the new shape. Called on screen entry and after every
// commit.
func (b *workflowsBuilderState) refreshItems() {
	b.items = projectWorkflows(b.cwd)
	maxList := len(b.items) // valid range: 0 (+ New) .. len(items)
	if b.listCursor > maxList {
		b.listCursor = maxList
	}
	if b.listCursor < 0 {
		b.listCursor = 0
	}
	if idx, ok := b.selectedWorkflowIdx(); ok {
		steps := b.items[idx].Steps
		maxSteps := len(steps) // valid range: 0 (+ New step) .. len(steps)
		if b.stepsCursor > maxSteps {
			b.stepsCursor = maxSteps
		}
	}
}

// syncRightFromLeft is the reactive sync: the right pane's mode
// follows the left cursor whenever the user navigates. Cursor on
// "+ New workflow" → empty pane. Cursor on a workflow → steps pane,
// stepsCursor reset to the +New step row.
//
// Called from updateLeft after every cursor mutation so the user's
// glance at the right pane is always accurate to where they are.
func (b *workflowsBuilderState) syncRightFromLeft() {
	if _, ok := b.selectedWorkflowIdx(); !ok {
		b.rightMode = workflowsBuilderRightEmpty
		return
	}
	b.rightMode = workflowsBuilderRightSteps
	b.stepsCursor = 0
}

// selectedWorkflowIdx maps the left cursor to a workflow index.
// Returns ok=false when the cursor is on the "+ New workflow" row
// (no workflow selected).
func (b *workflowsBuilderState) selectedWorkflowIdx() (int, bool) {
	if b.listCursor <= 0 || b.listCursor > len(b.items) {
		return 0, false
	}
	return b.listCursor - 1, true
}

// selectedStepIdx maps the right(steps) cursor to a step index for
// the selected workflow. ok=false when the cursor is on "+ New step"
// or no workflow is selected.
func (b *workflowsBuilderState) selectedStepIdx() (workflowIdx, stepIdx int, ok bool) {
	wIdx, hasWorkflow := b.selectedWorkflowIdx()
	if !hasWorkflow {
		return 0, 0, false
	}
	steps := b.items[wIdx].Steps
	if b.stepsCursor <= 0 || b.stepsCursor > len(steps) {
		return wIdx, 0, false
	}
	return wIdx, b.stepsCursor - 1, true
}

// commitItems writes b.items back to disk and re-hydrates so any
// normalisation the persistence layer applies is reflected locally.
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

// uniqueWorkflowName returns a name not used by any workflow in
// b.items. Used by "+ New workflow" so the list never has two rows
// that collide on Name (the runtime / picker key on Name).
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

// uniqueStepName returns a step name not used inside the selected
// workflow.
func (b *workflowsBuilderState) uniqueStepName(seed string) string {
	if seed == "" {
		seed = "step"
	}
	wIdx, ok := b.selectedWorkflowIdx()
	if !ok {
		return seed
	}
	taken := make(map[string]struct{})
	for _, s := range b.items[wIdx].Steps {
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

// runningGuard returns the toast string when destructive edits are
// blocked because the workflow under the left cursor is running.
// Empty when no guard applies.
func (b *workflowsBuilderState) runningGuard() string {
	wIdx, ok := b.selectedWorkflowIdx()
	if !ok {
		return ""
	}
	name := b.items[wIdx].Name
	active := workflowTracker().activeWorkflowNames()
	if _, running := active[name]; running {
		return "blocked: workflow is running"
	}
	return ""
}

// ----- Left pane (workflow list) -----

func (m model) workflowsBuilderUpdateLeft(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m.workflowsBuilder = nil
		return m.switchScreen(screenAsk), nil, true
	case msg.Code == tea.KeyTab:
		// Tab → right (only if right has content; otherwise no-op).
		if b.rightMode != workflowsBuilderRightEmpty {
			b.focus = workflowsBuilderFocusRight
		}
		return m, nil, true
	case msg.Code == tea.KeyUp:
		if b.listCursor > 0 {
			b.listCursor--
			b.syncRightFromLeft()
		}
		return m, nil, true
	case msg.Code == tea.KeyDown:
		if b.listCursor < len(b.items) {
			b.listCursor++
			b.syncRightFromLeft()
		}
		return m, nil, true
	case msg.Code == tea.KeyEnter:
		if b.listCursor == 0 {
			// "+ New workflow" → create + drill into the new entry.
			b.items = append(b.items, workflowDef{Name: b.uniqueWorkflowName("untitled")})
			if err := b.commitItems(); err != nil {
				b.toast = "save failed: " + err.Error()
				return m, nil, true
			}
			// New row goes at the end; jump there.
			b.listCursor = len(b.items)
			b.stepsCursor = 0
			b.rightMode = workflowsBuilderRightSteps
			b.focus = workflowsBuilderFocusRight
			return m, nil, true
		}
		// Enter on an existing workflow → focus shifts right;
		// rightMode is already steps via syncRightFromLeft.
		b.focus = workflowsBuilderFocusRight
		b.rightMode = workflowsBuilderRightSteps
		return m, nil, true
	case msg.Mod == 0 && msg.Code == 'r':
		_, ok := b.selectedWorkflowIdx()
		if !ok {
			return m, nil, true
		}
		if guard := b.runningGuard(); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		b.renaming = "workflow"
		b.renameDraft = b.items[b.listCursor-1].Name
		return m, nil, true
	case msg.Mod == 0 && msg.Code == 'd':
		_, ok := b.selectedWorkflowIdx()
		if !ok {
			return m, nil, true
		}
		if guard := b.runningGuard(); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		b.confirming = "delete-workflow"
		b.confirmCursor = 0
		return m, nil, true
	}
	return m, nil, true
}

// ----- Right(steps) pane -----

func (m model) workflowsBuilderUpdateRightSteps(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	wIdx, ok := b.selectedWorkflowIdx()
	if !ok {
		// No workflow selected — nothing to render here. Bounce
		// focus back to left.
		b.focus = workflowsBuilderFocusLeft
		return m, nil, true
	}
	steps := b.items[wIdx].Steps
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		// Esc on right(steps) returns focus to left.
		b.focus = workflowsBuilderFocusLeft
		return m, nil, true
	case msg.Code == tea.KeyTab:
		b.focus = workflowsBuilderFocusLeft
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
		if b.stepsCursor == 0 {
			// "+ New step" → create + drop into step details.
			if guard := b.runningGuard(); guard != "" {
				b.toast = guard
				return m, nil, true
			}
			defaultProvider := ""
			if len(providerRegistry) > 0 {
				defaultProvider = providerRegistry[0].ID()
			}
			b.items[wIdx].Steps = append(steps, workflowStep{
				Name:     b.uniqueStepName("step"),
				Provider: defaultProvider,
			})
			if err := b.commitItems(); err != nil {
				b.toast = "save failed: " + err.Error()
				return m, nil, true
			}
			// Jump cursor onto the new step.
			b.stepsCursor = len(b.items[wIdx].Steps)
			b.rightMode = workflowsBuilderRightStep
			b.stepFieldCursor = workflowsStepFieldName
			return m, nil, true
		}
		// Enter on an existing step → step details.
		b.rightMode = workflowsBuilderRightStep
		b.stepFieldCursor = workflowsStepFieldName
		return m, nil, true
	case msg.Mod == 0 && msg.Code == 'd':
		if b.stepsCursor == 0 {
			return m, nil, true
		}
		if guard := b.runningGuard(); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		b.confirming = "delete-step"
		b.confirmCursor = 0
		return m, nil, true
	}
	return m, nil, true
}

// ----- Right(step) pane -----

func (m model) workflowsBuilderUpdateRightStep(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	b := m.workflowsBuilder
	wIdx, sIdx, ok := b.selectedStepIdx()
	if !ok {
		b.rightMode = workflowsBuilderRightSteps
		return m, nil, true
	}
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		// Esc on right(step) pops back to right(steps), keeps focus.
		b.rightMode = workflowsBuilderRightSteps
		return m, nil, true
	case msg.Code == tea.KeyTab:
		// Tab on right(step) bounces focus to left, keeps the step
		// pane state so a later Tab back returns the user to the
		// same field.
		b.focus = workflowsBuilderFocusLeft
		return m, nil, true
	case msg.Code == tea.KeyUp:
		if b.stepFieldCursor > 0 {
			b.stepFieldCursor--
		}
		return m, nil, true
	case msg.Code == tea.KeyDown:
		if b.stepFieldCursor < workflowsStepFieldPrompt {
			b.stepFieldCursor++
		}
		return m, nil, true
	case msg.Code == tea.KeyEnter:
		if guard := b.runningGuard(); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		step := b.items[wIdx].Steps[sIdx]
		switch b.stepFieldCursor {
		case workflowsStepFieldName:
			b.renaming = "step"
			b.renameDraft = step.Name
		case workflowsStepFieldProvider:
			b.providerPicker = true
			b.providerCursor = indexOfRegisteredProvider(step.Provider)
		case workflowsStepFieldModel:
			b.modelPickerOpts = modelOptionsForProvider(step.Provider)
			b.modelPicker = true
			b.modelCursor = indexOfModel(b.modelPickerOpts, step.Model)
		case workflowsStepFieldPrompt:
			ta := newPromptTextarea(step.Prompt)
			b.prompt = &ta
		}
		return m, nil, true
	}
	if b.stepFieldCursor == workflowsStepFieldName && msg.Mod&^tea.ModShift == 0 {
		if guard := b.runningGuard(); guard != "" {
			b.toast = guard
			return m, nil, true
		}
		switch {
		case msg.Text != "":
			b.renaming = "step"
			b.renameDraft = msg.Text
			return m, nil, true
		case msg.Code == tea.KeyBackspace:
			r := []rune(b.items[wIdx].Steps[sIdx].Name)
			if len(r) > 0 {
				r = r[:len(r)-1]
			}
			b.renaming = "step"
			b.renameDraft = string(r)
			return m, nil, true
		}
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
// or 0 when no match.
func indexOfModel(opts []string, model string) int {
	for i, o := range opts {
		if o == model {
			return i
		}
	}
	return 0
}

// newPromptTextarea spins up a multi-line textarea seeded with the
// current step prompt.
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
	// screen update). For a dedicated multiline editor it makes
	// more sense for plain Enter to be the newline.
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
		if wIdx, sIdx, ok := b.selectedStepIdx(); ok {
			step := &b.items[wIdx].Steps[sIdx]
			if step.Provider != newID {
				step.Provider = newID
				step.Model = "" // reset to provider default; user picks fresh
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
		if wIdx, sIdx, ok := b.selectedStepIdx(); ok {
			b.items[wIdx].Steps[sIdx].Model = picked
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
			wIdx, ok := b.selectedWorkflowIdx()
			if !ok {
				b.renaming = ""
				return m, nil, true
			}
			for i, w := range b.items {
				if i != wIdx && w.Name == draft {
					b.toast = "another workflow already uses that name"
					return m, nil, true
				}
			}
			b.items[wIdx].Name = draft
		case "step":
			wIdx, sIdx, ok := b.selectedStepIdx()
			if !ok {
				b.renaming = ""
				return m, nil, true
			}
			steps := b.items[wIdx].Steps
			for i, s := range steps {
				if i != sIdx && s.Name == draft {
					b.toast = "another step in this workflow already uses that name"
					return m, nil, true
				}
			}
			steps[sIdx].Name = draft
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
		val := b.prompt.Value()
		if wIdx, sIdx, ok := b.selectedStepIdx(); ok {
			b.items[wIdx].Steps[sIdx].Prompt = val
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
			wIdx, ok := b.selectedWorkflowIdx()
			if !ok {
				b.confirming = ""
				return m, nil, true
			}
			b.items = append(b.items[:wIdx], b.items[wIdx+1:]...)
			// Clamp cursor and resync the right pane to whatever
			// is now under it.
			if b.listCursor > len(b.items) {
				b.listCursor = len(b.items)
			}
		case "delete-step":
			wIdx, sIdx, ok := b.selectedStepIdx()
			if !ok {
				b.confirming = ""
				return m, nil, true
			}
			steps := b.items[wIdx].Steps
			b.items[wIdx].Steps = append(steps[:sIdx], steps[sIdx+1:]...)
			if b.stepsCursor > len(b.items[wIdx].Steps) {
				b.stepsCursor = len(b.items[wIdx].Steps)
			}
			b.rightMode = workflowsBuilderRightSteps
		}
		if err := b.commitItems(); err != nil {
			b.toast = "save failed: " + err.Error()
		}
		// Re-sync right after refresh in case the deleted entry
		// changed which workflow the cursor sits on.
		if b.confirming == "delete-workflow" {
			b.syncRightFromLeft()
		}
		b.confirming = ""
		b.confirmCursor = 0
		return m, nil, true
	}
	return m, nil, true
}

// ----- Render (split-screen) -----

// workflowsLeftPaneMinWidth / MaxWidth bound the left pane so a wide
// terminal doesn't let the workflows list sprawl too far, while a
// narrow terminal still degrades without forcing the layout past the
// actual terminal size.
const (
	workflowsLeftPaneMinWidth  = 28
	workflowsLeftPaneMaxWidth  = 44
	workflowsRightPaneMinWidth = 32
	workflowsPanePadX          = 2
	// workflowsScreenMargin is the 1-cell empty frame around the
	// split-screen body — top, bottom, left, right. Matches the
	// chrome the issues screen uses so the two surfaces feel
	// consistent.
	workflowsScreenMargin = 1
)

type workflowsScreenLayout struct {
	paneHeight int
	leftWidth  int
	rightWidth int
	hintWidth  int
}

type workflowStepColumns struct {
	Name     int
	Provider int
	Model    int
}

func (b *workflowsBuilderState) render(width, height int) string {
	base := b.renderBase(width, height)
	overlay := b.renderOverlay(width, height)
	if overlay == "" || width <= 0 || height <= 0 {
		return base
	}
	canvas := uv.NewScreenBuffer(width, height)
	uv.NewStyledString(base).Draw(canvas, image.Rectangle{
		Min: image.Pt(0, 0),
		Max: image.Pt(width, height),
	})
	oW := lipgloss.Width(overlay)
	oH := lipgloss.Height(overlay)
	oX := (width - oW) / 2
	oY := (height - oH) / 2
	if oX < 0 {
		oX = 0
	}
	if oY < 0 {
		oY = 0
	}
	uv.NewStyledString(overlay).Draw(canvas, image.Rectangle{
		Min: image.Pt(oX, oY),
		Max: image.Pt(oX+oW, oY+oH),
	})
	return canvas.Render()
}

func (b *workflowsBuilderState) renderOverlay(width, height int) string {
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
	return ""
}

func (b *workflowsBuilderState) renderBase(width, height int) string {
	layout := newWorkflowsScreenLayout(width, height)
	left := b.renderLeftPane(layout.leftWidth, layout.paneHeight)
	right := b.renderRightPane(layout.rightWidth, layout.paneHeight)
	boxes := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	boxes = indentLines(boxes, workflowsScreenMargin)

	hint := b.activeHint()
	if t := b.consumeToast(); t != "" {
		hint = t + " · " + hint
	}
	hintLine := strings.Repeat(" ", workflowsScreenMargin) +
		configHelpStyle.Render(truncateForRow(hint, layout.hintWidth))

	var sb strings.Builder
	sb.WriteString("\n") // top margin
	sb.WriteString(boxes)
	sb.WriteString("\n\n") // gap between boxes and hint
	sb.WriteString(hintLine)
	sb.WriteString("\n") // bottom margin
	return sb.String()
}

// activeHint returns the help text shown on the screen-level hint
// row at the bottom. Pulled from the focused pane so the user sees
// keys relevant to where they are. The hint sits outside the box
// chrome (per UX request) so a long string never wraps inside a
// narrow pane.
func (b *workflowsBuilderState) activeHint() string {
	if b.focus == workflowsBuilderFocusLeft {
		return "↑/↓ navigate · enter open · r rename · d delete · esc back"
	}
	switch b.rightMode {
	case workflowsBuilderRightSteps:
		return "↑/↓ navigate · enter edit · d delete · tab focus left · esc back"
	case workflowsBuilderRightStep:
		return "↑/↓ navigate · enter edit · esc back · tab focus left"
	}
	return "tab focus left"
}

// renderLeftPane draws the workflow list. "+ New workflow" is row 0
// so the create affordance is always discoverable at the top. Rows
// carry only the workflow name — step counts moved to the right
// pane's subtitle so the left pane stays narrow without wrapping
// names against trailing metadata.
func (b *workflowsBuilderState) renderLeftPane(width, height int) string {
	innerW := workflowsPaneInnerWidth(width)
	rows := []string{
		renderWorkflowsListRow("+ New workflow", innerW, b.listCursor == 0, b.focus == workflowsBuilderFocusLeft),
	}
	for _, w := range b.items {
		rows = append(rows, renderWorkflowsListRow(w.Name, innerW, len(rows) == b.listCursor, b.focus == workflowsBuilderFocusLeft))
	}
	return renderWorkflowsPane(workflowsPaneArgs{
		width:    width,
		height:   height,
		title:    "Workflows",
		subtitle: "Select or create a workflow",
		rows:     rows,
		cursor:   b.listCursor,
		active:   b.focus == workflowsBuilderFocusLeft,
	})
}

// renderRightPane draws the right side: empty placeholder, steps
// list, or step details depending on rightMode + listCursor.
func (b *workflowsBuilderState) renderRightPane(width, height int) string {
	switch b.rightMode {
	case workflowsBuilderRightEmpty:
		return b.renderRightEmpty(width, height)
	case workflowsBuilderRightSteps:
		return b.renderRightSteps(width, height)
	case workflowsBuilderRightStep:
		return b.renderRightStep(width, height)
	}
	return ""
}

func (b *workflowsBuilderState) renderRightEmpty(width, height int) string {
	innerW := workflowsPaneInnerWidth(width)
	return renderWorkflowsPane(workflowsPaneArgs{
		width:    width,
		height:   height,
		title:    "Steps",
		subtitle: "Pick a workflow on the left to view its steps",
		rows:     []string{strings.Repeat(" ", innerW)},
		cursor:   0,
		active:   b.focus == workflowsBuilderFocusRight,
	})
}

func (b *workflowsBuilderState) renderRightSteps(width, height int) string {
	wIdx, ok := b.selectedWorkflowIdx()
	if !ok {
		return b.renderRightEmpty(width, height)
	}
	wf := b.items[wIdx]
	innerW := workflowsPaneInnerWidth(width)
	cols := computeWorkflowStepColumns(innerW)
	rows := []string{
		renderWorkflowsListRow("+ New step", innerW, b.stepsCursor == 0, b.focus == workflowsBuilderFocusRight),
	}
	for _, s := range wf.Steps {
		rows = append(rows, renderWorkflowStepRow(
			s,
			cols,
			innerW,
			len(rows) == b.stepsCursor,
			b.focus == workflowsBuilderFocusRight,
		))
	}
	return renderWorkflowsPane(workflowsPaneArgs{
		width:    width,
		height:   height,
		title:    "Workflow · " + wf.Name,
		subtitle: fmt.Sprintf("%s — runs sequentially", stepsCount(len(wf.Steps))),
		header:   renderWorkflowStepHeader(cols, innerW),
		rows:     rows,
		cursor:   b.stepsCursor,
		active:   b.focus == workflowsBuilderFocusRight,
	})
}

func (b *workflowsBuilderState) renderRightStep(width, height int) string {
	wIdx, sIdx, ok := b.selectedStepIdx()
	if !ok {
		return b.renderRightSteps(width, height)
	}
	step := b.items[wIdx].Steps[sIdx]
	promptPreview := step.Prompt
	if len(promptPreview) > 60 {
		promptPreview = promptPreview[:57] + "…"
	}
	rows := []string{
		renderWorkflowDetailRow("Name", step.Name, workflowsPaneInnerWidth(width), int(b.stepFieldCursor) == 0, b.focus == workflowsBuilderFocusRight),
		renderWorkflowDetailRow("Provider", workflowProviderDisplay(step.Provider), workflowsPaneInnerWidth(width), int(b.stepFieldCursor) == 1, b.focus == workflowsBuilderFocusRight),
		renderWorkflowDetailRow("Model", workflowModelDisplay(step.Model), workflowsPaneInnerWidth(width), int(b.stepFieldCursor) == 2, b.focus == workflowsBuilderFocusRight),
		renderWorkflowDetailRow("Prompt", workflowPromptPreview(promptPreview), workflowsPaneInnerWidth(width), int(b.stepFieldCursor) == 3, b.focus == workflowsBuilderFocusRight),
	}
	return renderWorkflowsPane(workflowsPaneArgs{
		width:    width,
		height:   height,
		title:    "Step · " + step.Name,
		subtitle: b.items[wIdx].Name,
		rows:     rows,
		cursor:   int(b.stepFieldCursor),
		active:   b.focus == workflowsBuilderFocusRight,
	})
}

// consumeToast clears b.toast on read so it shows for one render
// cycle. Helps the user see the warning without it sticking around
// stale.
func (b *workflowsBuilderState) consumeToast() string {
	t := b.toast
	b.toast = ""
	return t
}

// workflowsPaneArgs is the shape of one pane in the split-screen
// builder. Centralising the chrome means the two panes stay
// pixel-aligned without each renderer eyeballing widths.
//
// Help text is NOT carried here — the screen-level renderer draws
// a single hint row outside both boxes so a narrow pane can never
// push it onto a second line and bleed into the bottom margin.
type workflowsPaneArgs struct {
	width, height int
	title         string
	subtitle      string
	header        string
	rows          []string
	cursor        int
	active        bool
}

// renderWorkflowsPane renders a single side of the split-screen
// builder. The active pane gets an accent border so the user can
// tell at a glance which side has focus; the inactive pane uses
// the dim border style.
//
// Vertical padding inside the box is zero — the only empty space
// above the title comes from the outer margin applied by the screen
// renderer. Horizontal padding stays so titles / rows breathe
// inside the border.
//
// The body is built to exactly fill the box's content area: a fixed
// 4-line chrome (title + blank + subtitle + blank) plus listH rows
// sums to innerH. No hint inside the box — the screen-level
// renderer draws a single hint row underneath.
func renderWorkflowsPane(a workflowsPaneArgs) string {
	borderColor := activeTheme.dim
	if a.active {
		borderColor = activeTheme.accent
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, workflowsPanePadX)

	innerW := workflowsPaneInnerWidth(a.width)
	innerH := workflowsPaneInnerHeight(a.height)

	title := padRight(configTitleStyle.Render(truncateForRow(a.title, innerW)), innerW)
	subtitlePrefix := configPromptStyle.Render("> ")
	subtitleW := innerW - lipgloss.Width(subtitlePrefix)
	if subtitleW < 1 {
		subtitleW = 1
	}
	subtitle := padRight(subtitlePrefix+dimStyle.Render(truncateForRow(a.subtitle, subtitleW)), innerW)

	lines := make([]string, 0, innerH)
	appendLine := func(line string) {
		if len(lines) >= innerH {
			return
		}
		lines = append(lines, padRight(line, innerW))
	}

	appendLine(title)
	appendLine("")
	appendLine(subtitle)
	appendLine("")
	if a.header != "" {
		appendLine(a.header)
	}
	for _, row := range visibleWorkflowRows(a.rows, a.cursor, innerH-len(lines)) {
		appendLine(row)
	}
	for len(lines) < innerH {
		lines = append(lines, strings.Repeat(" ", innerW))
	}
	return box.Render(strings.Join(lines, "\n"))
}

// truncateForRow caps `s` to at most `max` cells, appending a "…"
// when truncation actually trims the string. Falls back to the raw
// rune-cut when xansi.Truncate would produce an empty result on
// max=1 (the ellipsis won't fit).
func truncateForRow(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= max {
		return s
	}
	if max == 1 {
		// Single cell: just show the first cell so the user has a
		// hint something was there. xansi.Truncate("xyz", 1, "…")
		// returns just "…" which is fine but we keep the literal
		// for consistency with the cell budget.
		runes := []rune(s)
		if len(runes) == 0 {
			return ""
		}
		return string(runes[:1])
	}
	return xansi.Truncate(s, max, "…")
}

func newWorkflowsScreenLayout(width, height int) workflowsScreenLayout {
	outerW := width - 2*workflowsScreenMargin
	if outerW < 2 {
		outerW = 2
	}
	paneH := height - 4
	if paneH < 1 {
		paneH = 1
	}
	leftW, rightW := splitWorkflowsPaneWidths(outerW)
	return workflowsScreenLayout{
		paneHeight: paneH,
		leftWidth:  leftW,
		rightWidth: rightW,
		hintWidth:  outerW,
	}
}

func splitWorkflowsPaneWidths(total int) (left, right int) {
	if total <= 2 {
		return 1, max(1, total-1)
	}
	if total >= workflowsLeftPaneMinWidth+workflowsRightPaneMinWidth {
		left = total / 3
		if left < workflowsLeftPaneMinWidth {
			left = workflowsLeftPaneMinWidth
		}
		if left > workflowsLeftPaneMaxWidth {
			left = workflowsLeftPaneMaxWidth
		}
		right = total - left
		if right < workflowsRightPaneMinWidth {
			right = workflowsRightPaneMinWidth
			left = total - right
		}
		return left, right
	}
	left = total / 2
	if left > workflowsLeftPaneMaxWidth {
		left = workflowsLeftPaneMaxWidth
	}
	if left < 1 {
		left = 1
	}
	right = total - left
	if right < 1 {
		right = 1
		left = total - right
	}
	return left, right
}

func workflowsPaneInnerWidth(total int) int {
	inner := total - 2 - 2*workflowsPanePadX
	if inner < 1 {
		return 1
	}
	return inner
}

func workflowsPaneInnerHeight(total int) int {
	inner := total - 2
	if inner < 1 {
		return 1
	}
	return inner
}

func visibleWorkflowRows(rows []string, cursor, height int) []string {
	if height <= 0 || len(rows) == 0 {
		return nil
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(rows) {
		cursor = len(rows) - 1
	}
	start := 0
	if cursor >= height {
		start = cursor - height + 1
	}
	end := start + height
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end]
}

func renderWorkflowsListRow(label string, width int, selected, activePane bool) string {
	line := padRight(truncateForRow(label, width), width)
	if !selected {
		return line
	}
	if activePane {
		return configSelectedRowStyle.Render(line)
	}
	return dimStyle.Render(line)
}

func computeWorkflowStepColumns(width int) workflowStepColumns {
	if width < 7 {
		return workflowStepColumns{Name: max(1, width)}
	}
	providerW := 10
	modelW := 14
	if width < providerW+modelW+10 {
		providerW = max(4, width/4)
		modelW = max(6, width/3)
	}
	nameW := width - providerW - modelW - 2
	if nameW < 8 {
		deficit := 8 - nameW
		shrinkModel := min(deficit, max(0, modelW-6))
		modelW -= shrinkModel
		deficit -= shrinkModel
		shrinkProvider := min(deficit, max(0, providerW-4))
		providerW -= shrinkProvider
		nameW = width - providerW - modelW - 2
	}
	if nameW < 1 {
		nameW = 1
	}
	return workflowStepColumns{
		Name:     nameW,
		Provider: providerW,
		Model:    max(0, width-nameW-providerW-2),
	}
}

func renderWorkflowStepHeader(cols workflowStepColumns, width int) string {
	line := strings.Join([]string{
		padRight(truncateForRow("Name", cols.Name), cols.Name),
		padRight(truncateForRow("Provider", cols.Provider), cols.Provider),
		padRight(truncateForRow("Model", cols.Model), cols.Model),
	}, " ")
	return configKeyDimStyle.Render(padRight(line, width))
}

func renderWorkflowStepRow(step workflowStep, cols workflowStepColumns, width int, selected, activePane bool) string {
	line := strings.Join([]string{
		padRight(truncateForRow(step.Name, cols.Name), cols.Name),
		padRight(truncateForRow(workflowProviderDisplay(step.Provider), cols.Provider), cols.Provider),
		padRight(truncateForRow(workflowModelDisplay(step.Model), cols.Model), cols.Model),
	}, " ")
	line = padRight(line, width)
	if !selected {
		return line
	}
	if activePane {
		return configSelectedRowStyle.Render(line)
	}
	return dimStyle.Render(line)
}

func renderWorkflowDetailRow(label, value string, width int, selected, activePane bool) string {
	labelW := 10
	if labelW > width/2 {
		labelW = max(1, width/3)
	}
	valueW := width - labelW - 2
	if valueW < 1 {
		valueW = 1
		labelW = max(1, width-valueW-2)
	}
	labelText := padRight(truncateForRow(label, labelW), labelW)
	valueText := padRight(truncateForRow(value, valueW), valueW)
	plain := labelText + "  " + valueText
	if selected {
		if activePane {
			return configSelectedRowStyle.Render(plain)
		}
		return dimStyle.Render(plain)
	}
	return configKeyDimStyle.Render(labelText) + "  " + valueText
}

func workflowProviderDisplay(provider string) string {
	if strings.TrimSpace(provider) == "" {
		return "(none)"
	}
	return provider
}

func workflowModelDisplay(model string) string {
	if strings.TrimSpace(model) == "" {
		return "(default)"
	}
	return model
}

func workflowPromptPreview(prompt string) string {
	flat := strings.Join(strings.Fields(strings.ReplaceAll(prompt, "\r", "")), " ")
	if flat == "" {
		return "(empty)"
	}
	return flat
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
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(activeTheme.accent).
		Padding(1, 2)
	contentW := max(30, lipgloss.Width(b.renameDraft)+2)
	if contentW > 50 {
		contentW = 50
	}
	if contentW > width-10 {
		contentW = max(20, width-10)
	}
	promptLine := filterPromptLine(b.renameDraft, hint)
	body := strings.Join([]string{
		configTitleStyle.Render(title),
		"",
		configPromptStyle.Render("> ") + padRight(promptLine, contentW),
		"",
		configHelpStyle.Render("enter save · esc cancel"),
	}, "\n")
	return box.Render(body)
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
	return box.Render(body)
}

func (b *workflowsBuilderState) renderConfirm(width, height int) string {
	target := "this entry"
	switch b.confirming {
	case "delete-workflow":
		if wIdx, ok := b.selectedWorkflowIdx(); ok {
			target = "workflow \"" + b.items[wIdx].Name + "\""
		}
	case "delete-step":
		if wIdx, sIdx, ok := b.selectedStepIdx(); ok {
			target = "step \"" + b.items[wIdx].Steps[sIdx].Name + "\""
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
	return box.Render(rendered)
}
