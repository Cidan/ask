package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Cidan/ask/pkg/workflow"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	WorkflowListToolDescription = `List all workflows visible to the current project, across three scopes.

A workflow lives in one of three scopes: 'user' (machine-local, stored in ~/.config/ask/ask.json), 'repo' (one JSON file per workflow under <project root>/.ask/workflows/ — committed to the repo and shared with the team), or 'global' (one JSON file per workflow under ~/.config/ask/workflows/ — machine-local but visible from every project). The merged list is ordered global → repo → user; the same name may exist in more than one scope, and each item's 'scope' field disambiguates.

Returns each workflow's name, scope, description (what it's for and when to use it — judge fit against this), and its steps' (name, provider, model). Step prompts are omitted to keep the listing payload small — call workflow_get to see the full prompt for a specific workflow.`

	WorkflowGetToolDescription = `Get the full definition of a workflow including each step's prompt.

Pass scope ('user', 'repo', or 'global') to read a specific copy; with no scope the global copy wins when the name exists in multiple scopes (personal-wins — a global workflow is the user's explicit pick). Errors when the named workflow does not exist.`

	WorkflowCreateToolDescription = `Create a new workflow.

The name must be non-empty and not collide with any existing workflow in the chosen scope. scope picks where it is stored: 'user' (default — machine-local ask.json), 'repo' (one JSON file under <project root>/.ask/workflows/, committed and shared with the team), or 'global' (one JSON file under ~/.config/ask/workflows/, machine-local but visible from every project).

description is optional but strongly recommended: state what the workflow is FOR and when to use it (its trigger conditions, in plain words). That text is surfaced verbatim in workflow_list, and the agent judges whether the workflow fits a task against it — without a description it must guess intent from the step names, which is unreliable.`

	WorkflowEditToolDescription = `Edit an existing workflow in place (it stays in its scope).

Pass new_name to rename. Pass description to replace the workflow's purpose statement (empty string clears it). Pass steps to replace the entire steps array (full-replace semantics — no per-step CRUD). Omit a field to leave it unchanged.`

	WorkflowDeleteToolDescription = `Delete a workflow.

When the name exists in multiple scopes you must pass scope to pick which copy to delete.`

	WorkflowCopyToolDescription = `Copy a workflow between scopes (or duplicate it within one).

'to' is the destination scope: 'repo' makes a workflow repo-local (a committed JSON file under <project root>/.ask/workflows/ that the whole team can use), 'user' copies it into the machine-local ask.json, 'global' copies it into ~/.config/ask/workflows/ (machine-local, visible from every project).`
)

type WorkflowListInput struct{}

type WorkflowInnerListStepView struct {
	Name     string `json:"name" jsonschema:"step name"`
	Provider string `json:"provider" jsonschema:"provider id from ask's provider registry"`
	Model    string `json:"model,omitempty" jsonschema:"model id (empty = provider default)"`
}

type WorkflowListStepView struct {
	Name          string                      `json:"name" jsonschema:"step name"`
	Kind          string                      `json:"kind,omitempty" jsonschema:"empty for an agent step; 'loop' for a loop container"`
	Provider      string                      `json:"provider,omitempty" jsonschema:"provider id from ask's provider registry; agent steps only"`
	Model         string                      `json:"model,omitempty" jsonschema:"model id (empty = provider default)"`
	Steps         []WorkflowInnerListStepView `json:"steps,omitempty" jsonschema:"inner steps run each iteration; loop steps only"`
	MaxIterations int                         `json:"maxIterations,omitempty" jsonschema:"iteration cap; loop steps only (0 = default)"`
}

type WorkflowListItem struct {
	Name        string                 `json:"name" jsonschema:"workflow name"`
	Scope       string                 `json:"scope" jsonschema:"where the workflow is stored: 'user', 'repo', 'global', or 'plugin' (read-only)"`
	Plugin      string                 `json:"plugin,omitempty" jsonschema:"name@marketplace for plugin workflows"`
	Description string                 `json:"description,omitempty" jsonschema:"the author's statement of what this workflow is for and when to use it"`
	Steps       []WorkflowListStepView `json:"steps" jsonschema:"steps in execution order"`
}

type WorkflowListOutput struct {
	Workflows []WorkflowListItem `json:"workflows"`
}

type WorkflowGetInput struct {
	Name  string `json:"name" jsonschema:"workflow name"`
	Scope string `json:"scope,omitempty" jsonschema:"optional scope ('user', 'repo', or 'global')"`
}

