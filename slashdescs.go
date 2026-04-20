package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func enrichSlashCommands(names []string) []claudeSlashEntry {
	index := slashCmdDescriptions()
	out := make([]claudeSlashEntry, len(names))
	for i, n := range names {
		out[i] = claudeSlashEntry{Name: n, Description: index[n]}
	}
	return out
}

func slashCmdDescriptions() map[string]string {
	index := map[string]string{}
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()

	for _, dir := range []string{
		filepath.Join(home, ".claude", "commands"),
		filepath.Join(cwd, ".claude", "commands"),
	} {
		walkCommandsDir(dir, "", index)
	}
	for _, dir := range []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(cwd, ".claude", "skills"),
	} {
		walkSkillsDir(dir, "", index)
	}

	pluginsRoot := filepath.Join(home, ".claude", "plugins", "cache")
	marketplaces, _ := os.ReadDir(pluginsRoot)
	for _, mp := range marketplaces {
		if !mp.IsDir() {
			continue
		}
		mpRoot := filepath.Join(pluginsRoot, mp.Name())
		plugins, _ := os.ReadDir(mpRoot)
		for _, plugin := range plugins {
			if !plugin.IsDir() {
				continue
			}
			pluginName := plugin.Name()
			pluginRoot := filepath.Join(mpRoot, pluginName)
			versions, _ := os.ReadDir(pluginRoot)
			for _, v := range versions {
				if !v.IsDir() {
					continue
				}
				vRoot := filepath.Join(pluginRoot, v.Name())
				walkCommandsDir(filepath.Join(vRoot, "commands"), pluginName+":", index)
				walkSkillsDir(filepath.Join(vRoot, "skills"), pluginName+":", index)
			}
		}
	}
	return index
}

func walkCommandsDir(dir, prefix string, out map[string]string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		name, desc := parseFrontmatter(path)
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(path), ".md")
		}
		if desc != "" && out[prefix+name] == "" {
			out[prefix+name] = desc
		}
		return nil
	})
}

func walkSkillsDir(dir, prefix string, out map[string]string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		skillPath := filepath.Join(dir, e.Name(), "SKILL.md")
		name, desc := parseFrontmatter(skillPath)
		if name == "" {
			name = e.Name()
		}
		if desc != "" && out[prefix+name] == "" {
			out[prefix+name] = desc
		}
	}
}

func parseFrontmatter(path string) (name, desc string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	s := string(data)
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return
	}
	rest := s[strings.Index(s, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return
	}
	fm := rest[:end]
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "name:"):
			name = unquoteYAML(strings.TrimSpace(strings.TrimPrefix(line, "name:")))
		case strings.HasPrefix(line, "description:"):
			desc = unquoteYAML(strings.TrimSpace(strings.TrimPrefix(line, "description:")))
		}
	}
	return
}

func unquoteYAML(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
