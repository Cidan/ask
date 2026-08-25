// Package plugin implements the Claude Code plugin marketplace format —
// marketplace.json catalogs, plugin.json manifests, the on-disk install
// state — plus the ask-side operations over it: add/refresh marketplaces,
// install/enable plugins at user or project scope, publish local skills,
// agents, and workflows into a marketplace, and import the state Claude
// Code already holds under ~/.claude/plugins.
//
// The file shapes are byte-compatible with Claude Code so a marketplace
// built for one tool installs in the other. ask-only content (workflows,
// the `provider:` key on agents) rides in the same plugin directory;
// Claude Code ignores what it does not know. Plugins may also ship MCP
// servers as a plugin-root `.mcp.json` or an `mcps/` directory of
// `.mcp.json`-format files; ask attaches them to sessions, and Claude Code
// reads the root `.mcp.json` too.
package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	MarketplaceManifestRel = ".claude-plugin/marketplace.json"
	PluginManifestRel      = ".claude-plugin/plugin.json"
)

// Owner is the marketplace owner block.
type Owner struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Author accepts both the object form and a bare string.
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

func (a *Author) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = Author{Name: s}
		return nil
	}
	type raw Author
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*a = Author(r)
	return nil
}

// PathList is a manifest field that may be one path or a list of paths.
type PathList []string

func (p *PathList) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*p = PathList{s}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	*p = list
	return nil
}

// MCPServersField is the manifest/marketplace-entry "mcpServers" component
// field. Claude Code allows either a path (or list of paths) to
// .mcp.json-format files, or an inline object of servers. We honor the path
// forms (they add files that ResolveContents picks up) and tolerate the
// inline-object form without error (those servers are resolved from the
// default .mcp.json / mcps/ locations instead), so neither shape breaks
// manifest parsing.
type MCPServersField struct {
	Paths PathList
}

func (f *MCPServersField) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || string(t) == "null" {
		return nil
	}
	if t[0] == '{' { // inline object: tolerated, not treated as a path
		return nil
	}
	return f.Paths.UnmarshalJSON(b)
}

// MarketplaceManifest is .claude-plugin/marketplace.json.
type MarketplaceManifest struct {
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Owner       Owner                `json:"owner"`
	Metadata    *MarketplaceMetadata `json:"metadata,omitempty"`
	Plugins     []Entry              `json:"plugins"`
}

type MarketplaceMetadata struct {
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	PluginRoot  string `json:"pluginRoot,omitempty"`
}

// Entry is one plugin listed by a marketplace.
type Entry struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Version     string          `json:"version,omitempty"`
	Category    string          `json:"category,omitempty"`
	Homepage    string          `json:"homepage,omitempty"`
	Author      *Author         `json:"author,omitempty"`
	Source      Source          `json:"source"`
	Strict      *bool           `json:"strict,omitempty"`
	Skills      PathList        `json:"skills,omitempty"`
	Agents      PathList        `json:"agents,omitempty"`
	Commands    PathList        `json:"commands,omitempty"`
	Workflows   PathList        `json:"workflows,omitempty"`
	MCPServers  MCPServersField `json:"mcpServers,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	Keywords    []string        `json:"keywords,omitempty"`
}

// IsStrict reports whether plugin.json is the authority for the plugin's
// component paths (the default). strict:false lets the marketplace entry
// stand in for a missing plugin.json — how anthropics/skills ships bare
// SKILL.md directories.
func (e Entry) IsStrict() bool { return e.Strict == nil || *e.Strict }

// Source kinds for a plugin entry.
const (
	SourcePath      = "path"
	SourceGitHub    = "github"
	SourceURL       = "url"
	SourceGitSubdir = "git-subdir"
	SourceGit       = "git"
)

// Source locates a plugin: a path relative to the marketplace, or a git
// repository (optionally pinned to a ref/sha and narrowed to a subdir).
type Source struct {
	Kind string
	Path string
	Repo string
	URL  string
	Ref  string
	SHA  string
}

var githubShorthandRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (s *Source) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*s = ParseSourceString(str)
		return nil
	}
	var obj struct {
		Source string `json:"source"`
		Path   string `json:"path"`
		Repo   string `json:"repo"`
		URL    string `json:"url"`
		Ref    string `json:"ref"`
		SHA    string `json:"sha"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	if obj.Source == "" {
		return fmt.Errorf("plugin source object is missing \"source\"")
	}
	*s = Source{Kind: obj.Source, Path: obj.Path, Repo: obj.Repo, URL: obj.URL, Ref: obj.Ref, SHA: obj.SHA}
	return nil
}

