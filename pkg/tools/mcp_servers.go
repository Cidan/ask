package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/plugin"
)

const (
	MCPServerTypeStdio = "stdio"
	MCPServerTypeHTTP  = "http"
	MCPServerTypeSSE   = "sse"
)

// MCPServerConfig describes one user-configured MCP server.
type MCPServerConfig struct {
	Type           string            `json:"type,omitempty"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	OAuth          bool              `json:"oauth,omitempty"`
	Disabled       bool              `json:"disabled,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	EnabledTools   []string          `json:"enabledTools,omitempty"`
	DisabledTools  []string          `json:"disabledTools,omitempty"`
}

// EffectiveType resolves type inference for MCP server config.
func (c MCPServerConfig) EffectiveType() string {
	switch c.Type {
	case MCPServerTypeStdio, MCPServerTypeHTTP, MCPServerTypeSSE:
		return c.Type
	}
	if c.Command != "" {
		return MCPServerTypeStdio
	}
	return MCPServerTypeHTTP
}

// ExpandMCPString expands ${VAR} and ${VAR:-default} against the environment.
func ExpandMCPString(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return os.Expand(s, func(key string) string {
		if name, def, ok := strings.Cut(key, ":-"); ok {
			if v := os.Getenv(name); v != "" {
				return v
			}
			return def
		}
		return os.Getenv(key)
	})
}

// Expanded returns a copy with every string field env-expanded.
func (c MCPServerConfig) Expanded() MCPServerConfig {
	out := c
	out.Command = ExpandMCPString(c.Command)
	out.URL = ExpandMCPString(c.URL)
	if len(c.Args) > 0 {
		out.Args = make([]string, len(c.Args))
		for i, a := range c.Args {
			out.Args[i] = ExpandMCPString(a)
		}
	}
	if len(c.Env) > 0 {
		out.Env = make(map[string]string, len(c.Env))
		for k, v := range c.Env {
			out.Env[k] = ExpandMCPString(v)
		}
	}
	if len(c.Headers) > 0 {
		out.Headers = make(map[string]string, len(c.Headers))
		for k, v := range c.Headers {
			out.Headers[k] = ExpandMCPString(v)
		}
	}
	return out
}

// NamedMCPServer pairs a server name with its resolved config.
type NamedMCPServer struct {
	Name   string
	Config MCPServerConfig
}

