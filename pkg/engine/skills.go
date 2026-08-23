package engine

import (
	"context"
	"encoding/json"
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
	"sync"
	"sync/atomic"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/plugin"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// OriginScope says where a skill or agent definition lives.
type OriginScope string

const (
	OriginUser    OriginScope = "user"
	OriginProject OriginScope = "project"
	OriginPlugin  OriginScope = "plugin"
)

// Origin is the provenance of a discovered skill or agent: a user or
// project directory, or an installed plugin ("name@marketplace").
type Origin struct {
	Scope  OriginScope
	Plugin string
}

func (o Origin) String() string {
	if o.Scope == OriginPlugin {
		return "plugin " + o.Plugin
	}
	return string(o.Scope)
}

// Editable reports whether the definition can be changed in place —
// plugin copies are replaced on update, never edited.
func (o Origin) Editable() bool { return o.Scope == OriginUser || o.Scope == OriginProject }

// Skill is one discovered skill: a SKILL.md package, or a single-file
// command (Claude Code's commands/*.md) loaded with the same contract.
type Skill struct {
	// Name is the slash/invocation name; plugin skills carry the
	// "plugin:" prefix the way Claude Code namespaces them.
	Name        string
	BareName    string
	Description string
	// Dir is the skill package directory; Path is its SKILL.md (or the
	// command file).
	Dir  string
	Path string
	// UserInvocable surfaces the skill as a /name slash command
	// (default true; `user-invocable: false` hides it).
	UserInvocable bool
	// DisableModelInvocation removes the skill from the system-prompt
	// trigger list — the user can still invoke it explicitly.
	DisableModelInvocation bool
	// Frontmatter holds the parsed ADK frontmatter when available.
	Frontmatter *skill.Frontmatter
	Origin      Origin
	Command     bool
}

// skillNameRe is the standard's name constraint: lowercase-friendly
// alphanumeric runs separated by single hyphens.
var skillNameRe = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)

const (
	skillNameMaxLen        = 64
	skillDescriptionMaxLen = 1024
)

// SkillRoot is one directory of skill packages plus the origin its
// skills are tagged with.
type SkillRoot struct {
	Dir    string
	Origin Origin
}

// SkillsUserDir is where ask writes new user-scope skills.
func SkillsUserDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "ask", "skills")
}

// SkillsProjectDir is where ask writes new project-scope skills.
func SkillsProjectDir(cwd string) string {
	if cwd == "" {
		return ""
	}
	root := config.ProjectRoot(cwd)
	if root == "" {
		root = cwd
	}
	return filepath.Join(root, ".ask", "skills")
}

// SkillSearchRoots returns the user and project discovery roots in
// precedence order — later roots win on a name clash, so project
// skills override user-global ones. Plugin skills are namespaced and
// never clash; they come from plugin.EnabledPlugins.
func SkillSearchRoots(cwd string) []SkillRoot {
	var roots []SkillRoot
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range []string{
			filepath.Join(home, ".config", "ask", "skills"),
			filepath.Join(home, ".agents", "skills"),
			filepath.Join(home, ".claude", "skills"),
		} {
			roots = append(roots, SkillRoot{Dir: d, Origin: Origin{Scope: OriginUser}})
		}
	}
	projectRoots := []string{cwd}
	if root := config.ProjectRoot(cwd); root != "" && root != cwd {
		projectRoots = append(projectRoots, root)
	}
	for _, root := range projectRoots {
		for _, d := range []string{
			filepath.Join(root, ".agents", "skills"),
			filepath.Join(root, ".claude", "skills"),
			filepath.Join(root, ".ask", "skills"),
		} {
			roots = append(roots, SkillRoot{Dir: d, Origin: Origin{Scope: OriginProject}})
		}
	}
	return roots
}

// SkillSearchDirs is SkillSearchRoots without the origin tags.
func SkillSearchDirs(cwd string) []string {
	roots := SkillSearchRoots(cwd)
	dirs := make([]string, 0, len(roots))
	for _, r := range roots {
		dirs = append(dirs, r.Dir)
	}
	return dirs
}

type skillCandidate struct {
	name    string
	bare    string
	dir     string
	path    string
	origin  Origin
	command bool
}

