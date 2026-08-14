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
	default:
		return "", fmt.Errorf("unknown workflow scope %q: valid scopes are %q, %q, %q",
			s, ScopeUser, ScopeRepo, ScopeGlobal)
	}
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

// LoadUserWorkflows reads the user-scope workflow list from ask.json for cwd.
func LoadUserWorkflows(cwd string) ([]Def, error) {
	pc, err := config.LoadProject(cwd)
	if err != nil {
		return nil, err
	}
	var out []Def
	for _, raw := range pc.Workflows.Items {
		var d Def
		if err := json.Unmarshal(raw, &d); err == nil && strings.TrimSpace(d.Name) != "" {
			d.Scope = ScopeUser
			out = append(out, d)
		}
	}
	return out, nil
}

// ListAll returns all workflows visible to cwd in order: global, repo, user.
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

	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Name < merged[j].Name
	})

	return merged
}

var ErrWorkflowAmbiguous = errors.New("workflow exists in multiple scopes; pass scope to pick one")

// ResolveByName resolves a workflow by name and optional scope.
func ResolveByName(cwd, name string, scope Scope) (Def, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Def{}, errors.New("workflow name is required")
	}
	if scope != "" {
		norm, err := NormalizeScope(scope)
		if err != nil {
			return Def{}, err
		}
		for _, w := range ListAll(cwd) {
			if w.Name == name && w.Scope == norm {
				return w, nil
			}
		}
		return Def{}, fmt.Errorf("workflow %q not found in %s scope", name, norm)
	}

	var matches []Def
	for _, w := range ListAll(cwd) {
		if w.Name == name {
			matches = append(matches, w)
		}
	}
	switch len(matches) {
	case 0:
		return Def{}, fmt.Errorf("workflow %q not found", name)
	case 1:
		return matches[0], nil
	}

	// Prefer global if ambiguous
	for _, m := range matches {
		if m.Scope == ScopeGlobal {
			return m, nil
		}
	}
	return matches[0], nil
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
		default:
			user = append(user, w)
		}
	}

	pc, _ := config.LoadProject(cwd)
	if len(user) == 0 {
		pc.Workflows.Items = nil
	} else {
		var rawItems []json.RawMessage
		for _, w := range user {
			w.Scope = ""
			if b, err := json.Marshal(w); err == nil {
				rawItems = append(rawItems, b)
			}
		}
		pc.Workflows.Items = rawItems
	}
	if err := config.SaveProjectLocked(cwd, pc); err != nil {
		return err
	}

	if err := syncWorkflowFiles(RepoDir(cwd), repo); err != nil {
		return err
	}
	return syncWorkflowFiles(GlobalDir(), global)
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
