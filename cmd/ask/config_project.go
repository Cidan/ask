package main

// projectFieldSpec is one editable text field of /config → Project: how
// it is read from and written into the project's config entry.
type projectFieldSpec struct {
	title    string
	helpHint string
	masked   bool
	load     func(askConfig, string) string
	save     func(*projectConfig, string)
}

// projectFieldSpecs are the project's credential and endpoint fields. The
// GitHub rows write into projectConfig.MCP.GitHub and the Linear rows into
// projectConfig.MCP.Linear — the project-level credential slots the
// matching issue provider uses.
var projectFieldSpecs = map[string]projectFieldSpec{
	"githubMCPEndpoint": {
		title:    "GitHub MCP endpoint",
		helpHint: "GitHub MCP server URL; blank uses api.githubcopilot.com/mcp",
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.GitHub.Endpoint
		},
		save: func(p *projectConfig, v string) { p.MCP.GitHub.Endpoint = v },
	},
	"githubMCPToken": {
		title:    "GitHub PAT",
		helpHint: "personal access token with repo and read:org, used when GitHub auth is the PAT",
		masked:   true,
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.GitHub.Token
		},
		save: func(p *projectConfig, v string) { p.MCP.GitHub.Token = v },
	},
	"linearGraphQLEndpoint": {
		title:    "Linear GraphQL endpoint",
		helpHint: "Linear GraphQL URL; blank uses api.linear.app/graphql",
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.Linear.Endpoint
		},
		save: func(p *projectConfig, v string) { p.MCP.Linear.Endpoint = v },
	},
	"linearAPIKey": {
		title:    "Linear API key",
		helpHint: "personal API key (lin_api_…)",
		masked:   true,
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.Linear.Token
		},
		save: func(p *projectConfig, v string) { p.MCP.Linear.Token = v },
	},
	"linearTeamKey": {
		title:    "Linear team key",
		helpHint: "team identifier (e.g. ENG) whose issues the Issues screen shows",
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.Linear.TeamKey
		},
		save: func(p *projectConfig, v string) { p.MCP.Linear.TeamKey = v },
	},
}

// providerActivationReady reports whether the project carries the
// credentials a given issue provider needs before it can dispatch a real
// network call. None ("") is always ready.
func providerActivationReady(p IssueProvider, pc projectConfig) bool {
	switch p.ID() {
	case "":
		return true
	case "github":
		return githubAuthConfigured(pc.MCP.GitHub)
	case "linear":
		return pc.MCP.Linear.Token != "" && pc.MCP.Linear.TeamKey != ""
	}
	return true
}