func skillCandidates(cwd string) []skillCandidate {
	var out []skillCandidate
	for _, root := range SkillSearchRoots(cwd) {
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root.Dir, e.Name())
			out = append(out, skillCandidate{
				name:   e.Name(),
				bare:   e.Name(),
				dir:    dir,
				path:   filepath.Join(dir, "SKILL.md"),
				origin: root.Origin,
			})
		}
	}
	for _, in := range plugin.EnabledPlugins(cwd) {
		if in.Dir == "" {
			continue
		}
		origin := Origin{Scope: OriginPlugin, Plugin: in.Ref.String()}
		prefix := in.Ref.Plugin + ":"
		c := in.Contents()
		for _, d := range c.SkillDirs {
			bare := filepath.Base(d)
			out = append(out, skillCandidate{
				name:   prefix + bare,
				bare:   bare,
				dir:    d,
				path:   filepath.Join(d, "SKILL.md"),
				origin: origin,
			})
		}
		for _, f := range c.CommandFiles {
			bare := strings.TrimSuffix(filepath.Base(f), ".md")
			out = append(out, skillCandidate{
				name:    prefix + bare,
				bare:    bare,
				dir:     filepath.Dir(f),
				path:    f,
				origin:  origin,
				command: true,
			})
		}
	}
	return out
}

// discoveredSkillItem holds internal metadata for a resolved skill.
type discoveredSkillItem struct {
	skill       Skill
	frontmatter *skill.Frontmatter
	body        string
}

// skillsGeneration is bumped whenever a skill or plugin is created,
// changed, installed, or removed. Sources built earlier rescan lazily on
// their next use, so a skill written mid-session is visible to the model
// on its next request without restarting the session.
var skillsGeneration atomic.Uint64

// BumpSkillsGeneration invalidates every live skill source.
func BumpSkillsGeneration() { skillsGeneration.Add(1) }

// skillSource implements skill.Source backed by discovered skills in search directories.
type skillSource struct {
	cwd    string
	mu     sync.RWMutex
	gen    uint64
	skills map[string]discoveredSkillItem
	order  []string
}

// NewSkillSource constructs an ADK skill.Source across all discovery
// roots for cwd plus every enabled plugin. Later roots take precedence
// (project-overrides-global).
func NewSkillSource(cwd string) skill.Source {
	s := &skillSource{cwd: cwd}
	s.rescan()
	return s
}

func (s *skillSource) rescan() {
	gen := skillsGeneration.Load()
	byName := map[string]discoveredSkillItem{}
	for _, c := range skillCandidates(s.cwd) {
		item, ok := loadSkillCandidate(c)
		if !ok {
			continue
		}
		byName[item.skill.Name] = item
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	s.mu.Lock()
	s.gen = gen
	s.skills = byName
	s.order = names
	s.mu.Unlock()
}

// snapshot returns the current skill map, rescanning first when a
// mutation happened since the last scan.
func (s *skillSource) snapshot() (map[string]discoveredSkillItem, []string) {
	s.mu.RLock()
	fresh := s.skills != nil && s.gen == skillsGeneration.Load()
	skills, order := s.skills, s.order
	s.mu.RUnlock()
	if fresh {
		return skills, order
	}
	s.rescan()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.skills, s.order
}

func loadSkillCandidate(c skillCandidate) (discoveredSkillItem, bool) {
	isPlugin := c.origin.Scope == OriginPlugin
	if !isPlugin && (len(c.bare) > skillNameMaxLen || !skillNameRe.MatchString(c.bare)) {
		return discoveredSkillItem{}, false
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return discoveredSkillItem{}, false
	}
	fields, body, ok := ParseFrontmatterBytes(data)
	if !ok {
		if !c.command {
			return discoveredSkillItem{}, false
		}
		fields, body = map[string]string{}, string(data)
	}
	bare := fields["name"]
	if bare == "" {
		bare = c.bare
	}
	if len(bare) > skillNameMaxLen || !skillNameRe.MatchString(bare) {
		return discoveredSkillItem{}, false
	}
	if !isPlugin && bare != c.bare {
		return discoveredSkillItem{}, false
	}
	name := c.name
	if isPlugin {
		prefix, _, _ := strings.Cut(c.name, ":")
		name = prefix + ":" + bare
	}
	desc := strings.TrimSpace(fields["description"])
	if desc == "" {
		if !c.command {
			return discoveredSkillItem{}, false
		}
		desc = firstLine(body)
	}
	if len(desc) > skillDescriptionMaxLen {
		desc = desc[:skillDescriptionMaxLen]
	}

	var adkFM *skill.Frontmatter
	if parsedFM, _, parseErr := skill.ParseBytes(data); parseErr == nil && parsedFM != nil {
		adkFM = parsedFM
	} else {
		adkFM = &skill.Frontmatter{Metadata: fields}
	}
	adkFM.Name = name
	adkFM.Description = desc

	s := Skill{
		Name:                   name,
		BareName:               bare,
		Description:            desc,
		Dir:                    c.dir,
		Path:                   c.path,
		UserInvocable:          fields["user-invocable"] != "false",
		DisableModelInvocation: fields["disable-model-invocation"] == "true",
		Frontmatter:            adkFM,
		Origin:                 c.origin,
		Command:                c.command,
	}
	return discoveredSkillItem{skill: s, frontmatter: adkFM, body: body}, true
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#"))
		if line != "" {
			if len(line) > 120 {
				return line[:120]
			}
			return line
		}
	}
	return "(no description)"
}

