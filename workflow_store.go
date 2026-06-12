package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// workflow_store.go is the two-scope persistence layer for workflow
// definitions:
//
//   - user scope: projectConfig.Workflows.Items inside
//     ~/.config/ask/ask.json (machine-local, the pre-scope location).
//   - repo scope: one JSON file per workflow under
//     <projectRoot>/.ask/workflows/, committed to the repo so the
//     whole team shares them.
//
// Every read merges the two scopes (repo first — the "project wins"
// convention skills and subagents already follow); every write routes
// by the def's Scope tag. Names are unique within a scope; the same
// name MAY exist in both scopes, in which case name-only resolution
// prefers repo and the mutating tool surface demands an explicit
// scope (resolveWorkflowByName).
//
// All mutations serialise through withConfigLock — the user scope
// needs it for the load → mutate → save cycle on ask.json, and
// running the repo-dir sync under the same lock means a concurrent
// builder commit and MCP workflow_edit can't interleave file writes.

// workflowsRepoDirName is the repo-local directory, relative to the
// project root.
const workflowsRepoDirName = ".ask/workflows"

// workflowsRepoDir returns the absolute repo-local workflows dir for
// cwd. Empty when cwd is empty.
func workflowsRepoDir(cwd string) string {
	root := projectRoot(cwd)
	if root == "" {
		return ""
	}
	return filepath.Join(root, filepath.FromSlash(workflowsRepoDirName))
}

// normalizeWorkflowScope maps "" to user (the pre-scope default) and
// validates the rest. Returns an error for anything else so tool
// callers get a clear message instead of a silent fallback.
func normalizeWorkflowScope(scope string) (string, error) {
	switch strings.TrimSpace(scope) {
	case "", workflowScopeUser:
		return workflowScopeUser, nil
	case workflowScopeRepo:
		return workflowScopeRepo, nil
	}
	return "", fmt.Errorf("unknown scope %q (use %q or %q)", scope, workflowScopeUser, workflowScopeRepo)
}

// workflowFileName maps a workflow name onto a filesystem-safe
// filename stem. Runes outside [a-zA-Z0-9._-] become '-'; runs
// collapse; empty results fall back to "workflow".
func workflowFileName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
			lastDash = r == '-'
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	stem := strings.Trim(b.String(), "-.")
	if stem == "" {
		stem = "workflow"
	}
	return stem
}

