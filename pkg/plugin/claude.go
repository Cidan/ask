package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ClaudeHome locates Claude Code's state; swappable for tests.
var ClaudeHome = func() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// ClaudeState is what Claude Code holds that ask can import: registered
// marketplaces and the plugins enabled at user and project level.
type ClaudeState struct {
	Marketplaces   map[string]KnownMarketplace
	UserEnabled    map[string]bool
	ProjectEnabled map[string]bool
}

// Empty reports whether there is nothing to import.
func (s ClaudeState) Empty() bool {
	return len(s.Marketplaces) == 0 && len(s.UserEnabled) == 0 && len(s.ProjectEnabled) == 0
}

// EnabledRefs is the sorted union of user- and project-enabled plugins.
func (s ClaudeState) EnabledRefs() []string {
	set := map[string]bool{}
	for k, v := range s.UserEnabled {
		if v {
			set[k] = true
		}
	}
	for k, v := range s.ProjectEnabled {
		if v {
			set[k] = true
		}
	}
	return sortedKeys(set)
}

type claudeSettings struct {
	EnabledPlugins        map[string]bool             `json:"enabledPlugins"`
	ExtraKnownMarketplace map[string]KnownMarketplace `json:"extraKnownMarketplaces"`
}

// ReadClaudeState reads ~/.claude/plugins/known_marketplaces.json,
// ~/.claude/settings.json, and <root>/.claude/settings.json. Missing
// files are simply absent from the result.
func ReadClaudeState(cwd string) ClaudeState {
	st := ClaudeState{Marketplaces: map[string]KnownMarketplace{}}
	home := ClaudeHome()
	if home == "" {
		return st
	}
	known := map[string]KnownMarketplace{}
	if err := readJSON(filepath.Join(home, "plugins", "known_marketplaces.json"), &known); err == nil {
		for k, v := range known {
			st.Marketplaces[k] = v
		}
	}
	var user claudeSettings
	if err := readJSON(filepath.Join(home, "settings.json"), &user); err == nil {
		st.UserEnabled = user.EnabledPlugins
		for k, v := range user.ExtraKnownMarketplace {
			if _, ok := st.Marketplaces[k]; !ok {
				st.Marketplaces[k] = v
			}
		}
	}
	if root := projectRootOf(cwd); root != "" {
		var proj claudeSettings
		if err := readJSON(filepath.Join(root, ".claude", "settings.json"), &proj); err == nil {
			st.ProjectEnabled = proj.EnabledPlugins
			for k, v := range proj.ExtraKnownMarketplace {
				if _, ok := st.Marketplaces[k]; !ok {
					st.Marketplaces[k] = v
				}
			}
		}
	}
	return st
}

// ImportReport is what ImportFromClaude did.
type ImportReport struct {
	Marketplaces []string
	Plugins      []string
	Skipped      []string
	Errors       []string
}

// Summary is the one-line toast form.
func (r ImportReport) Summary() string {
	s := fmt.Sprintf("imported %d marketplace(s), %d plugin(s)", len(r.Marketplaces), len(r.Plugins))
	if len(r.Skipped) > 0 {
		s += fmt.Sprintf(", %d already present", len(r.Skipped))
	}
	if len(r.Errors) > 0 {
		s += fmt.Sprintf(", %d failed: %s", len(r.Errors), r.Errors[0])
	}
	return s
}

// ImportFromClaude registers Claude Code's marketplaces and installs its
// enabled plugins into ask's own store at scope. Nothing under ~/.claude
// is modified.
func ImportFromClaude(ctx context.Context, cwd string, st ClaudeState, scope Scope) ImportReport {
	var rep ImportReport
	existing := ListMarketplaces(cwd)
	names := sortedKeys(st.Marketplaces)
	for _, name := range names {
		km := st.Marketplaces[name]
		raw := km.Source.Raw()
		if raw == "" {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: unsupported source %q", name, km.Source.Kind))
			continue
		}
		already := false
		for _, m := range existing {
			if m.Source.Equal(km.Source) || m.Name == name {
				already = true
				break
			}
		}
		if already {
			rep.Skipped = append(rep.Skipped, name)
			continue
		}
		if _, err := AddMarketplace(ctx, cwd, raw, scope); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		rep.Marketplaces = append(rep.Marketplaces, name)
	}
	enabled := map[string]bool{}
	for _, in := range EnabledPlugins(cwd) {
		enabled[in.Ref.String()] = true
	}
	for _, key := range st.EnabledRefs() {
		ref, err := ParseRef(key)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			continue
		}
		if enabled[key] {
			rep.Skipped = append(rep.Skipped, key)
			continue
		}
		if _, err := InstallPlugin(ctx, cwd, ref, scope); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		rep.Plugins = append(rep.Plugins, key)
	}
	sort.Strings(rep.Skipped)
	return rep
}
