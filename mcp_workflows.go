package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcp_workflows.go exposes the workflow CRUD/run surface as MCP tools
// on the existing per-tab bridge so the chat agent can list, inspect,
// create, edit, delete, and dispatch workflow pipelines without a
// separate HTTP backend. Every handler tenants against b.getCwd() —
// the same per-tab project root the memory hooks use — so a chat
// agent in project A cannot read or mutate workflows in project B.
//
// All read-modify-write paths go through withConfigLock so concurrent
// MCP calls (the SDK serves each request on its own goroutine) and
// the workflow tracker's terminal-status persistence cannot race the
// load → mutate → save cycle.
//
// Per the plan: workflow_run is fire-and-forget. The handler emits a
// spawnWorkflowTabMsg via teaProgramPtr.Send and returns immediately
// with the session key — there is no synchronous round-trip and no
// `wait` flag. Status tools are intentionally out of scope for v1.

// ----- Tool I/O schemas -----

type workflowListInput struct{}

// workflowInnerListStepView is an agent step inside a loop, in the
// trimmed list shape. It has no Kind/Steps fields on purpose: the MCP
// SDK's JSON-schema generator rejects self-referential Go types, and a
// separate inner type also makes a nested loop structurally
// inexpressible over the wire (the one-layer-deep rule).
type workflowInnerListStepView struct {
	Name     string `json:"name" jsonschema:"step name"`
	Provider string `json:"provider" jsonschema:"provider id (claude, codex, ...)"`
	Model    string `json:"model,omitempty" jsonschema:"model id (empty = provider default)"`
}

type workflowListStepView struct {
	Name     string `json:"name" jsonschema:"step name"`
	Kind     string `json:"kind,omitempty" jsonschema:"empty for an agent step; 'loop' for a loop container"`
	Provider string `json:"provider,omitempty" jsonschema:"provider id (claude, codex, ...); agent steps only"`
	Model    string `json:"model,omitempty" jsonschema:"model id (empty = provider default)"`

	Steps         []workflowInnerListStepView `json:"steps,omitempty" jsonschema:"inner steps run each iteration; loop steps only"`
	MaxIterations int                         `json:"maxIterations,omitempty" jsonschema:"iteration cap; loop steps only (0 = default)"`
}

type workflowListItem struct {
	Name  string                 `json:"name" jsonschema:"workflow name"`
	Steps []workflowListStepView `json:"steps" jsonschema:"steps in execution order; prompts omitted to keep the listing small"`
}

type workflowListOutput struct {
	Workflows []workflowListItem `json:"workflows" jsonschema:"all workflows defined for the current project"`
}

type workflowGetInput struct {
	Name string `json:"name" jsonschema:"workflow name"`
}

// workflowInnerStepView is an agent step inside a loop, in the full
// shape (carries the prompt). Like workflowInnerListStepView it has no
// Kind/Steps fields: the schema must stay non-recursive, and a nested
// loop must be impossible to express.
type workflowInnerStepView struct {
	Name     string `json:"name" jsonschema:"step name"`
	Provider string `json:"provider" jsonschema:"provider id (claude, codex, ...)"`
	Model    string `json:"model,omitempty" jsonschema:"model id (empty = provider default)"`
	Prompt   string `json:"prompt,omitempty" jsonschema:"user-authored prompt for this step"`
}

type workflowStepView struct {
	Name string `json:"name" jsonschema:"step name"`
	Kind string `json:"kind,omitempty" jsonschema:"empty for an agent step; 'loop' for a loop container"`

	// Agent-step fields (kind == "").
	Provider string `json:"provider,omitempty" jsonschema:"provider id (claude, codex, ...); required for agent steps"`
	Model    string `json:"model,omitempty" jsonschema:"model id (empty = provider default)"`
	Prompt   string `json:"prompt,omitempty" jsonschema:"user-authored prompt; agent steps only"`

	// Loop-step fields (kind == "loop").
	Steps         []workflowInnerStepView `json:"steps,omitempty" jsonschema:"inner agent steps run in order each iteration; loop steps only (loops cannot nest)"`
	MaxIterations int                     `json:"maxIterations,omitempty" jsonschema:"iteration cap; loop steps only (0 = default of 10)"`
	ExitCondition string                  `json:"exitCondition,omitempty" jsonschema:"free-text goal injected into inner step prompts so the agent knows when to call workflow_loop with break; loop steps only"`
}