// loadRepoWorkflows reads every *.json under the repo-local dir,
// skipping files that don't parse or carry an empty name (debugLog
// only — a malformed committed file must not take the feature down).
// Duplicate names within the dir keep the first (filename order) and
// skip the rest. Results are tagged Scope=repo and sorted by name so
// listings are stable regardless of directory iteration order.
func loadRepoWorkflows(cwd string) []workflowDef {
	dir := workflowsRepoDir(cwd)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	seen := map[string]bool{}
	var out []workflowDef
	for _, fname := range names {
		path := filepath.Join(dir, fname)
		data, err := os.ReadFile(path)
		if err != nil {
			debugLog("repo workflow %s: read: %v", path, err)
			continue
		}
		var def workflowDef
		if err := json.Unmarshal(data, &def); err != nil {
			debugLog("repo workflow %s: parse: %v", path, err)
			continue
		}
		def.Name = strings.TrimSpace(def.Name)
		if def.Name == "" {
			debugLog("repo workflow %s: skipped, name is required", path)
			continue
		}
		if seen[def.Name] {
			debugLog("repo workflow %s: skipped, duplicate name %q", path, def.Name)
			continue
		}
		seen[def.Name] = true
		def.Scope = workflowScopeRepo
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// loadUserWorkflows reads the user-scope list from ask.json, tagged
// Scope=user, in config order. The error surfaces a broken ask.json
// to the tool layer; UI listing paths drop it (an unreadable config
// degrades to "no user workflows" rather than a dead screen).
func loadUserWorkflows(cwd string) ([]workflowDef, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	pc := loadProjectConfig(cfg, cwd)
	out := make([]workflowDef, 0, len(pc.Workflows.Items))
	for _, w := range pc.Workflows.Items {
		w.Scope = workflowScopeUser
		out = append(out, w)
	}
	return out, nil
}

// listAllWorkflows merges the two scopes: repo first (project wins),
// then user, each in their scope-stable order.
func listAllWorkflows(cwd string) []workflowDef {
	user, _ := loadUserWorkflows(cwd)
	return append(loadRepoWorkflows(cwd), user...)
}

// findWorkflow returns the named workflow in the given scope.
// scope "" searches repo first, then user (project-wins resolution).
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

// errWorkflowAmbiguous is returned by resolveWorkflowByName when the
// name exists in both scopes and the caller didn't pick one.
var errWorkflowAmbiguous = errors.New("workflow exists in both scopes; pass scope to pick one")

// resolveWorkflowByName resolves name (+ optional scope) to exactly
// one workflow. With an explicit scope it's a plain scoped lookup.
// Without one, a name living in both scopes is an error — mutating
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

// saveAllWorkflows persists the full merged list: user-scope defs
// replace projectConfig.Workflows.Items (in list order), repo-scope
// defs are synced onto <root>/.ask/workflows/ (write each def to
// <sanitized-name>.json, remove files no longer claimed by any def).
// Defs with an empty Scope count as user. Files whose content is
// already current are left untouched so VCS mtimes don't churn.
//
// This is the single write path for the builder UI and the
// scope-mutation helpers below — both build the desired end state in
// memory and hand it here, which makes rename/move/copy/delete all
// the same operation: "make disk look like this".
func saveAllWorkflows(cwd string, items []workflowDef) error {
	return withConfigLock(func() error {
		return saveAllWorkflowsLocked(cwd, items)
	})
}

// mutateWorkflows runs one read-modify-write cycle against the merged
// workflow list under the config lock: fn receives the current merged
// list and returns the desired end state (or an error to abort with
// nothing written). This is the primitive every workflow mutation
// (tool cores, copy) goes through so concurrent callers can't
// interleave their load → mutate → save cycles.
func mutateWorkflows(cwd string, fn func(items []workflowDef) ([]workflowDef, error)) error {
	return withConfigLock(func() error {
		next, err := fn(listAllWorkflows(cwd))
		if err != nil {
			return err
		}
		return saveAllWorkflowsLocked(cwd, next)
	})
}

// saveAllWorkflowsLocked is saveAllWorkflows without the lock; the
// caller must hold configFileMu (withConfigLock is not reentrant).
func saveAllWorkflowsLocked(cwd string, items []workflowDef) error {
	var user, repo []workflowDef
	for _, w := range items {
		if w.Scope == workflowScopeRepo {
			repo = append(repo, w)
		} else {
			user = append(user, w)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	pc := loadProjectConfig(cfg, cwd)
	if len(user) == 0 {
		pc.Workflows.Items = nil
	} else {
		stored := make([]workflowDef, 0, len(user))
		for _, w := range user {
			w.Scope = ""
			stored = append(stored, w)
		}
		pc.Workflows.Items = stored
	}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		return err
	}
	return syncRepoWorkflowFiles(cwd, repo)
}

// syncRepoWorkflowFiles makes the repo-local dir contain exactly
// `defs`: one file per def named after the workflow (suffixed -2, -3…
// when two names sanitize identically), stale files removed. The dir
// is created on first write and removed when it empties out, so a
// project that never uses repo workflows never grows a .ask/ tree.
func syncRepoWorkflowFiles(cwd string, defs []workflowDef) error {
	dir := workflowsRepoDir(cwd)
	if dir == "" {
		if len(defs) > 0 {
			return errors.New("no project root to store repo workflows under")
		}
		return nil
	}
	claimed := map[string]bool{}
	if len(defs) > 0 {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	for _, def := range defs {
		stem := workflowFileName(def.Name)
		fname := stem + ".json"
		for i := 2; claimed[fname]; i++ {
			fname = fmt.Sprintf("%s-%d.json", stem, i)
		}
		claimed[fname] = true
		def.Scope = ""
		data, err := json.MarshalIndent(def, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal workflow %q: %w", def.Name, err)
		}
		data = append(data, '\n')
		path := filepath.Join(dir, fname)
		if cur, err := os.ReadFile(path); err == nil && string(cur) == string(data) {
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	remaining := 0
	for _, e := range entries {
		if e.IsDir() {
			remaining++
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") && !claimed[e.Name()] {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return fmt.Errorf("remove stale %s: %w", e.Name(), err)
			}
			continue
		}
		remaining++
	}
	if remaining == 0 && len(claimed) == 0 {
		_ = os.Remove(dir) // best-effort tidy; fails when non-empty
	}
	return nil
}

// copyWorkflowDef copies the named workflow into toScope under
// newName (empty newName keeps the original name). fromScope ""
// resolves like resolveWorkflowByName (explicit on ambiguity).
// Errors when the target scope already has a workflow by the target
// name — the caller resolves conflicts by passing a different
// newName (tools) or pre-computing a unique one (builder UI).
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

// cloneWorkflowSteps deep-copies a step tree so a copied workflow
// never shares loop inner-step slices with its source.
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

// workflowScopeTag is the short UI label for a scope ("repo"/"user").
// Defs that predate scoping (empty Scope) read as user.
func workflowScopeTag(scope string) string {
	if scope == workflowScopeRepo {
		return workflowScopeRepo
	}
	return workflowScopeUser
}