type WorkflowInnerStepView struct {
	Name     string `json:"name" jsonschema:"step name"`
	Provider string `json:"provider" jsonschema:"provider id from ask's provider registry"`
	Model    string `json:"model,omitempty" jsonschema:"model id (empty = provider default)"`
	Prompt   string `json:"prompt,omitempty" jsonschema:"user-authored prompt for this step"`
}

type WorkflowStepView struct {
	Name          string                  `json:"name" jsonschema:"step name"`
	Kind          string                  `json:"kind,omitempty" jsonschema:"empty for an agent step; 'loop' for a loop container"`
	Provider      string                  `json:"provider,omitempty" jsonschema:"provider id"`
	Model         string                  `json:"model,omitempty" jsonschema:"model id"`
	Prompt        string                  `json:"prompt,omitempty" jsonschema:"user-authored prompt"`
	Steps         []WorkflowInnerStepView `json:"steps,omitempty" jsonschema:"inner agent steps"`
	MaxIterations int                     `json:"maxIterations,omitempty" jsonschema:"iteration cap"`
	ExitCondition string                  `json:"exitCondition,omitempty" jsonschema:"free-text goal"`
}

type WorkflowDefView struct {
	Name        string             `json:"name"`
	Scope       string             `json:"scope"`
	Plugin      string             `json:"plugin,omitempty"`
	Description string             `json:"description,omitempty"`
	Steps       []WorkflowStepView `json:"steps"`
}

type WorkflowGetOutput struct {
	Workflow WorkflowDefView `json:"workflow"`
}

type WorkflowCreateInput struct {
	Name        string             `json:"name" jsonschema:"new workflow name"`
	Scope       string             `json:"scope,omitempty" jsonschema:"'user', 'repo', or 'global'"`
	Description string             `json:"description,omitempty" jsonschema:"what this workflow is for"`
	Steps       []WorkflowStepView `json:"steps,omitempty" jsonschema:"steps to create"`
}

type WorkflowCreateOutput struct {
	Workflow WorkflowDefView `json:"workflow"`
}

type WorkflowEditInput struct {
	Name        string              `json:"name" jsonschema:"existing workflow name"`
	Scope       string              `json:"scope,omitempty" jsonschema:"scope holding the workflow"`
	NewName     string              `json:"new_name,omitempty" jsonschema:"optional new name"`
	Description *string             `json:"description,omitempty" jsonschema:"if provided, replaces description"`
	Steps       *[]WorkflowStepView `json:"steps,omitempty" jsonschema:"if provided, replaces steps"`
}

type WorkflowEditOutput struct {
	Workflow WorkflowDefView `json:"workflow"`
}

type WorkflowDeleteInput struct {
	Name  string `json:"name" jsonschema:"workflow name to delete"`
	Scope string `json:"scope,omitempty" jsonschema:"scope holding the workflow"`
}

type WorkflowDeleteOutput struct {
	Deleted bool `json:"deleted"`
}

type WorkflowCopyInput struct {
	Name    string `json:"name" jsonschema:"workflow name to copy"`
	Scope   string `json:"scope,omitempty" jsonschema:"scope holding source"`
	To      string `json:"to" jsonschema:"destination scope: 'user', 'repo', or 'global'"`
	NewName string `json:"new_name,omitempty" jsonschema:"optional name for the copy"`
}

type WorkflowCopyOutput struct {
	Workflow WorkflowDefView `json:"workflow"`
}

// WorkflowTools returns the workflow core tools.
func WorkflowTools(env *ToolEnv) []Tool {
	cwd := func() string { return env.Cwd }
	return []Tool{
		NativeBridgeTool("workflow_list", WorkflowListToolDescription,
			func(_ context.Context, in WorkflowListInput) (*mcp.CallToolResult, WorkflowListOutput, error) {
				return WorkflowListCore(cwd(), in)
			}),
		NativeBridgeTool("workflow_get", WorkflowGetToolDescription,
			func(_ context.Context, in WorkflowGetInput) (*mcp.CallToolResult, WorkflowGetOutput, error) {
				return WorkflowGetCore(cwd(), in)
			}),
		NativeBridgeTool("workflow_create", WorkflowCreateToolDescription,
			func(_ context.Context, in WorkflowCreateInput) (*mcp.CallToolResult, WorkflowCreateOutput, error) {
				return WorkflowCreateCore(cwd(), in)
			}),
		NativeBridgeTool("workflow_edit", WorkflowEditToolDescription,
			func(_ context.Context, in WorkflowEditInput) (*mcp.CallToolResult, WorkflowEditOutput, error) {
				return WorkflowEditCore(cwd(), in)
			}),
		NativeBridgeTool("workflow_delete", WorkflowDeleteToolDescription,
			func(_ context.Context, in WorkflowDeleteInput) (*mcp.CallToolResult, WorkflowDeleteOutput, error) {
				return WorkflowDeleteCore(cwd(), in)
			}),
		NativeBridgeTool("workflow_copy", WorkflowCopyToolDescription,
			func(_ context.Context, in WorkflowCopyInput) (*mcp.CallToolResult, WorkflowCopyOutput, error) {
				return WorkflowCopyCore(cwd(), in)
			}),
	}
}

