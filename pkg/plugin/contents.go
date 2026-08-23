package plugin

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Contents is what a plugin directory contributes, resolved to absolute
// paths: skill packages (directories holding SKILL.md), single-file
// commands (Claude Code's commands/*.md, loaded as skills), agent
// definitions, and ask workflows.
type Contents struct {
	SkillDirs     []string
	CommandFiles  []string
	AgentFiles    []string
	WorkflowFiles []string
}

func (c Contents) Count() int {
	return len(c.SkillDirs) + len(c.CommandFiles) + len(c.AgentFiles) + len(c.WorkflowFiles)
}

func (c Contents) Empty() bool { return c.Count() == 0 }

// ResolveContents applies the plugin.json / marketplace-entry component
// paths (strict: both merge; strict:false: the entry alone) with the
// default directories, and expands them to concrete files.
func ResolveContents(dir string, entry *Entry, manifest *PluginManifest) Contents {
	var skills, agents, commands, workflows PathList
	if manifest != nil && (entry == nil || entry.IsStrict()) {
		skills = append(skills, manifest.Skills...)
		agents = append(agents, manifest.Agents...)
		commands = append(commands, manifest.Commands...)
		workflows = append(workflows, manifest.Workflows...)
	}
	if entry != nil {
		skills = append(skills, entry.Skills...)
		agents = append(agents, entry.Agents...)
		commands = append(commands, entry.Commands...)
		workflows = append(workflows, entry.Workflows...)
	}
	if len(skills) == 0 {
		skills = PathList{"skills"}
	}
	if len(agents) == 0 {
		agents = PathList{"agents"}
	}
	if len(commands) == 0 {
		commands = PathList{"commands"}
	}
	if len(workflows) == 0 {
		workflows = PathList{"workflows"}
	}

	var c Contents
	if fileExists(filepath.Join(dir, "SKILL.md")) {
		c.SkillDirs = append(c.SkillDirs, filepath.Clean(dir))
	}
	for _, p := range dedupe(skills) {
		abs, ok := resolveInside(dir, p)
		if !ok {
			continue
		}
		switch {
		case fileExists(filepath.Join(abs, "SKILL.md")):
			c.SkillDirs = append(c.SkillDirs, abs)
		case dirExists(abs):
			entries, _ := os.ReadDir(abs)
			for _, e := range entries {
				if e.IsDir() && fileExists(filepath.Join(abs, e.Name(), "SKILL.md")) {
					c.SkillDirs = append(c.SkillDirs, filepath.Join(abs, e.Name()))
				}
			}
		}
	}
	c.AgentFiles = collectFiles(dir, dedupe(agents), ".md")
	c.CommandFiles = collectFiles(dir, dedupe(commands), ".md")
	c.WorkflowFiles = collectFiles(dir, dedupe(workflows), ".json")
	c.SkillDirs = dedupe(c.SkillDirs)
	sort.Strings(c.SkillDirs)
	return c
}

func resolveInside(dir, p string) (string, bool) {
	abs := filepath.Join(dir, filepath.FromSlash(p))
	if !insideDir(dir, abs) {
		return "", false
	}
	return filepath.Clean(abs), true
}

func collectFiles(dir string, paths []string, ext string) []string {
	var out []string
	for _, p := range paths {
		abs, ok := resolveInside(dir, p)
		if !ok {
			continue
		}
		switch {
		case fileExists(abs) && strings.HasSuffix(abs, ext):
			out = append(out, abs)
		case dirExists(abs):
			_ = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if path != abs && strings.HasPrefix(d.Name(), ".") {
						return filepath.SkipDir
					}
					return nil
				}
				if strings.HasSuffix(d.Name(), ext) {
					out = append(out, path)
				}
				return nil
			})
		}
	}
	out = dedupe(out)
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
