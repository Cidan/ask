package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mcpServerConfig describes one user-configured MCP server. Servers
// come from three layers, later wins on name clash:
//
//  1. `.mcp.json` at the project root (the cross-tool convention —
//     `{"mcpServers": {"<name>": {...}}}`), so repos already configured
//     for other agents work in ask with zero setup;
//  2. the user-global `mcpServers` map in ~/.config/ask/ask.json;
//  3. the per-project `mcpServers` map (projectConfig).
//
// Type is "stdio" | "http" | "sse"; empty infers stdio when Command is
// set, http otherwise. String fields (command, args, env values, url,
// header values) expand `${VAR}` and `${VAR:-default}` against the
// environment at session start so tokens never have to live in the
// file. Disabled=true at a higher layer removes a server defined by a
// lower one.
type mcpServerConfig struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// OAuth enables the authorization-code flow (browser + dynamic
	// client registration) for http servers that 401 without it.
	OAuth    bool `json:"oauth,omitempty"`
	Disabled bool `json:"disabled,omitempty"`
	// TimeoutSeconds bounds the connect+list handshake. 0 = default.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// EnabledTools, when non-empty, is an allowlist; DisabledTools is
	// a denylist applied after it.
	EnabledTools  []string `json:"enabledTools,omitempty"`
	DisabledTools []string `json:"disabledTools,omitempty"`
}

const (
	mcpServerTypeStdio = "stdio"
	mcpServerTypeHTTP  = "http"
	mcpServerTypeSSE   = "sse"
)

// effectiveType applies the documented inference.
func (c mcpServerConfig) effectiveType() string {
	switch c.Type {
	case mcpServerTypeStdio, mcpServerTypeHTTP, mcpServerTypeSSE:
		return c.Type
	}
	if c.Command != "" {
		return mcpServerTypeStdio
	}
	return mcpServerTypeHTTP
}

// expandMCPString expands ${VAR} and ${VAR:-default} (and bare $VAR)
// against the environment.
func expandMCPString(s string) string {
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

// expanded returns a copy with every string field env-expanded.
func (c mcpServerConfig) expanded() mcpServerConfig {
	out := c
	out.Command = expandMCPString(c.Command)
	out.URL = expandMCPString(c.URL)
	if len(c.Args) > 0 {
		out.Args = make([]string, len(c.Args))
		for i, a := range c.Args {
			out.Args[i] = expandMCPString(a)
		}
	}
	if len(c.Env) > 0 {
		out.Env = make(map[string]string, len(c.Env))
		for k, v := range c.Env {
			out.Env[k] = expandMCPString(v)
		}
	}
	if len(c.Headers) > 0 {
		out.Headers = make(map[string]string, len(c.Headers))
		for k, v := range c.Headers {
			out.Headers[k] = expandMCPString(v)
		}
	}
	return out
}

// loadDotMCPJSON reads the project-root `.mcp.json` (claude-code's
// project MCP convention). Missing or malformed files return nil — a
// broken file must not block a session.
func loadDotMCPJSON(cwd string) map[string]mcpServerConfig {
	root := projectRoot(cwd)
	if root == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if err != nil {
		return nil
	}
	var file struct {
		MCPServers map[string]mcpServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		debugLog("mcp: .mcp.json parse: %v", err)
		return nil
	}
	return file.MCPServers
}

// resolveMCPServers merges the three config layers and returns the
// enabled servers in stable (name-sorted) order, env-expanded and with
// unusable entries (neither command nor url) dropped.
func resolveMCPServers(cfg askConfig, cwd string) []namedMCPServer {
	merged := map[string]mcpServerConfig{}
	for name, sc := range loadDotMCPJSON(cwd) {
		merged[name] = sc
	}
	for name, sc := range cfg.MCPServers {
		merged[name] = sc
	}
	for name, sc := range loadProjectConfig(cfg, cwd).MCPServers {
		merged[name] = sc
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]namedMCPServer, 0, len(merged))
	for _, name := range names {
		sc := merged[name]
		if sc.Disabled {
			continue
		}
		sc = sc.expanded()
		if sc.Command == "" && sc.URL == "" {
			debugLog("mcp: server %s has neither command nor url; skipped", name)
			continue
		}
		out = append(out, namedMCPServer{Name: name, Config: sc})
	}
	return out
}

// namedMCPServer pairs a server name with its resolved config.
type namedMCPServer struct {
	Name   string
	Config mcpServerConfig
}

// mcpToolAllowed applies the per-server enable/disable filters.
func mcpToolAllowed(c mcpServerConfig, tool string) bool {
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