func WorkflowListCore(cwd string, in WorkflowListInput) (*mcp.CallToolResult, WorkflowListOutput, error) {
	all := workflow.ListAll(cwd)
	out := WorkflowListOutput{Workflows: make([]WorkflowListItem, 0, len(all))}
	for _, w := range all {
		steps := make([]WorkflowListStepView, 0, len(w.Steps))
		for _, s := range w.Steps {
			inner := make([]WorkflowInnerListStepView, 0, len(s.Steps))
			for _, is := range s.Steps {
				inner = append(inner, WorkflowInnerListStepView{
					Name:     is.Name,
					Provider: is.Provider,
					Model:    is.Model,
				})
			}
			steps = append(steps, WorkflowListStepView{
				Name:          s.Name,
				Kind:          s.Kind,
				Provider:      s.Provider,
				Model:         s.Model,
				Steps:         inner,
				MaxIterations: s.MaxIterations,
			})
		}
		out.Workflows = append(out.Workflows, WorkflowListItem{
			Name:        w.Name,
			Scope:       string(w.Scope),
			Plugin:      w.Plugin,
			Description: w.Description,
			Steps:       steps,
		})
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: renderWorkflowList(all)}}}, out, nil
}

func stepViewsToSteps(views []WorkflowStepView) []workflow.Step {
	if views == nil {
		return nil
	}
	steps := make([]workflow.Step, 0, len(views))
	for _, s := range views {
		var inner []workflow.Step
		if s.Steps != nil {
			inner = make([]workflow.Step, 0, len(s.Steps))
			for _, is := range s.Steps {
				inner = append(inner, workflow.Step{
					Name:     is.Name,
					Provider: is.Provider,
					Model:    is.Model,
					Prompt:   is.Prompt,
				})
			}
		}
		steps = append(steps, workflow.Step{
			Name:          s.Name,
			Kind:          s.Kind,
			Provider:      s.Provider,
			Model:         s.Model,
			Prompt:        s.Prompt,
			Steps:         inner,
			MaxIterations: s.MaxIterations,
			ExitCondition: s.ExitCondition,
		})
	}
	return steps
}

func stepsToStepViews(steps []workflow.Step) []WorkflowStepView {
	if steps == nil {
		return make([]WorkflowStepView, 0)
	}
	views := make([]WorkflowStepView, 0, len(steps))
	for _, s := range steps {
		var inner []WorkflowInnerStepView
		if s.Steps != nil {
			inner = make([]WorkflowInnerStepView, 0, len(s.Steps))
			for _, is := range s.Steps {
				inner = append(inner, WorkflowInnerStepView{
					Name:     is.Name,
					Provider: is.Provider,
					Model:    is.Model,
					Prompt:   is.Prompt,
				})
			}
		}
		views = append(views, WorkflowStepView{
			Name:          s.Name,
			Kind:          s.Kind,
			Provider:      s.Provider,
			Model:         s.Model,
			Prompt:        s.Prompt,
			Steps:         inner,
			MaxIterations: s.MaxIterations,
			ExitCondition: s.ExitCondition,
		})
	}
	return views
}

func defToDefView(d workflow.Def) WorkflowDefView {
	return WorkflowDefView{
		Name:        d.Name,
		Scope:       string(d.Scope),
		Plugin:      d.Plugin,
		Description: d.Description,
		Steps:       stepsToStepViews(d.Steps),
	}
}

// workflowScopeLabel names a workflow's scope for display, defaulting to
// "user" when unset (the zero value ListAll assigns to user-scope items).
func workflowScopeLabel(d workflow.Def) string {
	if d.Scope == "" {
		return string(workflow.ScopeUser)
	}
	return string(d.Scope)
}