func (s Source) MarshalJSON() ([]byte, error) {
	if s.Kind == "" || s.Kind == SourcePath {
		return json.Marshal(s.Path)
	}
	m := map[string]string{"source": s.Kind}
	if s.Path != "" {
		m["path"] = s.Path
	}
	if s.Repo != "" {
		m["repo"] = s.Repo
	}
	if s.URL != "" {
		m["url"] = s.URL
	}
	if s.Ref != "" {
		m["ref"] = s.Ref
	}
	if s.SHA != "" {
		m["sha"] = s.SHA
	}
	return json.Marshal(m)
}

// ParseSourceString classifies the string form of a plugin source.
func ParseSourceString(str string) Source {
	str = strings.TrimSpace(str)
	switch {
	case strings.HasPrefix(str, "./"), strings.HasPrefix(str, "../"), strings.HasPrefix(str, "/"):
		return Source{Kind: SourcePath, Path: str}
	case strings.Contains(str, "://"), strings.HasPrefix(str, "git@"):
		return Source{Kind: SourceGit, URL: str}
	case githubShorthandRe.MatchString(str):
		return Source{Kind: SourceGitHub, Repo: str}
	}
	return Source{Kind: SourcePath, Path: str}
}

// Remote reports whether the plugin must be fetched from a git repository.
func (s Source) Remote() bool { return s.Kind != "" && s.Kind != SourcePath }

// GitURL is the clone URL for a remote source, "" for a path source.
func (s Source) GitURL() string {
	switch s.Kind {
	case SourceGitHub:
		return "https://github.com/" + strings.TrimSuffix(s.Repo, ".git") + ".git"
	case SourceURL, SourceGitSubdir, SourceGit:
		return s.URL
	}
	return ""
}

// String is the display form of a source.
func (s Source) String() string {
	switch s.Kind {
	case "", SourcePath:
		return s.Path
	case SourceGitHub:
		return "github:" + s.Repo
	}
	out := s.URL
	if s.Path != "" {
		out += " (" + s.Path + ")"
	}
	if s.Ref != "" {
		out += " @" + s.Ref
	}
	return out
}

// PluginManifest is .claude-plugin/plugin.json.
type PluginManifest struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"displayName,omitempty"`
	Version     string          `json:"version,omitempty"`
	Description string          `json:"description,omitempty"`
	Author      *Author         `json:"author,omitempty"`
	Homepage    string          `json:"homepage,omitempty"`
	Repository  string          `json:"repository,omitempty"`
	License     string          `json:"license,omitempty"`
	Keywords    []string        `json:"keywords,omitempty"`
	Skills      PathList        `json:"skills,omitempty"`
	Agents      PathList        `json:"agents,omitempty"`
	Commands    PathList        `json:"commands,omitempty"`
	Workflows   PathList        `json:"workflows,omitempty"`
	MCPServers  MCPServersField `json:"mcpServers,omitempty"`
}

var kebabNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidateName enforces the kebab-case rule shared by marketplace, plugin,
// skill, and agent names.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(name) > 64 {
		return fmt.Errorf("name %q is longer than 64 characters", name)
	}
	if !kebabNameRe.MatchString(name) {
		return fmt.Errorf("name %q must be kebab-case: lowercase letters, digits, and single hyphens", name)
	}
	return nil
}

// ReadMarketplaceManifest parses dir/.claude-plugin/marketplace.json.
func ReadMarketplaceManifest(dir string) (*MarketplaceManifest, error) {
	path := filepath.Join(dir, filepath.FromSlash(MarketplaceManifestRel))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m MarketplaceManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.Name = strings.TrimSpace(m.Name)
	if err := ValidateName(m.Name); err != nil {
		return nil, fmt.Errorf("%s: marketplace %w", path, err)
	}
	return &m, nil
}

// ReadPluginManifest parses dir/.claude-plugin/plugin.json; a missing file
// is (nil, nil) — the marketplace entry then describes the plugin.
func ReadPluginManifest(dir string) (*PluginManifest, error) {
	path := filepath.Join(dir, filepath.FromSlash(PluginManifestRel))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// Entry returns the marketplace's entry for name.
func (m *MarketplaceManifest) Entry(name string) (Entry, bool) {
	if m == nil {
		return Entry{}, false
	}
	for _, e := range m.Plugins {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}
