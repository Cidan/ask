package main

import (
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
	Name        string                 `json:"name" jsonschema:"workflow name"`
	Scope       string                 `json:"scope" jsonschema:"where the workflow is stored: 'user' (~/.config/ask/ask.json, machine-local) or 'repo' (<root>/.ask/workflows/, committed and shared)"`
	Description string                 `json:"description,omitempty" jsonschema:"the author's statement of what this workflow is for and when to use it; judge fit against THIS, not the step names"`
	Steps       []workflowListStepView `json:"steps" jsonschema:"steps in execution order; prompts omitted to keep the listing small"`
}

type workflowListOutput struct {
	Workflows []workflowListItem `json:"workflows" jsonschema:"all workflows visible to the current project, repo scope first"`
}

type workflowGetInput struct {
	Name  string `json:"name" jsonschema:"workflow name"`
	Scope string `json:"scope,omitempty" jsonschema:"optional scope to read from ('user' or 'repo'); empty resolves repo first, then user"`
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
	ExitCondition string                  `json:"exitCondition,omitempty" jsonschema:"free-text goal injected into inner step prompts so the agent knows when to break the loop via end_turn; loop steps only"`
}

type workflowDefView struct {
	Name        string             `json:"name"`
	Scope       string             `json:"scope" jsonschema:"where the workflow is stored: 'user' or 'repo'"`
	Description string             `json:"description,omitempty" jsonschema:"the author's statement of what this workflow is for and when to use it"`
	Steps       []workflowStepView `json:"steps"`
}

type workflowGetOutput struct {
	Workflow workflowDefView `json:"workflow"`
}

type workflowCreateInput struct {
	Name        string             `json:"name" jsonschema:"new workflow name; must be unique within the chosen scope"`
	Scope       string             `json:"scope,omitempty" jsonschema:"where to store the workflow: 'user' (default; machine-local ask.json) or 'repo' (<root>/.ask/workflows/, committed and shared with the team)"`
	Description string             `json:"description,omitempty" jsonschema:"what this workflow is for and when to use it; surfaced in workflow_list so the agent judges fit against your stated intent"`
	Steps       []workflowStepView `json:"steps,omitempty" jsonschema:"steps to create the workflow with; may be empty"`
}

type workflowCreateOutput struct {
	Workflow workflowDefView `json:"workflow" jsonschema:"the created workflow"`
}

type workflowEditInput struct {
	Name        string              `json:"name" jsonschema:"existing workflow name to edit"`
	Scope       string              `json:"scope,omitempty" jsonschema:"scope holding the workflow ('user' or 'repo'); required when the name exists in both scopes"`
	NewName     string              `json:"new_name,omitempty" jsonschema:"optional new name; must be unique within the workflow's scope"`
	Description *string             `json:"description,omitempty" jsonschema:"if provided, replaces the description (pass an empty string to clear it); omit to leave it unchanged"`
	Steps       *[]workflowStepView `json:"steps,omitempty" jsonschema:"if provided, replaces the entire steps array (full-replace semantics); omit to leave steps unchanged"`
}

type workflowEditOutput struct {
	Workflow workflowDefView `json:"workflow" jsonschema:"the updated workflow"`
}

type workflowDeleteInput struct {
	Name  string `json:"name" jsonschema:"workflow name to delete"`
	Scope string `json:"scope,omitempty" jsonschema:"scope holding the workflow ('user' or 'repo'); required when the name exists in both scopes"`
}

type workflowDeleteOutput struct {
	Deleted bool `json:"deleted" jsonschema:"true on success"`
}

type workflowCopyInput struct {
	Name    string `json:"name" jsonschema:"workflow name to copy"`
	Scope   string `json:"scope,omitempty" jsonschema:"scope holding the source ('user' or 'repo'); required when the name exists in both scopes"`
	To      string `json:"to" jsonschema:"destination scope: 'user' (machine-local) or 'repo' (committed under <root>/.ask/workflows/)"`
	NewName string `json:"new_name,omitempty" jsonschema:"optional name for the copy; required when the destination scope already has a workflow named after the source"`
}

type workflowCopyOutput struct {
	Workflow workflowDefView `json:"workflow" jsonschema:"the new copy, in its destination scope"`
}

type workflowRunInput struct {
	Name   string `json:"name" jsonschema:"workflow name to run"`
	Scope  string `json:"scope,omitempty" jsonschema:"scope holding the workflow ('user' or 'repo'); required when the name exists in both scopes"`
	Append string `json:"append" jsonschema:"REQUIRED. The workflow runs in a fresh session with NO access to this conversation — its history, file reads, and tool results do not carry over. This text is the ONLY context the run receives, threaded into step 1's prompt as a Reference block. Submit the FULL plan and all context the workflow needs to execute the task end to end: the goal, the concrete steps, relevant file paths, constraints, and acceptance criteria. Do not pass a one-line summary or a pointer back to this chat."`
}