// LoadDotMCPJSON reads the project-root `.mcp.json`.
func LoadDotMCPJSON(cwd string) map[string]MCPServerConfig {
	root := config.ProjectRoot(cwd)
	if root == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if err != nil {
		return nil
	}
	var file struct {
		MCPServers map[string]MCPServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	return file.MCPServers
}

// fromConfigMCPServer converts a config.MCPServerConfig into the tools shape.
// (config has no Headers field; every other field maps 1:1.)
func fromConfigMCPServer(sc config.MCPServerConfig) MCPServerConfig {
	return MCPServerConfig{
		Type:           sc.Type,
		Command:        sc.Command,
		Args:           sc.Args,
		Env:            sc.Env,
		URL:            sc.URL,
		OAuth:          sc.OAuth,
		Disabled:       sc.Disabled,
		TimeoutSeconds: sc.Timeout,
		EnabledTools:   sc.EnabledTools,
		DisabledTools:  sc.DisabledTools,
	}
}

// expandPluginRoot substitutes ${CLAUDE_PLUGIN_ROOT} (and $CLAUDE_PLUGIN_ROOT)
// with the plugin's installed directory, matching Claude Code's convention
// for referencing files bundled inside a plugin.
func expandPluginRoot(s, root string) string {
	if !strings.Contains(s, "CLAUDE_PLUGIN_ROOT") {
		return s
	}
	return os.Expand(s, func(k string) string {
		if k == "CLAUDE_PLUGIN_ROOT" {
			return root
		}
		return "${" + k + "}"
	})
}

func expandPluginRootConfig(c MCPServerConfig, root string) MCPServerConfig {
	c.Command = expandPluginRoot(c.Command, root)
	c.URL = expandPluginRoot(c.URL, root)
	if len(c.Args) > 0 {
		args := make([]string, len(c.Args))
		for i, a := range c.Args {
			args[i] = expandPluginRoot(a, root)
		}
		c.Args = args
	}
	if len(c.Env) > 0 {
		env := make(map[string]string, len(c.Env))
		for k, v := range c.Env {
			env[k] = expandPluginRoot(v, root)
		}
		c.Env = env
	}
	return c
}

// pluginMCPServer is one MCP server contributed by an enabled plugin.
type pluginMCPServer struct {
	Name   string
	Config MCPServerConfig
	Plugin string // plugin ref, e.g. "name@marketplace"
}

// PluginMCPServers returns the MCP servers declared by every enabled plugin
// (plugin-root .mcp.json and mcps/*.json), with ${CLAUDE_PLUGIN_ROOT}
// expanded to each plugin's installed directory. Later plugins win on a
// name clash; ordering is by plugin ref then server name for determinism.
func PluginMCPServers(cwd string) []pluginMCPServer {
	var out []pluginMCPServer
	for _, in := range plugin.EnabledPlugins(cwd) {
		if in.Dir == "" {
			continue
		}
		ref := in.Ref.String()
		for _, f := range in.Contents().MCPFiles {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var file struct {
				MCPServers map[string]MCPServerConfig `json:"mcpServers"`
			}
			if json.Unmarshal(data, &file) != nil {
				continue
			}
			names := make([]string, 0, len(file.MCPServers))
			for name := range file.MCPServers {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				out = append(out, pluginMCPServer{
					Name:   name,
					Config: expandPluginRootConfig(file.MCPServers[name], in.Dir),
					Plugin: ref,
				})
			}
		}
	}
	return out
}

// MCPServerOrigin identifies where an MCP server was configured.
type MCPServerOrigin int

const (
	MCPOriginPlugin      MCPServerOrigin = iota // a plugin's .mcp.json / mcps/
	MCPOriginProjectFile                        // the project-root .mcp.json
	MCPOriginUser                               // user config mcpServers
	MCPOriginProject                            // per-project config mcpServers
)

func (o MCPServerOrigin) String() string {
	switch o {
	case MCPOriginPlugin:
		return "plugin"
	case MCPOriginProjectFile:
		return ".mcp.json"
	case MCPOriginUser:
		return "user"
	case MCPOriginProject:
		return "project"
	}
	return "unknown"
}

// ResolvedMCPServer is one MCP server with provenance and effective state.
// Config is the stored (un-expanded) config; call Config.Expanded() before
// connecting. Disabled is the effective state after applying overrides.
type ResolvedMCPServer struct {
	Name     string
	Config   MCPServerConfig
	Origin   MCPServerOrigin
	Plugin   string // set when Origin == MCPOriginPlugin
	Disabled bool
}

// effectiveMCPDisabled resolves a server's enabled/disabled state:
// per-project override wins, then the user override, then the server's own
// config flag.
func effectiveMCPDisabled(name string, own bool, userOv, projOv map[string]bool) bool {
	if v, ok := projOv[name]; ok {
		return v
	}
	if v, ok := userOv[name]; ok {
		return v
	}
	return own
}

// ListMCPServers returns every configured MCP server from all sources
// (enabled plugins, project .mcp.json, user config, per-project config)
// with provenance and the effective enabled/disabled state, INCLUDING
// disabled ones. Precedence on a name clash (low -> high): plugin, project
// .mcp.json, user config, per-project config. This is the single source of
// truth for both the browser and the session attach path.
func ListMCPServers(cfg config.Config, cwd string) []ResolvedMCPServer {
	type rec struct {
		cfg    MCPServerConfig
		origin MCPServerOrigin
		plugin string
	}
	merged := map[string]rec{}
	for _, ps := range PluginMCPServers(cwd) {
		merged[ps.Name] = rec{cfg: ps.Config, origin: MCPOriginPlugin, plugin: ps.Plugin}
	}
	for name, sc := range LoadDotMCPJSON(cwd) {
		merged[name] = rec{cfg: sc, origin: MCPOriginProjectFile}
	}
	for name, sc := range cfg.MCPServers {
		merged[name] = rec{cfg: fromConfigMCPServer(sc), origin: MCPOriginUser}
	}
	pc := config.LoadProjectConfig(cfg, cwd) // project-root keyed
	for name, sc := range pc.MCPServers {
		merged[name] = rec{cfg: fromConfigMCPServer(sc), origin: MCPOriginProject}
	}

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ResolvedMCPServer, 0, len(merged))
	for _, name := range names {
		r := merged[name]
		out = append(out, ResolvedMCPServer{
			Name:     name,
			Config:   r.cfg,
			Origin:   r.origin,
			Plugin:   r.plugin,
			Disabled: effectiveMCPDisabled(name, r.cfg.Disabled, cfg.MCPDisabled, pc.MCPDisabled),
		})
	}
	return out
}

// ResolveMCPServers returns the servers that should actually be attached to
// a session: effective-enabled, env-expanded, and non-empty (a command or a
// URL). Disabled servers are dropped.
func ResolveMCPServers(cfg config.Config, cwd string) []NamedMCPServer {
	var out []NamedMCPServer
	for _, r := range ListMCPServers(cfg, cwd) {
		if r.Disabled {
			continue
		}
		sc := r.Config.Expanded()
		if sc.Command == "" && sc.URL == "" {
			continue
		}
		out = append(out, NamedMCPServer{Name: r.Name, Config: sc})
	}
	return out
}

// MCPToolAllowed applies the per-server enable/disable filters.
func MCPToolAllowed(c MCPServerConfig, tool string) bool {
	if len(c.EnabledTools) > 0 {
		ok := false
		for _, t := range c.EnabledTools {
			if t == tool {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, t := range c.DisabledTools {
		if t == tool {
			return false
		}
	}
	return true
}
