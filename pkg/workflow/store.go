package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/plugin"
)

const WorkflowsRepoDirName = ".ask/workflows"
const WorkflowsGlobalDirName = "workflows"

// RepoDir returns the absolute repo-local workflows directory for cwd.
func RepoDir(cwd string) string {
	if cwd == "" {
		return ""
	}
	root := config.ProjectRoot(cwd)
	if root == "" {
		root = cwd
	}
	return filepath.Join(root, filepath.FromSlash(WorkflowsRepoDirName))
}

// GlobalDir returns the absolute global workflows directory (~/.config/ask/workflows/).
func GlobalDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "ask", WorkflowsGlobalDirName)
}

// NormalizeScope maps empty string to ScopeUser and validates Scope.
func NormalizeScope(s Scope) (Scope, error) {
	switch strings.TrimSpace(string(s)) {
	case "", string(ScopeUser):
		return ScopeUser, nil
	case string(ScopeRepo):
		return ScopeRepo, nil
	case string(ScopeGlobal):
		return ScopeGlobal, nil
	case string(ScopePlugin):
		return "", fmt.Errorf("plugin workflows are read-only; copy the workflow into %q, %q, or %q scope first",
			ScopeUser, ScopeRepo, ScopeGlobal)
	default:
		return "", fmt.Errorf("unknown workflow scope %q: valid scopes are %q, %q, %q",
			s, ScopeUser, ScopeRepo, ScopeGlobal)
	}
}