// stepMeta renders a step's execution attributes: kind plus provider/model
// for agent steps, or iteration cap for loop steps.
func stepMeta(s workflow.Step) string {
	if s.IsLoop() {
		parts := []string{"loop"}
		if s.MaxIterations > 0 {
			parts = append(parts, fmt.Sprintf("maxIterations=%d", s.MaxIterations))
		}
		return "[" + strings.Join(parts, " · ") + "]"
	}
	parts := []string{"agent"}
	if s.Provider != "" {
		parts = append(parts, "provider="+s.Provider)
	}
	if s.Model != "" {
		parts = append(parts, "model="+s.Model)
	}
	return "[" + strings.Join(parts, " · ") + "]"
}

// writeWorkflowStep renders one step (and its inner loop steps) with the
// full prompt so the model reads the complete definition from the tool's
// text field, not only from structured data.
func writeWorkflowStep(b *strings.Builder, s workflow.Step, label, indent string) {
	fmt.Fprintf(b, "%s%s. %s  %s\n", indent, label, s.Name, stepMeta(s))
	if ec := strings.TrimSpace(s.ExitCondition); ec != "" {
		fmt.Fprintf(b, "%s   Exit condition: %s\n", indent, ec)
	}
	if p := strings.TrimSpace(s.Prompt); p != "" {
		fmt.Fprintf(b, "%s   Prompt:\n", indent)
		for _, line := range strings.Split(p, "\n") {
			fmt.Fprintf(b, "%s     %s\n", indent, line)
		}
	}
	for j, inner := range s.Steps {
		writeWorkflowStep(b, inner, fmt.Sprintf("%s.%d", label, j+1), indent+"   ")
	}
}

// renderWorkflowDef renders a workflow's full definition — name, scope,
// description, and every step with its prompt — as readable text. This is
// the tool's human-readable content: it reaches the model on every provider
// (the claudecode bridge and session resume forward only the text field),
// where the structured data payload would be dropped.
func renderWorkflowDef(d workflow.Def) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow: %s (scope: %s)", d.Name, workflowScopeLabel(d))
	if d.Plugin != "" {
		fmt.Fprintf(&b, " [plugin: %s]", d.Plugin)
	}
	b.WriteString("\n")
	desc := strings.TrimSpace(d.Description)
	if desc == "" {
		desc = "(none)"
	}
	fmt.Fprintf(&b, "Description: %s\n\nSteps:\n", desc)
	for i, s := range d.Steps {
		writeWorkflowStep(&b, s, fmt.Sprintf("%d", i+1), "")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderWorkflowList renders every workflow's name, scope, description, and
// step outline (kinds only, prompts omitted) as readable text — the listing
// the model reads to judge which workflow fits a task.
func renderWorkflowList(defs []workflow.Def) string {
	if len(defs) == 0 {
		return "No workflows are defined."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d workflow(s):\n", len(defs))
	for _, d := range defs {
		fmt.Fprintf(&b, "\n%s (scope: %s)", d.Name, workflowScopeLabel(d))
		if d.Plugin != "" {
			fmt.Fprintf(&b, " [plugin: %s]", d.Plugin)
		}
		b.WriteString("\n")
		desc := strings.TrimSpace(d.Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "  Description: %s\n", desc)
		outline := make([]string, 0, len(d.Steps))
		for _, s := range d.Steps {
			kind := "agent"
			if s.IsLoop() {
				kind = "loop"
			}
			outline = append(outline, fmt.Sprintf("%s [%s]", s.Name, kind))
		}
		fmt.Fprintf(&b, "  Steps: %s\n", strings.Join(outline, " → "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func WorkflowGetCore(cwd string, in WorkflowGetInput) (*mcp.CallToolResult, WorkflowGetOutput, error) {
	w, err := workflow.ResolveByName(cwd, in.Name, workflow.Scope(in.Scope))
	if err != nil {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}, WorkflowGetOutput{}, nil
	}
	view := defToDefView(w)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: renderWorkflowDef(w)}}}, WorkflowGetOutput{Workflow: view}, nil
}

func WorkflowCreateCore(cwd string, in WorkflowCreateInput) (*mcp.CallToolResult, WorkflowCreateOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "workflow name is required"}}, IsError: true}, WorkflowCreateOutput{}, nil
	}
	scope, err := workflow.NormalizeScope(workflow.Scope(in.Scope))
	if err != nil {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}, WorkflowCreateOutput{}, nil
	}
	steps := stepViewsToSteps(in.Steps)
	d := workflow.Def{
		Name:        name,
		Scope:       scope,
		Description: in.Description,
		Steps:       steps,
	}
	err = workflow.MutateWorkflows(cwd, func(items []workflow.Def) ([]workflow.Def, error) {
		for _, existing := range items {
			if existing.Name == name && existing.Scope == scope {
				return nil, fmt.Errorf("workflow %q already exists in %s scope", name, scope)
			}
		}
		return append(items, d), nil
	})
	if err != nil {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}, WorkflowCreateOutput{}, nil
	}
	view := defToDefView(d)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("created workflow %s in %s scope", name, scope)}}}, WorkflowCreateOutput{Workflow: view}, nil
}

