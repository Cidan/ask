package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Cidan/ask/pkg/config"
)

// Skill is one discovered SKILL.md package.
type Skill struct {
	Name                   string
	Description            string
	// Dir is the skill package directory; Path is its SKILL.md.
	Dir                    string
	Path                   string
	// UserInvocable surfaces the skill as a /name slash command
	// (default true; `user-invocable: false` hides it).
	UserInvocable          bool
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

// SkillSearchDirs returns the discovery roots in precedence order —
// later directories win on name clash, so project skills override
// user-global ones.
func SkillSearchDirs(cwd string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".config", "ask", "skills"),
			filepath.Join(home, ".agents", "skills"),
			filepath.Join(home, ".claude", "skills"),
		)
	}
	roots := []string{cwd}
	if root := config.ProjectRoot(cwd); root != "" && root != cwd {
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

// DiscoverSkills walks every search dir for <name>/SKILL.md packages.
// Invalid packages (bad name, missing description) are skipped rather than failing the session.
func DiscoverSkills(cwd string) []Skill {
	byName := map[string]Skill{}
	for _, dir := range SkillSearchDirs(cwd) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name(), "SKILL.md")
			fields, _, ok := ParseMarkdownFrontmatter(path)
			if !ok {
				continue
			}
			name := fields["name"]
			if name == "" {
				name = e.Name()
			}
			if len(name) > skillNameMaxLen || !skillNameRe.MatchString(name) {
				continue
			}
			if name != e.Name() {
				continue
			}
			desc := fields["description"]
			if strings.TrimSpace(desc) == "" {
				continue
			}
			if len(desc) > skillDescriptionMaxLen {
				desc = desc[:skillDescriptionMaxLen]
			}
			byName[name] = Skill{
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
	out := make([]Skill, 0, len(byName))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

// SkillsPromptBlock renders the system-prompt trigger list: name +
// description + location only — the body stays on disk until the
// model reads it (progressive disclosure).
func SkillsPromptBlock(skills []Skill) string {
	listed := make([]Skill, 0, len(skills))
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

// ExpandSkillInvocation turns a "/skill-name optional args" user line
// into the full skill invocation message — the user-invocable side of
// the standard. Returns ok=false when the line is not a known
// user-invocable skill (the caller sends the text unchanged).
func ExpandSkillInvocation(cwd, text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	nameAndArgs := strings.TrimPrefix(trimmed, "/")
	name, args, _ := strings.Cut(nameAndArgs, " ")
	if name == "" {
		return "", false
	}
	for _, s := range DiscoverSkills(cwd) {
		if s.Name != name || !s.UserInvocable {
			continue
		}
		_, body, ok := ParseMarkdownFrontmatter(s.Path)
		if !ok {
			return "", false
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<loaded_skill name=%q path=%q>\n%s\n</loaded_skill>\n\n", s.Name, s.Path, strings.TrimSpace(body))
		repoRoot := config.ProjectRoot(cwd)
		if repoRoot == "" {
			repoRoot = cwd
		}
		if linked := RuleLinkedDocs(repoRoot, body); len(linked) > 0 {
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

// ParseMarkdownFrontmatter reads a markdown file with YAML-ish
// frontmatter and returns the scalar fields plus the body after the
// closing delimiter. Only flat key: value lines are parsed.
func ParseMarkdownFrontmatter(path string) (fields map[string]string, body string, ok bool) {
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
		fields[key] = UnquoteYAML(strings.TrimSpace(value))
	}
	return fields, body, true
}

// UnquoteYAML strips quotes surrounding a YAML string value.
func UnquoteYAML(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
