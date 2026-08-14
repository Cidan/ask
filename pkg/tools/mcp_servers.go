package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cidan/ask/pkg/config"
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

// ResolveMCPServers merges project, user, and project-local MCP server configs.
func ResolveMCPServers(cfg config.Config, cwd string) []NamedMCPServer {
	merged := map[string]MCPServerConfig{}
	for name, sc := range LoadDotMCPJSON(cwd) {
		merged[name] = sc
	}
	for name, sc := range cfg.MCPServers {
		merged[name] = MCPServerConfig{
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
	if pc, ok := cfg.Projects[cwd]; ok {
		for name, sc := range pc.MCPServers {
			merged[name] = MCPServerConfig{
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
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]NamedMCPServer, 0, len(merged))
	for _, name := range names {
		sc := merged[name]
		if sc.Disabled {
			continue
		}
		sc = sc.Expanded()
		if sc.Command == "" && sc.URL == "" {
			continue
		}
		out = append(out, NamedMCPServer{Name: name, Config: sc})
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