type workflowRunOutput struct {
	Workflow   string `json:"workflow" jsonschema:"workflow name that was dispatched"`
	SessionKey string `json:"session_key" jsonschema:"unique key for this run; consumed by the workflow tracker"`
	StartedAt  string `json:"started_at" jsonschema:"RFC3339 timestamp marking dispatch"`
}

// endTurnReply is the model's answer to an endTurnSignalMsg. registered
// is true when a workflow step was live and the summary landed; note is
// the human-readable status echoed back to the agent.
type endTurnReply struct {
	registered bool
	note       string
}

// endTurnSignalMsg carries an end_turn tool call from the MCP bridge to
// the owning workflow tab. The tool blocks on `reply` (like
// askToolRequestMsg) so the summary/decision is guaranteed recorded
// before the agent's turn ends — the runner reads it at turnComplete.
// tabID routes the message to the right tab via dispatchByTabID.
type endTurnSignalMsg struct {
	tabID    int
	summary  string
	decision string
	reply    chan endTurnReply
}

// ----- Tool descriptions -----

const (
	workflowListToolDescription = `List all workflows visible to the current project, from both scopes.

A workflow lives in one of two scopes: 'user' (machine-local, stored in ~/.config/ask/ask.json) or 'repo' (stored as one JSON file per workflow under <project root>/.ask/workflows/ — committed to the repo and shared with the team). Repo-scope workflows list first. The same name may exist in both scopes; each item's 'scope' field disambiguates.

Returns each workflow's name, scope, description (what it's for and when to use it — judge fit against this), and its steps' (name, provider, model). Step prompts are omitted to keep the listing payload small — call workflow_get to see the full prompt for a specific workflow.`

	workflowGetToolDescription = `Get the full definition of a workflow including each step's prompt.

Pass scope ('user' or 'repo') to read a specific copy; with no scope the repo copy wins when the name exists in both. Errors when the named workflow does not exist.`

	workflowCreateToolDescription = `Create a new workflow.

The name must be non-empty and not collide with any existing workflow in the chosen scope. scope picks where it is stored: 'user' (default — machine-local ask.json) or 'repo' (one JSON file under <project root>/.ask/workflows/, committed and shared with the team).

description is optional but strongly recommended: state what the workflow is FOR and when to use it (its trigger conditions, in plain words). That text is surfaced verbatim in workflow_list, and the agent judges whether the workflow fits a task against it — without a description it must guess intent from the step names, which is unreliable.

Each step is one of two kinds:
  - Agent step (kind omitted or ""): name required; provider must be a registered agent CLI (claude, codex, ...); model optional (empty = provider default); prompt may be empty.
  - Loop step (kind="loop"): name required; steps holds one or more inner agent steps run in order each iteration; maxIterations is an optional cap (0 = default of 10); exitCondition is free text describing when the loop should stop. Loops cannot be nested — a loop's inner steps must all be agent steps.

A loop repeats its inner steps until an inner agent ends a turn with the end_turn tool's decision="break" (or maxIterations is reached). Every step must call end_turn with a summary; the final inner step of each iteration must additionally register a decision (continue/break). The exitCondition text is injected into the inner prompts to guide that call.

Errors on duplicate name within the scope, empty step name, unknown provider, a nested loop, or a loop with no inner steps.`

	workflowEditToolDescription = `Edit an existing workflow in place (it stays in its scope).

Pass new_name to rename. Pass description to replace the workflow's purpose statement (empty string clears it). Pass steps to replace the entire steps array (full-replace semantics — no per-step CRUD). Omit a field to leave it unchanged. Steps follow the same agent/loop shape documented on workflow_create. When the name exists in both scopes you must pass scope to pick which copy to edit; use workflow_copy to move a workflow between scopes.

Errors when the workflow doesn't exist, when the name is ambiguous across scopes and no scope was given, when new_name collides within the scope, when a step is malformed (empty name, unknown provider, nested loop, empty loop), or when the workflow is currently running anywhere in this process.`

	workflowDeleteToolDescription = `Delete a workflow.

When the name exists in both scopes you must pass scope to pick which copy to delete. Errors when the workflow doesn't exist, the name is ambiguous, or the workflow is currently running.`

	workflowCopyToolDescription = `Copy a workflow between scopes (or duplicate it within one).

'to' is the destination scope: 'repo' makes a workflow repo-local (a committed JSON file under <project root>/.ask/workflows/ that the whole team can use), 'user' copies it into the machine-local ask.json. The source is untouched — to move, copy then workflow_delete the original.

Naming conflicts: when the destination scope already has a workflow by that name, the call errors and you must pass new_name. new_name is also how you duplicate within the same scope.

Errors when the source doesn't exist, the source name is ambiguous across scopes and no scope was given, or the destination name is taken.`

	workflowRunToolDescription = `Dispatch a workflow run in the background.

Fire-and-forget: returns immediately with the session key. The workflow runs in a fresh tab; the user can switch to it with the tab bar to watch progress. When the name exists in both scopes pass scope to pick which copy runs.

CRITICAL — the workflow starts in a brand-new session with NO access to this conversation. Its message history, the files you have read, and your tool results DO NOT carry over. The append parameter is the ONLY channel of context into the run: its text is threaded into step 1's prompt as a "Reference:" block, and that is everything the workflow gets.

BEFORE calling workflow_run you MUST:
  1. Call clear_plans to remove any stale plan artifacts from previous runs.
  2. Create the directory ask/plans/start/ and write the starting plan into one or more FILES INSIDE that directory — for example ask/plans/start/plan.md.

CRITICAL: ask/plans/start/ must be a DIRECTORY, not a file. Do not write a single file named "start". The workflow runner verifies the shape before step 1. If it is missing, empty, or a file, step 1 is re-prompted to fix the directory before any work is done — the run itself is not aborted.

After step 1, each step writes its notes to ask/plans/<step-name>/. Missing directories are created automatically, but if the path exists as a file the step is re-prompted to fix it.

append is REQUIRED. You MUST submit the FULL plan the workflow needs to carry the task through end to end on its own — the goal, the concrete steps to take, the relevant file paths, the constraints, and the acceptance criteria. Do NOT pass a bare one-line summary, and do NOT point back at "the conversation above" — the workflow cannot see it. Write append as if briefing someone who has never seen this chat.

Errors when the workflow doesn't exist, the name is ambiguous across scopes, when it has no steps, when append is empty, or when the UI isn't ready to spawn a tab.`

	endTurnToolDescription = `Report the end of your turn for the current workflow step. REQUIRED on every step.

Call this once, as the final action of your turn, with:
  - summary: 1-3 sentences describing what you did this step and the outcome (plus anything left to do). This becomes this step's entry in the workflow log — write it for a human following along, not as a note to yourself.
  - decision: ONLY when your step prompt says you are the final step of a workflow loop iteration. Pass "continue" to run another iteration or "break" to end the loop. Breaking should be exceptional — only when the loop's exit goal is met. Omit decision when you are not inside a loop, or not its final step (unless you are deliberately breaking the loop early).

Calling this RECORDS your report; it does NOT end your turn early or exit a loop immediately. Finish your turn normally — the workflow acts on what you registered when your turn completes. If your turn ends without calling end_turn (or, as a loop's final step, without a decision), you will be re-prompted to provide it.`
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
		Name:        w.Name,
		Scope:       workflowScopeTag(w.Scope),
		Description: w.Description,
		Steps:       stepsDefToView(w.Steps),
	}
}

