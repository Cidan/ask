package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cidan/ask/pkg/plugin"
)

// The skill/agent store: where ask writes definitions it creates, and
// how it edits or removes the ones it can reach. New definitions land
// in ask's own directories (~/.config/ask/{skills,agents} for user
// scope, <root>/.ask/{skills,agents} for project scope); existing ones
// are edited in place wherever they were discovered. Plugin copies are
// read-only — change the source and republish, or copy the definition
// into user/project scope.

// SkillSpec describes a skill to create.
type SkillSpec struct {
	Name                   string
	Description            string
	Body                   string
	UserInvocable          *bool
	DisableModelInvocation *bool
	License                string
	Compatibility          string
}

// SkillPatch is a partial update; nil fields are left unchanged.
type SkillPatch struct {
	Description            *string
	Body                   *string
	UserInvocable          *bool
	DisableModelInvocation *bool
}

// AgentSpec describes a subagent to create.
type AgentSpec struct {
	Name        string
	Description string
	Prompt      string
	Provider    string
	Model       string
	Tools       []string
}

// AgentPatch is a partial update; nil fields are left unchanged.
type AgentPatch struct {
	Description *string
	Prompt      *string
	Provider    *string
	Model       *string
	Tools       *[]string
}

// AgentsUserDir is where ask writes new user-scope agents.
func AgentsUserDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "ask", "agents")
}

// AgentsProjectDir is where ask writes new project-scope agents.
func AgentsProjectDir(cwd string) string {
	if d := SkillsProjectDir(cwd); d != "" {
		return filepath.Join(filepath.Dir(d), "agents")
	}
	return ""
}

// NormalizeOriginScope maps "" to project and validates writable scopes.
func NormalizeOriginScope(s string) (OriginScope, error) {
	switch strings.TrimSpace(s) {
	case "", string(OriginProject):
		return OriginProject, nil
	case string(OriginUser):
		return OriginUser, nil
	case string(OriginPlugin):
		return "", fmt.Errorf("plugin scope is read-only; write to %q or %q", OriginUser, OriginProject)
	}
	return "", fmt.Errorf("unknown scope %q: valid scopes are %q and %q", s, OriginUser, OriginProject)
}

func writeDirFor(cwd string, scope OriginScope, user, project string) (string, error) {
	switch scope {
	case OriginUser:
		if user == "" {
			return "", fmt.Errorf("no home directory for user scope")
		}
		return user, nil
	case OriginProject:
		if project == "" {
			return "", fmt.Errorf("no project directory")
		}
		return project, nil
	}
	return "", fmt.Errorf("scope %q is not writable", scope)
}

// yamlScalar quotes a frontmatter value when plain YAML would misread it.
func yamlScalar(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return `""`
	}
	needsQuote := strings.ContainsAny(s, ":#\"'\\{}[]&*!|>%@`") ||
		strings.HasPrefix(s, "-") || strings.HasPrefix(s, "?") ||
		s == "true" || s == "false" || s == "null" || s == "yes" || s == "no"
	if !needsQuote {
		return s
	}
	q, _ := json.Marshal(s)
	return string(q)
}