type workflowDefView struct {
	Name  string             `json:"name"`
	Steps []workflowStepView `json:"steps"`
}

type workflowGetOutput struct {
	Workflow workflowDefView `json:"workflow"`
}

type workflowCreateInput struct {
	Name  string             `json:"name" jsonschema:"new workflow name; must be unique within this project"`
	Steps []workflowStepView `json:"steps,omitempty" jsonschema:"steps to create the workflow with; may be empty"`
}

type workflowCreateOutput struct {
	Workflow workflowDefView `json:"workflow" jsonschema:"the created workflow"`
}

type workflowEditInput struct {
	Name    string              `json:"name" jsonschema:"existing workflow name to edit"`
	NewName string              `json:"new_name,omitempty" jsonschema:"optional new name; must be unique within this project"`
	Steps   *[]workflowStepView `json:"steps,omitempty" jsonschema:"if provided, replaces the entire steps array (full-replace semantics); omit to leave steps unchanged"`
}

type workflowEditOutput struct {
	Workflow workflowDefView `json:"workflow" jsonschema:"the updated workflow"`
}

type workflowDeleteInput struct {
	Name string `json:"name" jsonschema:"workflow name to delete"`
}

type workflowDeleteOutput struct {
	Deleted bool `json:"deleted" jsonschema:"true on success"`
}

type workflowRunInput struct {
	Name   string `json:"name" jsonschema:"workflow name to run"`
	Append string `json:"append,omitempty" jsonschema:"text appended after step 1's prompt as a Reference block; empty omits the block"`
}

type workflowRunOutput struct {
	Workflow   string `json:"workflow" jsonschema:"workflow name that was dispatched"`
	SessionKey string `json:"session_key" jsonschema:"unique key for this run; consumed by the workflow tracker"`
	StartedAt  string `json:"started_at" jsonschema:"RFC3339 timestamp marking dispatch"`
}

type workflowLoopInput struct {
	Decision string `json:"decision" jsonschema:"break to end the loop now, or continue to run another iteration"`
	Reason   string `json:"reason,omitempty" jsonschema:"short justification for the decision, surfaced in the workflow log"`
}

type workflowLoopOutput struct {
	Registered bool   `json:"registered" jsonschema:"true when the intent was recorded against an active loop"`
	Decision   string `json:"decision" jsonschema:"the decision that was registered, echoed back"`
	Note       string `json:"note" jsonschema:"human-readable status"`
}

// workflowLoopReply is the model's answer to a workflowLoopSignalMsg.
// registered is true when the run was inside a live loop and the intent
// landed; note is the human-readable status echoed back to the agent.
type workflowLoopReply struct {
	registered bool
	note       string
}

// workflowLoopSignalMsg carries a workflow_loop tool call from the MCP
// bridge to the owning workflow tab. The tool blocks on `reply` (like
// askToolRequestMsg) so the intent is guaranteed recorded before the
// agent's turn ends — the runner reads it at turnComplete. tabID routes
// the message to the right tab via dispatchByTabID.
type workflowLoopSignalMsg struct {
	tabID    int
	decision string
	reason   string
	reply    chan workflowLoopReply
}

// ----- Tool descriptions -----