func (s *skillSource) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	skills, order := s.snapshot()
	out := make([]*skill.Frontmatter, 0, len(order))
	for _, name := range order {
		if item, ok := skills[name]; ok && item.frontmatter != nil {
			out = append(out, item.frontmatter)
		}
	}
	return out, nil
}

func (s *skillSource) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	skills, _ := s.snapshot()
	if item, ok := skills[name]; ok && item.frontmatter != nil {
		return item.frontmatter, nil
	}
	return nil, skill.ErrSkillNotFound
}

func (s *skillSource) LoadInstructions(ctx context.Context, name string) (string, error) {
	skills, _ := s.snapshot()
	if item, ok := skills[name]; ok {
		return item.body, nil
	}
	return "", skill.ErrSkillNotFound
}

func (s *skillSource) LoadResource(ctx context.Context, name, resourcePath string) (io.ReadCloser, error) {
	skills, _ := s.snapshot()
	item, ok := skills[name]
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
	skills, _ := s.snapshot()
	item, ok := skills[name]
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

// DiscoverSkills walks every root and enabled plugin for skill packages.
// Invalid packages (bad name, missing description) are skipped rather
// than failing the session.
func DiscoverSkills(cwd string) []Skill {
	src := NewSkillSource(cwd)
	asSource, ok := src.(*skillSource)
	if !ok {
		return nil
	}
	skills, order := asSource.snapshot()
	out := make([]Skill, 0, len(order))
	for _, name := range order {
		if item, exists := skills[name]; exists {
			out = append(out, item.skill)
		}
	}
	return out
}

// FindSkill returns the discovered skill with name.
func FindSkill(cwd, name string) (Skill, bool) {
	for _, s := range DiscoverSkills(cwd) {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// SkillBody returns the instruction body of a discovered skill.
func SkillBody(s Skill) string {
	_, body, ok := ParseMarkdownFrontmatter(s.Path)
	if !ok {
		data, err := os.ReadFile(s.Path)
		if err != nil {
			return ""
		}
		return string(data)
	}
	return body
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
	source := NewSkillSource(cwd)
	src := source.(*skillSource)
	skills, _ := src.snapshot()
	item, ok := skills[name]
	if !ok || !item.skill.UserInvocable {
		return "", false
	}
	s := item.skill
	body := strings.TrimSpace(item.body)
	args = strings.TrimSpace(args)
	if strings.Contains(body, "$ARGUMENTS") {
		body = strings.ReplaceAll(body, "$ARGUMENTS", args)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<loaded_skill name=%q path=%q>\n%s\n</loaded_skill>\n\n", s.Name, s.Path, body)
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
	if args != "" {
		fmt.Fprintf(&b, "The user invoked this skill with arguments: %s", args)
	} else {
		b.WriteString("The user invoked this skill with no arguments.")
	}
	return b.String(), true
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

// UnquoteYAML strips quotes surrounding a YAML string value, decoding
// the escapes each quoting style allows.
func UnquoteYAML(s string) string {
	if len(s) < 2 {
		return s
	}
	switch {
	case s[0] == '"' && s[len(s)-1] == '"':
		var out string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
		return s[1 : len(s)-1]
	case s[0] == '\'' && s[len(s)-1] == '\'':
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}