// RenderSkillFile builds SKILL.md content from a spec.
func RenderSkillFile(spec SkillSpec) ([]byte, error) {
	if err := plugin.ValidateName(spec.Name); err != nil {
		return nil, fmt.Errorf("skill %w", err)
	}
	desc := strings.TrimSpace(spec.Description)
	if desc == "" {
		return nil, fmt.Errorf("skill %q needs a description — it is the trigger the model matches against", spec.Name)
	}
	if len(desc) > skillDescriptionMaxLen {
		return nil, fmt.Errorf("skill description is longer than %d characters", skillDescriptionMaxLen)
	}
	if strings.TrimSpace(spec.Body) == "" {
		return nil, fmt.Errorf("skill %q needs a body — the instructions the model follows", spec.Name)
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", spec.Name)
	fmt.Fprintf(&b, "description: %s\n", yamlScalar(desc))
	if spec.License != "" {
		fmt.Fprintf(&b, "license: %s\n", yamlScalar(spec.License))
	}
	if spec.Compatibility != "" {
		fmt.Fprintf(&b, "compatibility: %s\n", yamlScalar(spec.Compatibility))
	}
	if spec.UserInvocable != nil && !*spec.UserInvocable {
		b.WriteString("user-invocable: false\n")
	}
	if spec.DisableModelInvocation != nil && *spec.DisableModelInvocation {
		b.WriteString("disable-model-invocation: true\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(spec.Body))
	b.WriteString("\n")
	return []byte(b.String()), nil
}

// RenderAgentFile builds an agent definition from a spec.
func RenderAgentFile(spec AgentSpec) ([]byte, error) {
	if err := plugin.ValidateName(spec.Name); err != nil {
		return nil, fmt.Errorf("agent %w", err)
	}
	desc := strings.TrimSpace(spec.Description)
	if desc == "" {
		return nil, fmt.Errorf("agent %q needs a description — it is how the model decides when to delegate to it", spec.Name)
	}
	if strings.TrimSpace(spec.Prompt) == "" {
		return nil, fmt.Errorf("agent %q needs a prompt — its system instructions", spec.Name)
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", spec.Name)
	fmt.Fprintf(&b, "description: %s\n", yamlScalar(desc))
	if len(spec.Tools) > 0 {
		fmt.Fprintf(&b, "tools: %s\n", strings.Join(spec.Tools, ", "))
	}
	if spec.Provider != "" {
		fmt.Fprintf(&b, "provider: %s\n", spec.Provider)
	}
	if spec.Model != "" {
		fmt.Fprintf(&b, "model: %s\n", spec.Model)
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(spec.Prompt))
	b.WriteString("\n")
	return []byte(b.String()), nil
}

// allSkills lists every valid skill package, shadowed copies included.
func allSkills(cwd string) []Skill {
	var out []Skill
	for _, c := range skillCandidates(cwd) {
		if item, ok := loadSkillCandidate(c); ok {
			out = append(out, item.skill)
		}
	}
	return out
}

// ResolveEditableSkill finds the one user/project skill called name.
// With scope "" the name must be unambiguous.
func ResolveEditableSkill(cwd, name string, scope OriginScope) (Skill, error) {
	var matches []Skill
	for _, s := range allSkills(cwd) {
		if s.Name != name {
			continue
		}
		if scope != "" && s.Origin.Scope != scope {
			continue
		}
		matches = append(matches, s)
	}
	switch len(matches) {
	case 0:
		if scope != "" {
			return Skill{}, fmt.Errorf("skill %q not found in %s scope", name, scope)
		}
		return Skill{}, fmt.Errorf("skill %q not found", name)
	case 1:
	default:
		return Skill{}, fmt.Errorf("skill %q exists in more than one scope; pass scope to pick one", name)
	}
	s := matches[0]
	if !s.Origin.Editable() {
		return Skill{}, fmt.Errorf("skill %q comes from %s and is read-only; copy it into user or project scope to change it", name, s.Origin)
	}
	return s, nil
}

// CreateSkill writes a new skill package into scope.
func CreateSkill(cwd string, scope OriginScope, spec SkillSpec) (Skill, error) {
	dir, err := writeDirFor(cwd, scope, SkillsUserDir(), SkillsProjectDir(cwd))
	if err != nil {
		return Skill{}, err
	}
	data, err := RenderSkillFile(spec)
	if err != nil {
		return Skill{}, err
	}
	for _, s := range allSkills(cwd) {
		if s.Name == spec.Name && s.Origin.Scope == scope {
			return Skill{}, fmt.Errorf("skill %q already exists in %s scope at %s", spec.Name, scope, s.Path)
		}
	}
	pkg := filepath.Join(dir, spec.Name)
	path := filepath.Join(pkg, "SKILL.md")
	if _, err := os.Stat(path); err == nil {
		return Skill{}, fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		return Skill{}, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return Skill{}, err
	}
	BumpSkillsGeneration()
	if s, err := ResolveEditableSkill(cwd, spec.Name, scope); err == nil {
		return s, nil
	}
	return Skill{Name: spec.Name, BareName: spec.Name, Description: spec.Description, Dir: pkg, Path: path, UserInvocable: true, Origin: Origin{Scope: scope}}, nil
}

// UpdateSkill applies patch to an existing user/project skill in place,
// keeping frontmatter keys it does not model.
func UpdateSkill(cwd, name string, scope OriginScope, patch SkillPatch) (Skill, error) {
	s, err := ResolveEditableSkill(cwd, name, scope)
	if err != nil {
		return Skill{}, err
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return Skill{}, err
	}
	fm, body := splitFrontmatterLines(string(data))
	if fm == nil {
		fm = []string{"name: " + s.BareName, "description: " + yamlScalar(s.Description)}
	}
	if patch.Description != nil {
		desc := strings.TrimSpace(*patch.Description)
		if desc == "" {
			return Skill{}, fmt.Errorf("skill description cannot be empty")
		}
		fm = setFrontmatterKey(fm, "description", yamlScalar(desc))
	}
	if patch.UserInvocable != nil {
		if *patch.UserInvocable {
			fm = dropFrontmatterKey(fm, "user-invocable")
		} else {
			fm = setFrontmatterKey(fm, "user-invocable", "false")
		}
	}
	if patch.DisableModelInvocation != nil {
		if *patch.DisableModelInvocation {
			fm = setFrontmatterKey(fm, "disable-model-invocation", "true")
		} else {
			fm = dropFrontmatterKey(fm, "disable-model-invocation")
		}
	}
	if patch.Body != nil {
		if strings.TrimSpace(*patch.Body) == "" {
			return Skill{}, fmt.Errorf("skill body cannot be empty")
		}
		body = *patch.Body
	}
	if err := os.WriteFile(s.Path, joinFrontmatter(fm, body), 0o644); err != nil {
		return Skill{}, err
	}
	BumpSkillsGeneration()
	if fresh, ok := FindSkill(cwd, name); ok && fresh.Path == s.Path {
		return fresh, nil
	}
	updated, err := ResolveEditableSkill(cwd, name, s.Origin.Scope)
	if err != nil {
		return s, nil
	}
	return updated, nil
}

// DeleteSkill removes a user/project skill package.
func DeleteSkill(cwd, name string, scope OriginScope) error {
	s, err := ResolveEditableSkill(cwd, name, scope)
	if err != nil {
		return err
	}
	defer BumpSkillsGeneration()
	if s.Command {
		return os.Remove(s.Path)
	}
	for _, root := range SkillSearchRoots(cwd) {
		if filepath.Dir(s.Dir) == filepath.Clean(root.Dir) {
			return os.RemoveAll(s.Dir)
		}
	}
	return fmt.Errorf("refusing to delete %s: not inside a skills directory", s.Dir)
}

// ResolveEditableAgent finds the one user/project agent called name.
func ResolveEditableAgent(cwd, name string, scope OriginScope) (SubagentDef, error) {
	var matches []SubagentDef
	for _, d := range loadSubagentDefs(cwd) {
		if d.Name != name {
			continue
		}
		if scope != "" && d.Origin.Scope != scope {
			continue
		}
		matches = append(matches, d)
	}
	switch len(matches) {
	case 0:
		if scope != "" {
			return SubagentDef{}, fmt.Errorf("agent %q not found in %s scope", name, scope)
		}
		return SubagentDef{}, fmt.Errorf("agent %q not found", name)
	case 1:
	default:
		return SubagentDef{}, fmt.Errorf("agent %q exists in more than one scope; pass scope to pick one", name)
	}
	d := matches[0]
	if !d.Origin.Editable() {
		return SubagentDef{}, fmt.Errorf("agent %q comes from %s and is read-only; copy it into user or project scope to change it", name, d.Origin)
	}
	return d, nil
}

// CreateAgent writes a new agent definition into scope.
func CreateAgent(cwd string, scope OriginScope, spec AgentSpec) (SubagentDef, error) {
	dir, err := writeDirFor(cwd, scope, AgentsUserDir(), AgentsProjectDir(cwd))
	if err != nil {
		return SubagentDef{}, err
	}
	data, err := RenderAgentFile(spec)
	if err != nil {
		return SubagentDef{}, err
	}
	for _, d := range loadSubagentDefs(cwd) {
		if d.Name == spec.Name && d.Origin.Scope == scope {
			return SubagentDef{}, fmt.Errorf("agent %q already exists in %s scope at %s", spec.Name, scope, d.Source)
		}
	}
	path := filepath.Join(dir, spec.Name+".md")
	if _, err := os.Stat(path); err == nil {
		return SubagentDef{}, fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return SubagentDef{}, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return SubagentDef{}, err
	}
	BumpSkillsGeneration()
	if d, err := ResolveEditableAgent(cwd, spec.Name, scope); err == nil {
		return d, nil
	}
	return SubagentDef{Name: spec.Name, BareName: spec.Name, Description: spec.Description, Prompt: spec.Prompt, Source: path, Origin: Origin{Scope: scope}}, nil
}

// UpdateAgent applies patch to an existing user/project agent in place.
func UpdateAgent(cwd, name string, scope OriginScope, patch AgentPatch) (SubagentDef, error) {
	d, err := ResolveEditableAgent(cwd, name, scope)
	if err != nil {
		return SubagentDef{}, err
	}
	data, err := os.ReadFile(d.Source)
	if err != nil {
		return SubagentDef{}, err
	}
	fm, body := splitFrontmatterLines(string(data))
	if fm == nil {
		fm = []string{"name: " + d.BareName, "description: " + yamlScalar(d.Description)}
	}
	if patch.Description != nil {
		desc := strings.TrimSpace(*patch.Description)
		if desc == "" {
			return SubagentDef{}, fmt.Errorf("agent description cannot be empty")
		}
		fm = setFrontmatterKey(fm, "description", yamlScalar(desc))
	}
	if patch.Provider != nil {
		fm = setOrDropFrontmatterKey(fm, "provider", strings.TrimSpace(*patch.Provider))
	}
	if patch.Model != nil {
		fm = setOrDropFrontmatterKey(fm, "model", strings.TrimSpace(*patch.Model))
	}
	if patch.Tools != nil {
		fm = setOrDropFrontmatterKey(fm, "tools", strings.Join(*patch.Tools, ", "))
	}
	if patch.Prompt != nil {
		if strings.TrimSpace(*patch.Prompt) == "" {
			return SubagentDef{}, fmt.Errorf("agent prompt cannot be empty")
		}
		body = *patch.Prompt
	}
	if err := os.WriteFile(d.Source, joinFrontmatter(fm, body), 0o644); err != nil {
		return SubagentDef{}, err
	}
	BumpSkillsGeneration()
	updated, err := ResolveEditableAgent(cwd, name, d.Origin.Scope)
	if err != nil {
		return d, nil
	}
	return updated, nil
}

// DeleteAgent removes a user/project agent definition.
func DeleteAgent(cwd, name string, scope OriginScope) error {
	d, err := ResolveEditableAgent(cwd, name, scope)
	if err != nil {
		return err
	}
	defer BumpSkillsGeneration()
	return os.Remove(d.Source)
}

// splitFrontmatterLines returns the frontmatter lines (nil when the file
// has none) and the body that follows.
func splitFrontmatterLines(s string) ([]string, string) {
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return nil, s
	}
	rest := s[strings.Index(s, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, s
	}
	fm := strings.Split(strings.TrimRight(rest[:end], "\r"), "\n")
	body := rest[end+len("\n---"):]
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	for i := range fm {
		fm[i] = strings.TrimRight(fm[i], "\r")
	}
	return fm, body
}

func setFrontmatterKey(lines []string, key, value string) []string {
	for i, l := range lines {
		if k, _, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == key {
			lines[i] = key + ": " + value
			return lines
		}
	}
	return append(lines, key+": "+value)
}

func dropFrontmatterKey(lines []string, key string) []string {
	out := lines[:0]
	for _, l := range lines {
		if k, _, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == key {
			continue
		}
		out = append(out, l)
	}
	return out
}

func setOrDropFrontmatterKey(lines []string, key, value string) []string {
	if value == "" {
		return dropFrontmatterKey(lines, key)
	}
	return setFrontmatterKey(lines, key, value)
}

func joinFrontmatter(fm []string, body string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	for _, l := range fm {
		if strings.TrimSpace(l) == "" {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	return []byte(b.String())
}