func WorkflowEditCore(cwd string, in WorkflowEditInput) (*mcp.CallToolResult, WorkflowEditOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "workflow name is required"}}, IsError: true}, WorkflowEditOutput{}, nil
	}
	var updatedDef workflow.Def
	err := workflow.MutateWorkflows(cwd, func(items []workflow.Def) ([]workflow.Def, error) {
		foundIdx := -1
		for i, existing := range items {
			if existing.Name == name {
				if in.Scope == "" || string(existing.Scope) == in.Scope {
					if foundIdx >= 0 {
						return nil, workflow.ErrWorkflowAmbiguous
					}
					foundIdx = i
				}
			}
		}
		if foundIdx < 0 {
			return nil, fmt.Errorf("workflow %q not found", name)
		}
		d := items[foundIdx]
		if in.NewName != "" {
			d.Name = in.NewName
		}
		if in.Description != nil {
			d.Description = *in.Description
		}
		if in.Steps != nil {
			d.Steps = stepViewsToSteps(*in.Steps)
		}
		items[foundIdx] = d
		updatedDef = d
		return items, nil
	})
	if err != nil {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}, WorkflowEditOutput{}, nil
	}
	view := defToDefView(updatedDef)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("updated workflow %s", updatedDef.Name)}}}, WorkflowEditOutput{Workflow: view}, nil
}

func WorkflowDeleteCore(cwd string, in WorkflowDeleteInput) (*mcp.CallToolResult, WorkflowDeleteOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "workflow name is required"}}, IsError: true}, WorkflowDeleteOutput{}, nil
	}
	err := workflow.MutateWorkflows(cwd, func(items []workflow.Def) ([]workflow.Def, error) {
		foundIdx := -1
		for i, existing := range items {
			if existing.Name == name {
				if in.Scope == "" || string(existing.Scope) == in.Scope {
					if foundIdx >= 0 {
						return nil, workflow.ErrWorkflowAmbiguous
					}
					foundIdx = i
				}
			}
		}
		if foundIdx < 0 {
			return nil, fmt.Errorf("workflow %q not found", name)
		}
		return append(items[:foundIdx], items[foundIdx+1:]...), nil
	})
	if err != nil {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}, WorkflowDeleteOutput{}, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("deleted workflow %s", name)}}}, WorkflowDeleteOutput{Deleted: true}, nil
}

func WorkflowCopyCore(cwd string, in WorkflowCopyInput) (*mcp.CallToolResult, WorkflowCopyOutput, error) {
	src, err := workflow.ResolveByName(cwd, in.Name, workflow.Scope(in.Scope))
	if err != nil {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}, WorkflowCopyOutput{}, nil
	}
	dstScope, err := workflow.NormalizeScope(workflow.Scope(in.To))
	if err != nil {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}, WorkflowCopyOutput{}, nil
	}
	dstName := src.Name
	if in.NewName != "" {
		dstName = in.NewName
	}
	copied := src
	copied.Name = dstName
	copied.Scope = dstScope

	err = workflow.MutateWorkflows(cwd, func(items []workflow.Def) ([]workflow.Def, error) {
		for _, existing := range items {
			if existing.Name == dstName && existing.Scope == dstScope {
				return nil, fmt.Errorf("workflow %q already exists in destination scope %s", dstName, dstScope)
			}
		}
		return append(items, copied), nil
	})
	if err != nil {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}, WorkflowCopyOutput{}, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("copied workflow %s to %s scope as %s", in.Name, dstScope, dstName)}}}, WorkflowCopyOutput{Workflow: defToDefView(copied)}, nil
}