const (
	workflowListToolDescription = `List all workflows defined for the current project.

Returns each workflow's name and its steps' (name, provider, model). Step prompts are omitted to keep the listing payload small — call workflow_get to see the full prompt for a specific workflow.`

	workflowGetToolDescription = `Get the full definition of a workflow including each step's prompt.

Returns the workflow with all steps in execution order. Errors when the named workflow does not exist in the current project.`

	workflowCreateToolDescription = `Create a new workflow in the current project.

The name must be non-empty and not collide with any existing workflow.

Each step is one of two kinds:
  - Agent step (kind omitted or ""): name required; provider must be a registered agent CLI (claude, codex, ...); model optional (empty = provider default); prompt may be empty.
  - Loop step (kind="loop"): name required; steps holds one or more inner agent steps run in order each iteration; maxIterations is an optional cap (0 = default of 10); exitCondition is free text describing when the loop should stop. Loops cannot be nested — a loop's inner steps must all be agent steps.

A loop repeats its inner steps until an inner agent calls the workflow_loop tool with decision="break" (or maxIterations is reached). The final inner step of each iteration must register a decision; the exitCondition text is injected into the inner prompts to guide that call.

Errors on duplicate name, empty step name, unknown provider, a nested loop, or a loop with no inner steps.`

	workflowEditToolDescription = `Edit an existing workflow.

Pass new_name to rename. Pass steps to replace the entire steps array (full-replace semantics — no per-step CRUD). Omit a field to leave it unchanged. Steps follow the same agent/loop shape documented on workflow_create.

Errors when the workflow doesn't exist, when new_name collides with another workflow, when a step is malformed (empty name, unknown provider, nested loop, empty loop), or when the workflow is currently running anywhere in this process.`

	workflowDeleteToolDescription = `Delete a workflow from the current project.

Errors when the workflow doesn't exist or is currently running.`

	workflowRunToolDescription = `Dispatch a workflow run in the background.

Fire-and-forget: returns immediately with the session key. The workflow runs in a fresh tab; the user can switch to it with the tab bar to watch progress. Pass append to thread an arbitrary text blob into step 1's user prompt under a "Reference:" header; omit it to run the workflow with no extra context.

Errors when the workflow doesn't exist, when it has no steps, or when the UI isn't ready to spawn a tab.`

	workflowLoopToolDescription = `Register your break/continue intent for the loop you are currently running inside.

You will see this is relevant when your step prompt says you are running inside a workflow loop. Call this tool with:
  - decision="break" when the loop's exit condition is met — end the loop and let the workflow move on to the next step.
  - decision="continue" to run another iteration of the loop.
Always include a short reason.

This only RECORDS your intent; it does NOT end your turn or exit the loop immediately. Keep working and finish your turn normally — when your turn completes, the workflow acts on the most recent decision you registered. If you are the final step of the loop's iteration and your turn ends without a registered decision, you will be re-prompted to make one.

Has no effect when called outside a loop.`
)

// mcpSpawnWorkflowTab is the indirection workflow_run uses to dispatch
// a spawn message back to the app. Production points at the live
// tea.Program through teaProgramPtr; tests swap it to a captor so the
// run handler's wiring can be verified without a real bubbletea
// program. Returns an error when no UI is registered, mirroring the
// shape that bubbles up to the LLM through the IsError result.
var mcpSpawnWorkflowTab = sendSpawnWorkflowTabMsg

// sendSpawnWorkflowTabMsg is the production implementation of
// mcpSpawnWorkflowTab. Sends the spawn message via the registered
// tea.Program; returns an error when one is not yet wired (early
// startup, tests bypassing setTeaProgram).
func sendSpawnWorkflowTabMsg(msg spawnWorkflowTabMsg) error {
	p := teaProgramPtr.Load()
	if p == nil {
		return errors.New("ask UI not ready to spawn a workflow tab")
	}
	p.Send(msg)
	return nil
}

// ----- Helpers -----

// errResult builds a CallToolResult marked IsError with the message
// the LLM should see. Use for validation failures so the agent can
// adjust and retry; reserve a non-nil error return for true internal
// errors (file writes, etc.) where retry is unlikely to help.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}
}

