package main

import (
	"fmt"
	"strings"

	"github.com/Cidan/ask/pkg/workflow"
)

// workflow_store.go is a thin adapter over the canonical three-scope
// workflow store in pkg/workflow. The TUI's currency is config.WorkflowDef
// (aliased as workflowDef); pkg/workflow deals in workflow.Def. Every
// function here converts at the boundary and delegates the real work —
// file IO, ask.json persistence, dir sync, scope resolution — to the
// package the agent-facing workflow_* tools already use, so there is a
// single store implementation shared by the builder UI and the agent.
//
// The three scopes (see pkg/workflow/store.go for the full contract):
//   - user:   projectConfig.Workflows.Items in ~/.config/ask/ask.json
//   - repo:   one JSON file per workflow under <root>/.ask/workflows/
//   - global: one JSON file per workflow under ~/.config/ask/workflows/

// errWorkflowAmbiguous is returned by resolveWorkflowByName when the name
// exists in multiple scopes and the caller didn't pick one.
var errWorkflowAmbiguous = workflow.ErrWorkflowAmbiguous

func toPkgWorkflowDefs(items []workflowDef) []workflow.Def {
	if items == nil {
		return nil
	}
	out := make([]workflow.Def, len(items))
	for i, w := range items {
		out[i] = workflow.ConfigDefToDef(w)
	}
	return out
}

func fromPkgWorkflowDefs(defs []workflow.Def) []workflowDef {
	if defs == nil {
		return nil
	}
	out := make([]workflowDef, len(defs))
	for i, d := range defs {
		out[i] = workflow.DefToConfigDef(d)
	}
	return out
}

// workflowsRepoDir returns the absolute repo-local workflows dir for cwd.
func workflowsRepoDir(cwd string) string { return workflow.RepoDir(cwd) }

// workflowsGlobalDir returns the absolute global workflows dir under
// ~/.config/ask/workflows/.
func workflowsGlobalDir() string { return workflow.GlobalDir() }

// normalizeWorkflowScope maps "" to user and validates the rest.
func normalizeWorkflowScope(scope string) (string, error) {
	s, err := workflow.NormalizeScope(workflow.Scope(scope))
	return string(s), err
}

// workflowFileName maps a workflow name onto a filesystem-safe filename stem.
func workflowFileName(name string) string { return workflow.FileName(name) }

func loadRepoWorkflows(cwd string) []workflowDef {
	return fromPkgWorkflowDefs(workflow.LoadFileWorkflows(workflow.RepoDir(cwd), workflow.ScopeRepo))
}

func loadGlobalWorkflows() []workflowDef {
	return fromPkgWorkflowDefs(workflow.LoadFileWorkflows(workflow.GlobalDir(), workflow.ScopeGlobal))
}

// listAllWorkflows merges the three scopes: global first (personal-wins),
// then repo (project-wins), then user.
func listAllWorkflows(cwd string) []workflowDef {
	return fromPkgWorkflowDefs(workflow.ListAll(cwd))
}

// findWorkflow returns the named workflow in the given scope. scope ""
// walks the merged list (global → repo → user) and returns the first
// match, so a name present in multiple scopes resolves to global.
func findWorkflow(cwd, name, scope string) (workflowDef, bool) {
	for _, w := range listAllWorkflows(cwd) {
		if w.Name != name {
			continue
		}
		if scope == "" || w.Scope == scope {
			return w, true
		}
	}
	return workflowDef{}, false
}

// resolveWorkflowByName resolves name (+ optional scope) to exactly one
// workflow. With an explicit scope it's a plain scoped lookup. Without
// one, a name living in more than one scope is an error — mutating
// surfaces must never guess which copy to touch.
func resolveWorkflowByName(cwd, name, scope string) (workflowDef, error) {
	if scope != "" {
		norm, err := normalizeWorkflowScope(scope)
		if err != nil {
			return workflowDef{}, err
		}
		w, ok := findWorkflow(cwd, name, norm)
		if !ok {
			return workflowDef{}, fmt.Errorf("workflow %q not found in %s scope", name, norm)
		}
		return w, nil
	}
	var matches []workflowDef
	for _, w := range listAllWorkflows(cwd) {
		if w.Name == name {
			matches = append(matches, w)
		}
	}
	switch len(matches) {
	case 0:
		return workflowDef{}, fmt.Errorf("workflow %q not found", name)
	case 1:
		return matches[0], nil
	}
	return workflowDef{}, fmt.Errorf("workflow %q: %w", name, errWorkflowAmbiguous)
}

// saveAllWorkflows persists the full merged list, routing each def by its
// Scope tag (user → ask.json, repo/global → their dirs) under the config
// lock. This is the single write path for the builder UI.
func saveAllWorkflows(cwd string, items []workflowDef) error {
	return workflow.SaveAll(cwd, toPkgWorkflowDefs(items))
}

// mutateWorkflows runs one read-modify-write cycle against the merged
// workflow list under the config lock.
func mutateWorkflows(cwd string, fn func(items []workflowDef) ([]workflowDef, error)) error {
	return workflow.MutateWorkflows(cwd, func(defs []workflow.Def) ([]workflow.Def, error) {
		next, err := fn(fromPkgWorkflowDefs(defs))
		if err != nil {
			return nil, err
		}
		return toPkgWorkflowDefs(next), nil
	})
}

// copyWorkflowDef copies the named workflow into toScope under newName
// (empty newName keeps the original name). fromScope "" resolves like
// resolveWorkflowByName (explicit on ambiguity). Errors when the target
// scope already holds the target name — the caller resolves conflicts by
// passing a different newName.
func copyWorkflowDef(cwd, name, fromScope, toScope, newName string) (workflowDef, error) {
	target, err := normalizeWorkflowScope(toScope)
	if err != nil {
		return workflowDef{}, err
	}
	var dup workflowDef
	err = mutateWorkflows(cwd, func(items []workflowDef) ([]workflowDef, error) {
		src, err := resolveWorkflowByName(cwd, name, fromScope)
		if err != nil {
			return nil, err
		}
		targetName := strings.TrimSpace(newName)
		if targetName == "" {
			targetName = src.Name
		}
		if src.Scope == target && targetName == src.Name {
			return nil, fmt.Errorf("workflow %q is already in %s scope; pass new_name to duplicate it there", name, target)
		}
		for _, w := range items {
			if w.Scope == target && w.Name == targetName {
				return nil, fmt.Errorf("workflow %q already exists in %s scope; pass new_name to copy under a different name", targetName, target)
			}
		}
		dup = src
		dup.Name = targetName
		dup.Scope = target
		dup.Steps = cloneWorkflowSteps(src.Steps)
		return append(items, dup), nil
	})
	if err != nil {
		return workflowDef{}, err
	}
	return dup, nil
}

// cloneWorkflowSteps deep-copies a step tree so a copied workflow never
// shares loop inner-step slices with its source.
func cloneWorkflowSteps(in []workflowStep) []workflowStep {
	if in == nil {
		return nil
	}
	out := make([]workflowStep, len(in))
	for i, s := range in {
		s.Steps = cloneWorkflowSteps(s.Steps)
		out[i] = s
	}
	return out
}

// workflowScopeTag is the short UI label for a scope. Defs that predate
// scoping (empty Scope) read as user.
func workflowScopeTag(scope string) string {
	switch scope {
	case workflowScopeRepo:
		return workflowScopeRepo
	case workflowScopeGlobal:
		return workflowScopeGlobal
	}
	return workflowScopeUser
}
