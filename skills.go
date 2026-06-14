package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skills.go implements the Agent Skills open standard (agentskills.io)
// for the in-process agent sessions: SKILL.md packages discovered from
// the conventional directories, surfaced to the model as triggers in
// the system prompt (progressive disclosure — the body loads on demand
// through the read tool), and to the user as slash commands.

// askSkill is one discovered SKILL.md package.
type askSkill struct {
	Name        string
	Description string
	// Dir is the skill package directory; Path is its SKILL.md.
	Dir  string
	Path string
	// UserInvocable surfaces the skill as a /name slash command
	// (default true; `user-invocable: false` hides it).
	UserInvocable bool
	// DisableModelInvocation removes the skill from the system-prompt
	// trigger list — the user can still invoke it explicitly.
	DisableModelInvocation bool
}

// skillNameRe is the standard's name constraint: lowercase-friendly
// alphanumeric runs separated by single hyphens.
var skillNameRe = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)

const (
	skillNameMaxLen        = 64
	skillDescriptionMaxLen = 1024
)

// skillSearchDirs returns the discovery roots in precedence order —
// later directories win on name clash, so project skills override
// user-global ones.
func skillSearchDirs(cwd string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".config", "ask", "skills"),
			filepath.Join(home, ".agents", "skills"),
			filepath.Join(home, ".claude", "skills"),
		)
	}
	roots := []string{cwd}
	if root := projectRoot(cwd); root != "" && root != cwd {
		roots = append(roots, root)
	}
	for _, root := range roots {
		dirs = append(dirs,
			filepath.Join(root, ".agents", "skills"),
			filepath.Join(root, ".claude", "skills"),
			filepath.Join(root, ".ask", "skills"),
		)
	}
	return dirs
}

// discoverSkills walks every search dir for <name>/SKILL.md packages.
// Invalid packages (bad name, missing description) are skipped with a
// debug note rather than failing the session.
func discoverSkills(cwd string) []askSkill {
	byName := map[string]askSkill{}
	for _, dir := range skillSearchDirs(cwd) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name(), "SKILL.md")
			fields, _, ok := parseMarkdownFrontmatter(path)
			if !ok {
				continue
			}
			name := fields["name"]
			if name == "" {
				name = e.Name()
			}
			if len(name) > skillNameMaxLen || !skillNameRe.MatchString(name) {
				debugLog("skill %s skipped: invalid name %q", path, name)
				continue
			}
			if name != e.Name() {
				debugLog("skill %s skipped: name %q != directory %q", path, name, e.Name())
				continue
			}
			desc := fields["description"]
			if strings.TrimSpace(desc) == "" {
				debugLog("skill %s skipped: description is required", path)
				continue
			}
			if len(desc) > skillDescriptionMaxLen {
				desc = desc[:skillDescriptionMaxLen]
			}
			byName[name] = askSkill{
				Name:                   name,
				Description:            desc,
				Dir:                    filepath.Join(dir, e.Name()),
				Path:                   path,
				UserInvocable:          fields["user-invocable"] != "false",
				DisableModelInvocation: fields["disable-model-invocation"] == "true",
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]askSkill, 0, len(byName))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

// skillsPromptBlock renders the system-prompt trigger list: name +
// description + location only — the body stays on disk until the
// model reads it (progressive disclosure).
func skillsPromptBlock(skills []askSkill) string {
	listed := make([]askSkill, 0, len(skills))
	for _, s := range skills {
		if !s.DisableModelInvocation {
			listed = append(listed, s)
		}
	}
	if len(listed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<available_skills>\n")
	for _, s := range listed {
		fmt.Fprintf(&b, "  <skill><name>%s</name><description>%s</description><location>%s</location></skill>\n",
			s.Name, s.Description, s.Path)
	}
	b.WriteString("</available_skills>\n")
	b.WriteString(`<skills_usage>
Skills are reusable instruction packages. Each description above is a trigger: when the current task matches one, you MUST read the skill's location file with the read tool BEFORE doing that work, then follow its instructions. Supporting files (scripts, references, templates) live in the same directory as the SKILL.md and are referenced relative to it. Do not guess at a skill's contents from its name.
</skills_usage>`)
	return b.String()
}

// expandSkillInvocation turns a "/skill-name optional args" user line
// into the full skill invocation message — the user-invocable side of
// the standard. Returns ok=false when the line is not a known
// user-invocable skill (the caller sends the text unchanged).
func expandSkillInvocation(cwd, text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	nameAndArgs := strings.TrimPrefix(trimmed, "/")
	name, args, _ := strings.Cut(nameAndArgs, " ")
	if name == "" {
		return "", false
	}
	for _, s := range discoverSkills(cwd) {
		if s.Name != name || !s.UserInvocable {
			continue
		}
		_, body, ok := parseMarkdownFrontmatter(s.Path)
		if !ok {
			return "", false
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<loaded_skill name=%q path=%q>\n%s\n</loaded_skill>\n\n", s.Name, s.Path, strings.TrimSpace(body))
		repoRoot := projectRoot(cwd)
		if repoRoot == "" {
			repoRoot = cwd
		}
		if linked := ruleLinkedDocs(repoRoot, body); len(linked) > 0 {
			for _, d := range linked {
				fmt.Fprintf(&b, "<file path=%q>\n%s\n</file>\n", d.Path, d.Body)
			}
		}
		b.WriteString("\n")
		if strings.TrimSpace(args) != "" {
			fmt.Fprintf(&b, "The user invoked this skill with arguments: %s", strings.TrimSpace(args))
		} else {
			b.WriteString("The user invoked this skill with no arguments.")
		}
		return b.String(), true
	}
	return "", false
}

// parseMarkdownFrontmatter reads a markdown file with YAML-ish
// frontmatter and returns the scalar fields plus the body after the
// closing delimiter. Only flat `key: value` lines are parsed — the
// skills/agents standards only use scalars. ok=false when the file is
// missing or has no frontmatter block.
func parseMarkdownFrontmatter(path string) (fields map[string]string, body string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", false
	}
	s := strings.TrimPrefix(string(data), "\ufeff")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return nil, "", false
	}
	rest := s[strings.Index(s, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, "", false
	}
	fm := rest[:end]
	body = rest[end+len("\n---"):]
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	fields = map[string]string{}
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimRight(line, "\r")
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || strings.HasPrefix(key, "#") {
			continue
		}
		fields[key] = unquoteYAML(strings.TrimSpace(value))
	}
	return fields, body, true
}