// okResult builds a CallToolResult carrying the structured payload as
// a text block. The SDK populates StructuredContent automatically when
// the typed handler returns a non-zero output struct, but we still
// emit the text block so older MCP clients that only read .content
// see something useful.
func okResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// validateProviderID returns an error if id is not registered. Empty
// id is rejected — callers must pick a provider explicitly.
func validateProviderID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("provider is required")
	}
	prov := providerByID(id)
	if prov == nil || prov.ID() != id {
		return fmt.Errorf("unknown provider: %q", id)
	}
	return nil
}

// validateSteps screens each step in `steps` for the rules common to
// create / edit. Agent steps need a non-empty name and a registered
// provider. Loop steps need a non-empty name, at least one inner step,
// a non-negative MaxIterations, and inner steps that are themselves
// valid agent steps — loops cannot nest, so an inner loop is rejected.
func validateSteps(steps []workflowStepView) error {
	for i, s := range steps {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			return fmt.Errorf("step %d: name is required", i+1)
		}
		switch s.Kind {
		case workflowStepKindLoop:
			if len(s.Steps) == 0 {
				return fmt.Errorf("step %d (%q): a loop must contain at least one inner step", i+1, name)
			}
			if s.MaxIterations < 0 {
				return fmt.Errorf("step %d (%q): maxIterations cannot be negative", i+1, name)
			}
			// Inner steps are workflowInnerStepView, which has no Kind/
			// Steps fields, so a nested loop is structurally impossible
			// here — we only check name and provider.
			for j, inner := range s.Steps {
				iname := strings.TrimSpace(inner.Name)
				if iname == "" {
					return fmt.Errorf("step %d (%q) inner step %d: name is required", i+1, name, j+1)
				}
				if err := validateProviderID(inner.Provider); err != nil {
					return fmt.Errorf("step %d (%q) inner step %d (%q): %w", i+1, name, j+1, iname, err)
				}
			}
		case workflowStepKindAgent:
			if err := validateProviderID(s.Provider); err != nil {
				return fmt.Errorf("step %d (%q): %w", i+1, name, err)
			}
		default:
			return fmt.Errorf("step %d (%q): unknown kind %q", i+1, name, s.Kind)
		}
	}
	return nil
}

// stepsViewToDef converts the wire-shape step list into the on-disk
// workflowStep slice, trimming whitespace on every field as we go.
// Returns nil for an empty input so the persisted workflowDef matches
// the shape produced by the builder UI (which also stores nil for the
// no-steps case).
func stepsViewToDef(in []workflowStepView) []workflowStep {
	if len(in) == 0 {
		return nil
	}
	out := make([]workflowStep, 0, len(in))
	for _, s := range in {
		step := workflowStep{Name: strings.TrimSpace(s.Name), Kind: s.Kind}
		if s.Kind == workflowStepKindLoop {
			step.Steps = innerStepsViewToDef(s.Steps)
			step.MaxIterations = s.MaxIterations
			step.ExitCondition = strings.TrimSpace(s.ExitCondition)
		} else {
			step.Provider = strings.TrimSpace(s.Provider)
			step.Model = strings.TrimSpace(s.Model)
			step.Prompt = s.Prompt
		}
		out = append(out, step)
	}
	return out
}

// innerStepsViewToDef converts a loop's inner agent-step wire shapes to
// the on-disk workflowStep slice (Kind stays empty — inner steps are
// always agent steps).
func innerStepsViewToDef(in []workflowInnerStepView) []workflowStep {
	if len(in) == 0 {
		return nil
	}
	out := make([]workflowStep, 0, len(in))
	for _, s := range in {
		out = append(out, workflowStep{
			Name:     strings.TrimSpace(s.Name),
			Provider: strings.TrimSpace(s.Provider),
			Model:    strings.TrimSpace(s.Model),
			Prompt:   s.Prompt,
		})
	}
	return out
}