// workflowItemsForCwd returns the merged repo+user workflow list,
// propagating an unreadable ask.json as an error (the tool layer
// reports it; pure-UI paths use listAllWorkflows and degrade
// silently instead).
func workflowItemsForCwd(cwd string) ([]workflowDef, error) {
	user, err := loadUserWorkflows(cwd)
	if err != nil {
		return nil, err
	}
	return append(loadRepoWorkflows(cwd), user...), nil
}

// resolveWorkflowResult adapts resolveWorkflowByName's error shapes
// to tool results: lookup/ambiguity/scope errors become IsError
// results the LLM can react to.
func resolveWorkflowResult(cwd, name, scope string) (workflowDef, *mcp.CallToolResult) {
	w, err := resolveWorkflowByName(cwd, name, scope)
	if err != nil {
		return workflowDef{}, errResult(err.Error())
	}
	return w, nil
}

// requireWorkflowCwd returns an error result when the tenant cwd is
// empty. Empty cwd means the caller wasn't fully wired (test without
// setCwd, startup race) and any project lookup would fall through to
// the global config — refuse explicitly so a misconfigured caller
// can't bleed into another project's data.
func requireWorkflowCwd(cwd string) *mcp.CallToolResult {
	if cwd == "" {
		return errResult("workflow tools require a project cwd; the ask tab does not have one configured")
	}
	return nil
}