// LoadPluginWorkflows reads the workflows shipped by every enabled plugin.
func LoadPluginWorkflows(cwd string) []Def {
	var out []Def
	for _, in := range plugin.EnabledPlugins(cwd) {
		if in.Dir == "" {
			continue
		}
		for _, path := range in.Contents().WorkflowFiles {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var d Def
			if err := json.Unmarshal(data, &d); err != nil {
				continue
			}
			d.Name = strings.TrimSpace(d.Name)
			if d.Name == "" {
				continue
			}
			d.Scope = ScopePlugin
			d.Plugin = in.Ref.String()
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Plugin != out[j].Plugin {
			return out[i].Plugin < out[j].Plugin
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// FileName maps a workflow name onto a filesystem-safe filename stem.
func FileName(name string) string {
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

// LoadFileWorkflows reads every *.json under dir and tags them with scope.
func LoadFileWorkflows(dir string, scope Scope) []Def {
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
	var out []Def
	for _, fname := range names {
		path := filepath.Join(dir, fname)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var d Def
		if err := json.Unmarshal(data, &d); err != nil {
			continue
		}
		d.Name = strings.TrimSpace(d.Name)
		if d.Name == "" || seen[d.Name] {
			continue
		}
		seen[d.Name] = true
		d.Scope = scope
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ExportBytes renders d as the standalone JSON a plugin's workflows/
// directory holds (scope-free).
func ExportBytes(d Def) ([]byte, error) {
	d.Scope = ""
	d.Plugin = ""
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ExportFile writes ExportBytes(d) as FileName(d.Name).json in a fresh
// temporary directory and returns its path.
func ExportFile(d Def) (string, error) {
	data, err := ExportBytes(d)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "ask-workflow-*")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, FileName(d.Name)+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// LoadUserWorkflows reads the user-scope workflow list from ask.json for cwd.
func LoadUserWorkflows(cwd string) ([]Def, error) {
	pc, err := config.LoadProject(cwd)
	if err != nil {
		return nil, err
	}
	var out []Def
	for _, raw := range pc.Workflows.Items {
		d := ConfigDefToDef(raw)
		if strings.TrimSpace(d.Name) != "" {
			d.Scope = ScopeUser
			out = append(out, d)
		}
	}
	return out, nil
}

// ListAll returns all workflows visible to cwd in order: global, repo,
// user, then the read-only plugin workflows.
func ListAll(cwd string) []Def {
	var merged []Def
	seen := map[string]bool{}

	// 1. Global
	for _, w := range LoadFileWorkflows(GlobalDir(), ScopeGlobal) {
		key := string(ScopeGlobal) + ":" + w.Name
		if !seen[key] {
			seen[key] = true
			merged = append(merged, w)
		}
	}

	// 2. Repo
	for _, w := range LoadFileWorkflows(RepoDir(cwd), ScopeRepo) {
		key := string(ScopeRepo) + ":" + w.Name
		if !seen[key] {
			seen[key] = true
			merged = append(merged, w)
		}
	}

	// 3. User
	userDefs, _ := LoadUserWorkflows(cwd)
	for _, w := range userDefs {
		key := string(ScopeUser) + ":" + w.Name
		if !seen[key] {
			seen[key] = true
			merged = append(merged, w)
		}
	}

	// 4. Plugins (read-only)
	for _, w := range LoadPluginWorkflows(cwd) {
		key := string(ScopePlugin) + ":" + w.Plugin + ":" + w.Name
		if !seen[key] {
			seen[key] = true
			merged = append(merged, w)
		}
	}

	return merged
}

var ErrWorkflowAmbiguous = errors.New("workflow exists in multiple scopes; pass scope to pick one")

// ResolveByName resolves a workflow by name and optional scope.
// With no scope specified, the first matching workflow in global -> repo -> user order is returned (personal-wins).
func ResolveByName(cwd, name string, scope Scope) (Def, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Def{}, errors.New("workflow name is required")
	}
	if scope != "" {
		norm := Scope(strings.TrimSpace(string(scope)))
		if norm != ScopePlugin {
			var err error
			if norm, err = NormalizeScope(scope); err != nil {
				return Def{}, err
			}
		}
		for _, w := range ListAll(cwd) {
			if w.Name == name && w.Scope == norm {
				return w, nil
			}
		}
		return Def{}, fmt.Errorf("workflow %q not found in %s scope", name, norm)
	}

	for _, w := range ListAll(cwd) {
		if w.Name == name {
			return w, nil
		}
	}
	return Def{}, fmt.Errorf("workflow %q not found", name)
}

// SaveAll persists workflows across their respective scopes.
func SaveAll(cwd string, items []Def) error {
	return config.WithConfigLock(func() error {
		return saveAllLocked(cwd, items)
	})
}

// MutateWorkflows runs a read-modify-write cycle on the merged workflows.
func MutateWorkflows(cwd string, fn func(items []Def) ([]Def, error)) error {
	return config.WithConfigLock(func() error {
		current := ListAll(cwd)
		next, err := fn(current)
		if err != nil {
			return err
		}
		return saveAllLocked(cwd, next)
	})
}

func saveAllLocked(cwd string, items []Def) error {
	var repo, global, user []Def
	for _, w := range items {
		switch w.Scope {
		case ScopeRepo:
			repo = append(repo, w)
		case ScopeGlobal:
			global = append(global, w)
		case ScopePlugin:
			// Plugin workflows are never written back: the installed copy
			// is the source of truth until the plugin is updated.
		default:
			user = append(user, w)
		}
	}

	pc, _ := config.LoadProject(cwd)
	if len(user) == 0 {
		pc.Workflows.Items = nil
	} else {
		var items []config.WorkflowDef
		for _, w := range user {
			w.Scope = ""
			items = append(items, DefToConfigDef(w))
		}
		pc.Workflows.Items = items
	}
	if err := config.SaveProjectLocked(cwd, pc); err != nil {
		return err
	}

	if err := syncWorkflowFiles(RepoDir(cwd), repo); err != nil {
		return err
	}
	return syncWorkflowFiles(GlobalDir(), global)
}

// ConfigDefToDef converts a config.WorkflowDef to a workflow.Def.
func ConfigDefToDef(d config.WorkflowDef) Def {
	return Def{
		Name:        d.Name,
		Description: d.Description,
		Steps:       configStepsToSteps(d.Steps),
		Scope:       Scope(d.Scope),
		Plugin:      d.Plugin,
	}
}

func configStepsToSteps(steps []config.WorkflowStep) []Step {
	if steps == nil {
		return nil
	}
	out := make([]Step, len(steps))
	for i, s := range steps {
		out[i] = Step{
			Name:          s.Name,
			Kind:          s.Kind,
			Provider:      s.Provider,
			Model:         s.Model,
			Prompt:        s.Prompt,
			Steps:         configStepsToSteps(s.Steps),
			MaxIterations: s.MaxIterations,
			ExitCondition: s.ExitCondition,
		}
	}
	return out
}

// DefToConfigDef converts a workflow.Def to a config.WorkflowDef.
func DefToConfigDef(d Def) config.WorkflowDef {
	return config.WorkflowDef{
		Name:        d.Name,
		Description: d.Description,
		Steps:       stepsToConfigSteps(d.Steps),
		Scope:       string(d.Scope),
		Plugin:      d.Plugin,
	}
}

func stepsToConfigSteps(steps []Step) []config.WorkflowStep {
	if steps == nil {
		return nil
	}
	out := make([]config.WorkflowStep, len(steps))
	for i, s := range steps {
		out[i] = config.WorkflowStep{
			Name:          s.Name,
			Kind:          s.Kind,
			Provider:      s.Provider,
			Model:         s.Model,
			Prompt:        s.Prompt,
			Steps:         stepsToConfigSteps(s.Steps),
			MaxIterations: s.MaxIterations,
			ExitCondition: s.ExitCondition,
		}
	}
	return out
}

func syncWorkflowFiles(dir string, defs []Def) error {
	if dir == "" {
		if len(defs) > 0 {
			return errors.New("directory path is empty")
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
		stem := FileName(def.Name)
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
	if remaining == 0 {
		_ = os.Remove(dir)
	}
	return nil
}