// stepsDefToView is the inverse of stepsViewToDef. Returns an empty
// slice (not nil) so JSON marshalling emits `"steps": []` rather than
// `"steps": null` — matches what the builder writes to disk.
func stepsDefToView(in []workflowStep) []workflowStepView {
	out := make([]workflowStepView, 0, len(in))
	for _, s := range in {
		v := workflowStepView{Name: s.Name, Kind: s.Kind}
		if s.isLoop() {
			v.Steps = innerStepsDefToView(s.Steps)
			v.MaxIterations = s.MaxIterations
			v.ExitCondition = s.ExitCondition
		} else {
			v.Provider = s.Provider
			v.Model = s.Model
			v.Prompt = s.Prompt
		}
		out = append(out, v)
	}
	return out
}

// innerStepsDefToView projects a loop's inner steps into the full inner
// wire shape. Returns an empty (non-nil) slice so JSON marshals `[]`
// rather than `null`, matching the rest of the view conversions.
func innerStepsDefToView(in []workflowStep) []workflowInnerStepView {
	out := make([]workflowInnerStepView, 0, len(in))
	for _, s := range in {
		out = append(out, workflowInnerStepView{
			Name:     s.Name,
			Provider: s.Provider,
			Model:    s.Model,
			Prompt:   s.Prompt,
		})
	}
	return out
}

// stepsDefToListView is the trimmed view used by workflow_list — drops
// the prompt to keep the listing payload small.
func stepsDefToListView(in []workflowStep) []workflowListStepView {
	out := make([]workflowListStepView, 0, len(in))
	for _, s := range in {
		v := workflowListStepView{Name: s.Name, Kind: s.Kind}
		if s.isLoop() {
			v.Steps = innerStepsDefToListView(s.Steps)
			v.MaxIterations = s.MaxIterations
		} else {
			v.Provider = s.Provider
			v.Model = s.Model
		}
		out = append(out, v)
	}
	return out
}

// innerStepsDefToListView is the trimmed inner-step projection used by
// workflow_list — drops the prompt to keep the listing payload small.
func innerStepsDefToListView(in []workflowStep) []workflowInnerListStepView {
	out := make([]workflowInnerListStepView, 0, len(in))
	for _, s := range in {
		out = append(out, workflowInnerListStepView{
			Name:     s.Name,
			Provider: s.Provider,
			Model:    s.Model,
		})
	}
	return out
}

// workflowDefToView is the wire-shape projection of a stored
// workflowDef. Used by every tool that returns a single workflow.
func workflowDefToView(w workflowDef) workflowDefView {
	return workflowDefView{
		Name:  w.Name,
		Steps: stepsDefToView(w.Steps),
	}
}

func workflowItemsForCwd(cwd string) ([]workflowDef, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	pc := loadProjectConfig(cfg, cwd)
	return pc.Workflows.Items, nil
}

func workflowByNameForCwd(cwd, name string) (workflowDef, bool, error) {
	items, err := workflowItemsForCwd(cwd)
	if err != nil {
		return workflowDef{}, false, err
	}
	for _, w := range items {
		if w.Name == name {
			return w, true, nil
		}
	}
	return workflowDef{}, false, nil
}

// requireCwd returns the bridge's tenant cwd, or an error result when
// it's empty. Empty cwd means the bridge wasn't fully wired (test
// without setCwd, startup race) and any project lookup would fall
// through to the global config — refuse explicitly so a misconfigured
// bridge can't bleed into another project's data.
func (b *mcpBridge) requireCwd() (string, *mcp.CallToolResult) {
	cwd := b.getCwd()
	if cwd == "" {
		return "", errResult("workflow tools require a project cwd; the ask tab does not have one configured")
	}
	return cwd, nil
}

// ----- Handlers -----

func (b *mcpBridge) workflowListTool(_ context.Context, _ *mcp.CallToolRequest, _ workflowListInput) (*mcp.CallToolResult, workflowListOutput, error) {
	cwd, errRes := b.requireCwd()
	if errRes != nil {
		return errRes, workflowListOutput{}, nil
	}
	items, err := workflowItemsForCwd(cwd)
	if err != nil {
		return nil, workflowListOutput{}, fmt.Errorf("load workflows: %w", err)
	}
	out := workflowListOutput{Workflows: make([]workflowListItem, 0, len(items))}
	for _, w := range items {
		out.Workflows = append(out.Workflows, workflowListItem{
			Name:  w.Name,
			Steps: stepsDefToListView(w.Steps),
		})
	}
	return okResult(fmt.Sprintf("%d workflow(s) defined", len(out.Workflows))), out, nil
}

