package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
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
	// Frontmatter holds the parsed ADK frontmatter when available.
	Frontmatter            *skill.Frontmatter
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

// discoveredSkillItem holds internal metadata for a resolved skill.
type discoveredSkillItem struct {
	skill       Skill
	frontmatter *skill.Frontmatter
	body        string
}

// skillSource implements skill.Source backed by discovered skills in search directories.
type skillSource struct {
	cwd    string
	skills map[string]discoveredSkillItem
	order  []string
}

// NewSkillSource constructs an ADK skill.Source across all discovery directories for cwd.
// Later directories in SkillSearchDirs take precedence (project-overrides-global).
func NewSkillSource(cwd string) skill.Source {
	byName := map[string]discoveredSkillItem{}
	for _, dir := range SkillSearchDirs(cwd) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if len(name) > skillNameMaxLen || !skillNameRe.MatchString(name) {
				continue
			}
			skillPath := filepath.Join(dir, name, "SKILL.md")
			data, err := os.ReadFile(skillPath)
			if err != nil {
				continue
			}
			fields, body, ok := ParseFrontmatterBytes(data)
			if !ok {
				continue
			}
			fmName := fields["name"]
			if fmName == "" {
				fmName = name
			}
			if fmName != name || len(fmName) > skillNameMaxLen || !skillNameRe.MatchString(fmName) {
				continue
			}
			desc := fields["description"]
			if strings.TrimSpace(desc) == "" {
				continue
			}
			if len(desc) > skillDescriptionMaxLen {
				desc = desc[:skillDescriptionMaxLen]
			}

			// Validate with ADK's skill.Frontmatter if possible
			var adkFM *skill.Frontmatter
			if parsedFM, _, parseErr := skill.ParseBytes(data); parseErr == nil && parsedFM != nil {
				adkFM = parsedFM
			} else {
				adkFM = &skill.Frontmatter{
					Name:        fmName,
					Description: desc,
					Metadata:    fields,
				}
			}

			s := Skill{
				Name:                   fmName,
				Description:            desc,
				Dir:                    filepath.Join(dir, name),
				Path:                   skillPath,
				UserInvocable:          fields["user-invocable"] != "false",
				DisableModelInvocation: fields["disable-model-invocation"] == "true",
				Frontmatter:            adkFM,
			}
			byName[fmName] = discoveredSkillItem{
				skill:       s,
				frontmatter: adkFM,
				body:        body,
			}
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	return &skillSource{
		cwd:    cwd,
		skills: byName,
		order:  names,
	}
}

func (s *skillSource) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	out := make([]*skill.Frontmatter, 0, len(s.order))
	for _, name := range s.order {
		if item, ok := s.skills[name]; ok && item.frontmatter != nil {
			out = append(out, item.frontmatter)
		}
	}
	return out, nil
}

func (s *skillSource) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	if item, ok := s.skills[name]; ok && item.frontmatter != nil {
		return item.frontmatter, nil
	}
	return nil, skill.ErrSkillNotFound
}

func (s *skillSource) LoadInstructions(ctx context.Context, name string) (string, error) {
	if item, ok := s.skills[name]; ok {
		return item.body, nil
	}
	return "", skill.ErrSkillNotFound
}

func (s *skillSource) LoadResource(ctx context.Context, name, resourcePath string) (io.ReadCloser, error) {
	item, ok := s.skills[name]
	if !ok {
		return nil, skill.ErrSkillNotFound
	}
	cleanPath := path.Clean(resourcePath)
	if !strings.HasPrefix(cleanPath, "references/") && !strings.HasPrefix(cleanPath, "assets/") && !strings.HasPrefix(cleanPath, "scripts/") {
		return nil, fmt.Errorf("%w: %q must be within 'references/', 'assets/', or 'scripts/'", skill.ErrInvalidResourcePath, resourcePath)
	}
	fullPath := filepath.Join(item.skill.Dir, filepath.FromSlash(cleanPath))
	f, err := os.Open(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", skill.ErrResourceNotFound, cleanPath)
		}
		return nil, fmt.Errorf("open resource %q: %w", fullPath, err)
	}
	return f, nil
}

func (s *skillSource) ListResources(ctx context.Context, name, resourceDirectoryPath string) ([]string, error) {
	item, ok := s.skills[name]
	if !ok {
		return nil, skill.ErrSkillNotFound
	}
	cleanPath := path.Clean(resourceDirectoryPath)
	isRoot := cleanPath == "." || cleanPath == ""

	if !isRoot {
		top := strings.SplitN(cleanPath, "/", 2)[0]
		if top != "references" && top != "assets" && top != "scripts" {
			return nil, fmt.Errorf("%w: %q must be within 'references/', 'assets/', or 'scripts/'", skill.ErrInvalidResourcePath, resourceDirectoryPath)
		}
	}

	targets := []string{cleanPath}
	if isRoot {
		targets = []string{"references", "assets", "scripts"}
	}

	var resources []string
	for _, target := range targets {
		targetDir := filepath.Join(item.skill.Dir, filepath.FromSlash(target))
		if _, err := os.Stat(targetDir); err != nil {
			if errors.Is(err, os.ErrNotExist) && !isRoot {
				return nil, fmt.Errorf("%w: %q", skill.ErrResourceNotFound, cleanPath)
			}
			continue
		}
		_ = filepath.WalkDir(targetDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(item.skill.Dir, p)
			if err == nil {
				resources = append(resources, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	return resources, nil
}

// NewSkillToolset creates an ADK SkillToolset backed by NewSkillSource(cwd).
func NewSkillToolset(ctx context.Context, cwd string) (*skilltoolset.SkillToolset, error) {
	source := NewSkillSource(cwd)
	return skilltoolset.New(ctx, skilltoolset.Config{
		Source: source,
	})
}

// DiscoverSkills walks every search dir for <name>/SKILL.md packages.
// Invalid packages (bad name, missing description) are skipped rather than failing the session.
func DiscoverSkills(cwd string) []Skill {
	src := NewSkillSource(cwd)
	asSource, ok := src.(*skillSource)
	if !ok {
		return nil
	}
	out := make([]Skill, 0, len(asSource.order))
	for _, name := range asSource.order {
		if item, exists := asSource.skills[name]; exists {
			out = append(out, item.skill)
		}
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
		source := NewSkillSource(cwd)
		body, err := source.LoadInstructions(context.Background(), s.Name)
		if err != nil {
			_, b, ok := ParseMarkdownFrontmatter(s.Path)
			if !ok {
				return "", false
			}
			body = b
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

// ParseMarkdownFrontmatter reads a markdown file with YAML frontmatter
// and returns the scalar fields plus the body after the closing delimiter.
func ParseMarkdownFrontmatter(path string) (fields map[string]string, body string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", false
	}
	return ParseFrontmatterBytes(data)
}

// ParseFrontmatterBytes parses frontmatter and body from bytes.
func ParseFrontmatterBytes(data []byte) (fields map[string]string, body string, ok bool) {
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
	if adkFM, adkBody, err := skill.ParseBytes(data); err == nil && adkFM != nil {
		if adkFM.Name != "" {
			fields["name"] = adkFM.Name
		}
		if adkFM.Description != "" {
			fields["description"] = adkFM.Description
		}
		if adkFM.License != "" {
			fields["license"] = adkFM.License
		}
		if adkFM.Compatibility != "" {
			fields["compatibility"] = adkFM.Compatibility
		}
		for k, v := range adkFM.Metadata {
			fields[k] = v
		}
		body = adkBody
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
