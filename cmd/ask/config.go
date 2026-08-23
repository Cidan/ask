package main

import (
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

// configFileMu / withConfigLock serialises read-modify-write cycles against the on-disk
// config (~/.config/ask/ask.json) via pkg/config.WithConfigLock.
func withConfigLock(fn func() error) error {
	return config.WithConfigLock(fn)
}

// Config type aliases pointing to canonical definitions in pkg/config.
type (
	askConfig         = config.Config
	apiProviderConfig = config.APIProviderConfig
	vertexConfig      = config.VertexConfig
	uiConfig          = config.UIConfig
	retryUIConfig     = config.RetryUIConfig
	webSearchConfig   = config.WebSearchConfig
	recentModelRef    = config.RecentModelRef
	projectConfig     = config.ProjectConfig
	projectMCPConfig  = config.ProjectMCPConfig
	githubMCPConfig   = config.GitHubMCPConfig
	linearMCPConfig   = config.LinearMCPConfig
	workflowsConfig   = config.WorkflowsConfig
	workflowDef       = config.WorkflowDef
	workflowStep      = config.WorkflowStep
	workflowSession   = config.WorkflowSession
	issuesConfig      = config.IssuesConfig
)

const (
	workflowScopeUser   = config.WorkflowScopeUser
	workflowScopeRepo   = config.WorkflowScopeRepo
	workflowScopeGlobal = config.WorkflowScopeGlobal
	workflowScopePlugin = config.WorkflowScopePlugin

	workflowStepKindAgent = config.WorkflowStepKindAgent
	workflowStepKindLoop  = config.WorkflowStepKindLoop

	workflowLoopBreak    = config.WorkflowLoopBreak
	workflowLoopContinue = config.WorkflowLoopContinue

	workflowLoopDefaultMaxIterations = config.WorkflowLoopDefaultMaxIterations

	agentDefaultMaxRetries     = config.AgentDefaultMaxRetries
	agentDefaultInitialDelayMs = config.AgentDefaultInitialDelayMs
	agentDefaultBackoffFactor  = config.AgentDefaultBackoffFactor

	githubMCPDefaultEndpoint     = config.GitHubMCPDefaultEndpoint
	linearGraphQLDefaultEndpoint = config.LinearGraphQLDefaultEndpoint
)

func githubMCPEndpointOrDefault(c githubMCPConfig) string {
	return config.GitHubMCPEndpointOrDefault(c)
}

func linearGraphQLEndpointOrDefault(c linearMCPConfig) string {
	return config.LinearGraphQLEndpointOrDefault(c)
}

func projectKey(cwd string) string {
	return config.ProjectKey(cwd)
}

func projectRoot(cwd string) string {
	return config.ProjectRoot(cwd)
}

func loadProjectConfig(cfg askConfig, cwd string) projectConfig {
	return config.LoadProjectConfig(cfg, cwd)
}

func upsertProjectConfig(cfg askConfig, cwd string, pc projectConfig) askConfig {
	return config.UpsertProjectConfig(cfg, cwd, pc)
}

func isProjectConfigEmpty(pc projectConfig) bool {
	return config.IsProjectConfigEmpty(pc)
}

func agentRetryOptions(cfg askConfig) (maxRetries int, initialDelay time.Duration, backoffFactor float64) {
	return config.AgentRetryOptions(cfg)
}

func configPath() (string, error) {
	return config.ConfigPath()
}

func loadConfig() (askConfig, error) {
	return config.Load()
}

func saveConfig(cfg askConfig) error {
	return config.Save(cfg)
}

func migrateLegacyProviderEffort(cfg *askConfig, data []byte) {
	config.MigrateLegacyProviderEffort(cfg, data)
}

func migrateLegacyToolOutput(cfg *askConfig, data []byte) {
	config.MigrateLegacyToolOutput(cfg, data)
}

func migrateLegacyIssuesGitHub(cfg *askConfig, data []byte) {
	config.MigrateLegacyIssuesGitHub(cfg, data)
}

// Provider endpoints and environment keys.
const (
	braveEnvAPIKey = "BRAVE_API_KEY"
)

func resolveBraveAPIKey(c webSearchConfig) string {
	return config.ResolveBraveAPIKey(c)
}

func resolveVertexProject(vc vertexConfig) string {
	return providers.VertexResolveProject(vc)
}

func resolveVertexLocation(vc vertexConfig) string {
	return providers.VertexResolveLocation(vc)
}

const maxRecentModels = 5

func recordRecentModel(providerID, modelID string) {
	if providerID == "" || modelID == "" {
		return
	}
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		ref := recentModelRef{Provider: providerID, Model: modelID}
		out := make([]recentModelRef, 0, maxRecentModels)
		out = append(out, ref)
		for _, r := range cfg.RecentModels {
			if r != ref && len(out) < maxRecentModels {
				out = append(out, r)
			}
		}
		cfg.RecentModels = out
		return saveConfig(cfg)
	}); err != nil {
		debugLog("recordRecentModel %s/%s: %v", providerID, modelID, err)
	}
}

func maskedSummary(v string) string {
	if v == "" {
		return "(not set)"
	}
	return "configured"
}

func plainSummary(v string) string {
	if v == "" {
		return "(not set)"
	}
	return v
}

type memoryPickerRow struct {
	name string
	key  string
	id   string
}

func renderMemoryPickerRow(r memoryPickerRow, width int, selected bool) string {
	nameW := lipgloss.Width(r.name)
	keyW := lipgloss.Width(r.key)
	pad := width - nameW - keyW
	if pad < 1 {
		pad = 1
	}
	if selected {
		plain := r.name + strings.Repeat(" ", pad) + r.key
		if w := lipgloss.Width(plain); w < width {
			plain += strings.Repeat(" ", width-w)
		}
		return configSelectedRowStyle.Render(plain)
	}
	line := r.name + strings.Repeat(" ", pad)
	if r.key != "" {
		line += configKeyDimStyle.Render(r.key)
	}
	return padRight(line, width)
}