func (b *mcpBridge) workflowGetTool(_ context.Context, _ *mcp.CallToolRequest, in workflowGetInput) (*mcp.CallToolResult, workflowGetOutput, error) {
	cwd, errRes := b.requireCwd()
	if errRes != nil {
		return errRes, workflowGetOutput{}, nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errResult("name is required"), workflowGetOutput{}, nil
	}
	w, ok, err := workflowByNameForCwd(cwd, name)
	if err != nil {
		return nil, workflowGetOutput{}, fmt.Errorf("load workflows: %w", err)
	}
	if !ok {
		return errResult(fmt.Sprintf("workflow %q not found", name)), workflowGetOutput{}, nil
	}
	out := workflowGetOutput{Workflow: workflowDefToView(w)}
	return okResult(fmt.Sprintf("workflow %q has %d step(s)", w.Name, len(w.Steps))), out, nil
}

func (b *mcpBridge) workflowCreateTool(_ context.Context, _ *mcp.CallToolRequest, in workflowCreateInput) (*mcp.CallToolResult, workflowCreateOutput, error) {
	cwd, errRes := b.requireCwd()
	if errRes != nil {
		return errRes, workflowCreateOutput{}, nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errResult("name is required"), workflowCreateOutput{}, nil
	}
	if err := validateSteps(in.Steps); err != nil {
		return errResult(err.Error()), workflowCreateOutput{}, nil
	}
	def := workflowDef{Name: name, Steps: stepsViewToDef(in.Steps)}

	var collision bool
	if err := withConfigLock(func() error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pc := loadProjectConfig(cfg, cwd)
		for _, w := range pc.Workflows.Items {
			if w.Name == name {
				collision = true
				return nil
			}
		}
		pc.Workflows.Items = append(pc.Workflows.Items, def)
		cfg = upsertProjectConfig(cfg, cwd, pc)
		return saveConfig(cfg)
	}); err != nil {
		return nil, workflowCreateOutput{}, fmt.Errorf("save: %w", err)
	}
	if collision {
		return errResult(fmt.Sprintf("workflow %q already exists", name)), workflowCreateOutput{}, nil
	}
	return okResult(fmt.Sprintf("workflow %q created with %d step(s)", def.Name, len(def.Steps))),
		workflowCreateOutput{Workflow: workflowDefToView(def)}, nil
}