// ----- Handlers -----

func workflowListCore(cwd string, _ workflowListInput) (*mcp.CallToolResult, workflowListOutput, error) {
	if errRes := requireWorkflowCwd(cwd); errRes != nil {
		return errRes, workflowListOutput{}, nil
	}
	items, err := workflowItemsForCwd(cwd)
	if err != nil {
		return nil, workflowListOutput{}, fmt.Errorf("load workflows: %w", err)
	}
	out := workflowListOutput{Workflows: make([]workflowListItem, 0, len(items))}
	for _, w := range items {
		out.Workflows = append(out.Workflows, workflowListItem{
			Name:        w.Name,
			Scope:       workflowScopeTag(w.Scope),
			Description: w.Description,
			Steps:       stepsDefToListView(w.Steps),
		})
	}
	return okResult(fmt.Sprintf("%d workflow(s) defined", len(out.Workflows))), out, nil
}

func workflowGetCore(cwd string, in workflowGetInput) (*mcp.CallToolResult, workflowGetOutput, error) {
	if errRes := requireWorkflowCwd(cwd); errRes != nil {
		return errRes, workflowGetOutput{}, nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errResult("name is required"), workflowGetOutput{}, nil
	}
	// Reads are forgiving on ambiguity: with no explicit scope the
	// repo copy wins (project-wins, same as the picker / runner).
	scope := in.Scope
	if scope != "" {
		if _, err := normalizeWorkflowScope(scope); err != nil {
			return errResult(err.Error()), workflowGetOutput{}, nil
		}
		scope, _ = normalizeWorkflowScope(scope)
	}
	w, ok := findWorkflow(cwd, name, scope)
	if !ok {
		return errResult(fmt.Sprintf("workflow %q not found", name)), workflowGetOutput{}, nil
	}
	out := workflowGetOutput{Workflow: workflowDefToView(w)}
	return okResult(fmt.Sprintf("workflow %q (%s scope) has %d step(s)", w.Name, workflowScopeTag(w.Scope), len(w.Steps))), out, nil
}

func workflowCreateCore(cwd string, in workflowCreateInput) (*mcp.CallToolResult, workflowCreateOutput, error) {
	if errRes := requireWorkflowCwd(cwd); errRes != nil {
		return errRes, workflowCreateOutput{}, nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errResult("name is required"), workflowCreateOutput{}, nil
	}
	scope, err := normalizeWorkflowScope(in.Scope)
	if err != nil {
		return errResult(err.Error()), workflowCreateOutput{}, nil
	}
	if err := validateSteps(in.Steps); err != nil {
		return errResult(err.Error()), workflowCreateOutput{}, nil
	}
	def := workflowDef{Name: name, Scope: scope, Description: strings.TrimSpace(in.Description), Steps: stepsViewToDef(in.Steps)}

	var collision bool
	if err := mutateWorkflows(cwd, func(items []workflowDef) ([]workflowDef, error) {
		for _, w := range items {
			if w.Name == name && workflowScopeTag(w.Scope) == scope {
				collision = true
				return items, nil
			}
		}
		return append(items, def), nil
	}); err != nil {
		return nil, workflowCreateOutput{}, fmt.Errorf("save: %w", err)
	}
	if collision {
		return errResult(fmt.Sprintf("workflow %q already exists in %s scope", name, scope)), workflowCreateOutput{}, nil
	}
	return okResult(fmt.Sprintf("workflow %q created in %s scope with %d step(s)", def.Name, scope, len(def.Steps))),
		workflowCreateOutput{Workflow: workflowDefToView(def)}, nil
}

