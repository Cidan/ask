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

func WorkflowsRepoDir(cwd string) string {
	if cwd == "" {
		return ""
	}
	root := cwd
	return filepath.Join(root, filepath.FromSlash(WorkflowsRepoDirName))
}

func WorkflowsGlobalDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "ask", WorkflowsGlobalDirName)
}

func NormalizeScope(s Scope) (Scope, error) {
	switch s {
	case "", ScopeUser:
		return ScopeUser, nil
	case ScopeRepo:
		return ScopeRepo, nil
	case ScopeGlobal:
		return ScopeGlobal, nil
	default:
		return "", fmt.Errorf("unknown workflow scope %q: valid scopes are %q, %q, %q",
			s, ScopeUser, ScopeRepo, ScopeGlobal)
	}
}

func LoadFileWorkflows(dir string, scope Scope) []Def {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Def
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var d Def
		if err := json.Unmarshal(data, &d); err != nil {
			continue
		}
		d.Scope = scope
		out = append(out, d)
	}
	return out
}

func ListAll(cwd string) []Def {
	var merged []Def
	seen := map[string]bool{}

	// 1. Global
	for _, w := range LoadFileWorkflows(WorkflowsGlobalDir(), ScopeGlobal) {
		key := string(ScopeGlobal) + ":" + w.Name
		if !seen[key] {
			seen[key] = true
			merged = append(merged, w)
		}
	}

	// 2. Repo
	for _, w := range LoadFileWorkflows(WorkflowsRepoDir(cwd), ScopeRepo) {
		key := string(ScopeRepo) + ":" + w.Name
		if !seen[key] {
			seen[key] = true
			merged = append(merged, w)
		}
	}

	// 3. User (from ask.json)
	cfg, err := config.Load()
	if err == nil && cfg.Projects != nil {
		if pc, ok := cfg.Projects[cwd]; ok && pc.Workflows.ActiveWorkflow != "" {
			// user workflows if any
		}
	}

	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Name < merged[j].Name
	})

	return merged
}

func ResolveByName(cwd, name string, scope Scope) (Def, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Def{}, errors.New("workflow name is required")
	}
	all := ListAll(cwd)
	var matches []Def
	for _, w := range all {
		if w.Name == name {
			if scope == "" || w.Scope == scope {
				matches = append(matches, w)
			}
		}
	}
	if len(matches) == 0 {
		return Def{}, fmt.Errorf("workflow %q not found", name)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	// Prefer global if ambiguous and scope was unspecified
	for _, m := range matches {
		if m.Scope == ScopeGlobal {
			return m, nil
		}
	}
	return matches[0], nil
}