func (b *mcpBridge) workflowEditTool(_ context.Context, _ *mcp.CallToolRequest, in workflowEditInput) (*mcp.CallToolResult, workflowEditOutput, error) {
	cwd, errRes := b.requireCwd()
	if errRes != nil {
		return errRes, workflowEditOutput{}, nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errResult("name is required"), workflowEditOutput{}, nil
	}
	newName := name
	if in.NewName != "" {
		newName = strings.TrimSpace(in.NewName)
		if newName == "" {
			return errResult("new_name cannot be blank when provided"), workflowEditOutput{}, nil
		}
	}
	if in.Steps != nil {
		if err := validateSteps(*in.Steps); err != nil {
			return errResult(err.Error()), workflowEditOutput{}, nil
		}
	}

	// Editing a workflow that's running anywhere in the process is
	// blocked — same gate the builder UI uses. Without this, an MCP
	// edit landing mid-run would mutate the workflowDef in place
	// while workflows_run.go's startWorkflowStep is reading from it,
	// producing a torn pipeline.
	active := workflowTracker().activeWorkflowNames()
	if _, running := active[name]; running {
		return errResult(fmt.Sprintf("workflow %q is currently running and cannot be edited", name)), workflowEditOutput{}, nil
	}
	// If renaming, the new name must also not be running. (A running
	// workflow currently uses its old name; renaming it could leave
	// the tracker pointing at a stale name. Block to be safe.)
	if newName != name {
		if _, running := active[newName]; running {
			return errResult(fmt.Sprintf("workflow %q is currently running and cannot be renamed onto", newName)), workflowEditOutput{}, nil
		}
	}

	var (
		notFound bool
		collide  bool
		updated  workflowDef
	)
	if err := withConfigLock(func() error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pc := loadProjectConfig(cfg, cwd)
		idx := -1
		for i, w := range pc.Workflows.Items {
			if w.Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			notFound = true
			return nil
		}
		if newName != name {
			for i, w := range pc.Workflows.Items {
				if i != idx && w.Name == newName {
					collide = true
					return nil
				}
			}
		}
		w := pc.Workflows.Items[idx]
		w.Name = newName
		if in.Steps != nil {
			w.Steps = stepsViewToDef(*in.Steps)
		}
		pc.Workflows.Items[idx] = w
		updated = w
		// Renamed workflows leave any disk session record under the
		// OLD name. The session key for issue-sourced workflows is
		// keyed on issue identity (not workflow name), so the
		// session bookkeeping survives the rename — only the
		// `Workflow` field on the disk session would still mention
		// the old name. That's acceptable for v1; the next terminal
		// status write will overwrite with the new name.
		cfg = upsertProjectConfig(cfg, cwd, pc)
		return saveConfig(cfg)
	}); err != nil {
		return nil, workflowEditOutput{}, fmt.Errorf("save: %w", err)
	}
	if notFound {
		return errResult(fmt.Sprintf("workflow %q not found", name)), workflowEditOutput{}, nil
	}
	if collide {
		return errResult(fmt.Sprintf("another workflow already uses the name %q", newName)), workflowEditOutput{}, nil
	}
	return okResult(fmt.Sprintf("workflow %q updated", updated.Name)),
		workflowEditOutput{Workflow: workflowDefToView(updated)}, nil
}

func (b *mcpBridge) workflowDeleteTool(_ context.Context, _ *mcp.CallToolRequest, in workflowDeleteInput) (*mcp.CallToolResult, workflowDeleteOutput, error) {
	cwd, errRes := b.requireCwd()
	if errRes != nil {
		return errRes, workflowDeleteOutput{}, nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errResult("name is required"), workflowDeleteOutput{}, nil
	}
	active := workflowTracker().activeWorkflowNames()
	if _, running := active[name]; running {
		return errResult(fmt.Sprintf("workflow %q is currently running and cannot be deleted", name)), workflowDeleteOutput{}, nil
	}

	var notFound bool
	if err := withConfigLock(func() error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pc := loadProjectConfig(cfg, cwd)
		idx := -1
		for i, w := range pc.Workflows.Items {
			if w.Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			notFound = true
			return nil
		}
		pc.Workflows.Items = append(pc.Workflows.Items[:idx], pc.Workflows.Items[idx+1:]...)
		if len(pc.Workflows.Items) == 0 {
			pc.Workflows.Items = nil
		}
		cfg = upsertProjectConfig(cfg, cwd, pc)
		return saveConfig(cfg)
	}); err != nil {
		return nil, workflowDeleteOutput{}, fmt.Errorf("save: %w", err)
	}
	if notFound {
		return errResult(fmt.Sprintf("workflow %q not found", name)), workflowDeleteOutput{}, nil
	}
	return okResult(fmt.Sprintf("workflow %q deleted", name)),
		workflowDeleteOutput{Deleted: true}, nil
}