func workflowEditCore(cwd string, in workflowEditInput) (*mcp.CallToolResult, workflowEditOutput, error) {
	if errRes := requireWorkflowCwd(cwd); errRes != nil {
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

	// Resolve the target up front: explicit scope wins, ambiguous
	// names without one are an error (mutations never guess).
	target, errRes := resolveWorkflowResult(cwd, name, in.Scope)
	if errRes != nil {
		return errRes, workflowEditOutput{}, nil
	}
	scope := workflowScopeTag(target.Scope)

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
	if err := mutateWorkflows(cwd, func(items []workflowDef) ([]workflowDef, error) {
		idx := -1
		for i, w := range items {
			if w.Name == name && workflowScopeTag(w.Scope) == scope {
				idx = i
				break
			}
		}
		if idx < 0 {
			notFound = true
			return items, nil
		}
		if newName != name {
			for i, w := range items {
				if i != idx && w.Name == newName && workflowScopeTag(w.Scope) == scope {
					collide = true
					return items, nil
				}
			}
		}
		w := items[idx]
		w.Name = newName
		if in.Description != nil {
			w.Description = strings.TrimSpace(*in.Description)
		}
		if in.Steps != nil {
			w.Steps = stepsViewToDef(*in.Steps)
		}
		items[idx] = w
		updated = w
		// Renamed workflows leave any disk session record under the
		// OLD name. The session key for issue-sourced workflows is
		// keyed on issue identity (not workflow name), so the
		// session bookkeeping survives the rename — only the
		// `Workflow` field on the disk session would still mention
		// the old name. That's acceptable for v1; the next terminal
		// status write will overwrite with the new name.
		return items, nil
	}); err != nil {
		return nil, workflowEditOutput{}, fmt.Errorf("save: %w", err)
	}
	if notFound {
		return errResult(fmt.Sprintf("workflow %q not found in %s scope", name, scope)), workflowEditOutput{}, nil
	}
	if collide {
		return errResult(fmt.Sprintf("another workflow in %s scope already uses the name %q", scope, newName)), workflowEditOutput{}, nil
	}
	return okResult(fmt.Sprintf("workflow %q updated (%s scope)", updated.Name, scope)),
		workflowEditOutput{Workflow: workflowDefToView(updated)}, nil
}

func workflowDeleteCore(cwd string, in workflowDeleteInput) (*mcp.CallToolResult, workflowDeleteOutput, error) {
	if errRes := requireWorkflowCwd(cwd); errRes != nil {
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
	target, errRes := resolveWorkflowResult(cwd, name, in.Scope)
	if errRes != nil {
		return errRes, workflowDeleteOutput{}, nil
	}
	scope := workflowScopeTag(target.Scope)

	var notFound bool
	if err := mutateWorkflows(cwd, func(items []workflowDef) ([]workflowDef, error) {
		idx := -1
		for i, w := range items {
			if w.Name == name && workflowScopeTag(w.Scope) == scope {
				idx = i
				break
			}
		}
		if idx < 0 {
			notFound = true
			return items, nil
		}
		return append(items[:idx], items[idx+1:]...), nil
	}); err != nil {
		return nil, workflowDeleteOutput{}, fmt.Errorf("save: %w", err)
	}
	if notFound {
		return errResult(fmt.Sprintf("workflow %q not found in %s scope", name, scope)), workflowDeleteOutput{}, nil
	}
	return okResult(fmt.Sprintf("workflow %q deleted from %s scope", name, scope)),
		workflowDeleteOutput{Deleted: true}, nil
}

func workflowCopyCore(cwd string, in workflowCopyInput) (*mcp.CallToolResult, workflowCopyOutput, error) {
	if errRes := requireWorkflowCwd(cwd); errRes != nil {
		return errRes, workflowCopyOutput{}, nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errResult("name is required"), workflowCopyOutput{}, nil
	}
	if strings.TrimSpace(in.To) == "" {
		return errResult("to is required: pass 'user' or 'repo'"), workflowCopyOutput{}, nil
	}
	dup, err := copyWorkflowDef(cwd, name, in.Scope, in.To, in.NewName)
	if err != nil {
		return errResult(err.Error()), workflowCopyOutput{}, nil
	}
	return okResult(fmt.Sprintf("workflow %q copied to %s scope as %q", name, workflowScopeTag(dup.Scope), dup.Name)),
		workflowCopyOutput{Workflow: workflowDefToView(dup)}, nil
}

func workflowRunCore(cwd string, tabID int, in workflowRunInput) (*mcp.CallToolResult, workflowRunOutput, error) {
	if errRes := requireWorkflowCwd(cwd); errRes != nil {
		return errRes, workflowRunOutput{}, nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errResult("name is required"), workflowRunOutput{}, nil
	}
	// append is the sole context channel into the fresh workflow session —
	// the run cannot see this conversation. Reject an empty/whitespace
	// value so the model is forced to submit the full plan instead of
	// dispatching a workflow with no brief.
	if strings.TrimSpace(in.Append) == "" {
		return errResult("append is required: the workflow runs in a fresh session with no access to this conversation, so you must submit the full plan and context (goal, steps, file paths, constraints, acceptance criteria) the workflow needs to execute the task end to end"), workflowRunOutput{}, nil
	}
	w, errRes := resolveWorkflowResult(cwd, name, in.Scope)
	if errRes != nil {
		return errRes, workflowRunOutput{}, nil
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

	source := textWorkflowSource(tabID, in.Append)
	startedAt := time.Now().UTC()
	if err := mcpSpawnWorkflowTab(spawnWorkflowTabMsg{
		OriginTabID: tabID,
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