func (b *mcpBridge) workflowRunTool(_ context.Context, _ *mcp.CallToolRequest, in workflowRunInput) (*mcp.CallToolResult, workflowRunOutput, error) {
	cwd, errRes := b.requireCwd()
	if errRes != nil {
		return errRes, workflowRunOutput{}, nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errResult("name is required"), workflowRunOutput{}, nil
	}
	w, ok, err := workflowByNameForCwd(cwd, name)
	if err != nil {
		return nil, workflowRunOutput{}, fmt.Errorf("load workflows: %w", err)
	}
	if !ok {
		return errResult(fmt.Sprintf("workflow %q not found", name)), workflowRunOutput{}, nil
	}
	if len(w.Steps) == 0 {
		return errResult(fmt.Sprintf("workflow %q has no steps", name)), workflowRunOutput{}, nil
	}
	// Re-validate the persisted definition before dispatch — a
	// workflow saved before a provider was unregistered (or hand-
	// edited on disk) would crash the runner mid-step. Catch it here
	// so the LLM gets a clear error instead of a workflow tab that
	// fails on step N for opaque reasons. validateSteps walks loop
	// containers too, so a malformed loop is rejected up front.
	if err := validateSteps(stepsDefToView(w.Steps)); err != nil {
		return errResult(fmt.Sprintf("workflow %q is invalid: %v", name, err)), workflowRunOutput{}, nil
	}

	source := textWorkflowSource(b.tabID, in.Append)
	startedAt := time.Now().UTC()
	if err := mcpSpawnWorkflowTab(spawnWorkflowTabMsg{
		OriginTabID: b.tabID,
		Cwd:         cwd,
		Workflow:    w,
		Source:      source,
	}); err != nil {
		return errResult(err.Error()), workflowRunOutput{}, nil
	}
	return okResult(fmt.Sprintf("workflow %q dispatched (session_key=%s)", w.Name, source.Key())),
		workflowRunOutput{
			Workflow:   w.Name,
			SessionKey: source.Key(),
			StartedAt:  startedAt.Format(time.RFC3339Nano),
		}, nil
}

// workflowLoopTool records an inner loop step's break/continue intent.
// It does not tenant on cwd (loop control is about the live run on this
// tab, not project data); instead it routes a workflowLoopSignalMsg to
// the owning tab and blocks on the reply so the intent is recorded
// before the agent's turn ends. The runner consumes the intent at
// turnComplete — see advanceWorkflowStep.
func (b *mcpBridge) workflowLoopTool(ctx context.Context, _ *mcp.CallToolRequest, in workflowLoopInput) (*mcp.CallToolResult, workflowLoopOutput, error) {
	decision := strings.TrimSpace(in.Decision)
	if decision != workflowLoopBreak && decision != workflowLoopContinue {
		return errResult(fmt.Sprintf("decision must be %q or %q", workflowLoopBreak, workflowLoopContinue)),
			workflowLoopOutput{}, nil
	}
	p := teaProgramPtr.Load()
	if p == nil {
		return errResult("ask UI not ready"), workflowLoopOutput{}, nil
	}
	reply := make(chan workflowLoopReply, 1)
	p.Send(workflowLoopSignalMsg{
		tabID:    b.tabID,
		decision: decision,
		reason:   strings.TrimSpace(in.Reason),
		reply:    reply,
	})
	select {
	case resp := <-reply:
		return okResult(resp.note), workflowLoopOutput{
			Registered: resp.registered,
			Decision:   decision,
			Note:       resp.note,
		}, nil
	case <-ctx.Done():
		return nil, workflowLoopOutput{}, ctx.Err()
	}
}

// registerWorkflowTools wires the workflow CRUD/run/loop tools onto
// b.server. Called once per bridge from newMCPBridge so every chat
// tab carries its own typed handlers tenanted on its own cwd.
func (b *mcpBridge) registerWorkflowTools() {
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "workflow_list",
		Description: workflowListToolDescription,
	}, b.workflowListTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "workflow_get",
		Description: workflowGetToolDescription,
	}, b.workflowGetTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "workflow_create",
		Description: workflowCreateToolDescription,
	}, b.workflowCreateTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "workflow_edit",
		Description: workflowEditToolDescription,
	}, b.workflowEditTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "workflow_delete",
		Description: workflowDeleteToolDescription,
	}, b.workflowDeleteTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "workflow_run",
		Description: workflowRunToolDescription,
	}, b.workflowRunTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "workflow_loop",
		Description: workflowLoopToolDescription,
	}, b.workflowLoopTool)
}
