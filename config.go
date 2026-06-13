package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// configFileMu serialises read-modify-write cycles against the on-disk
// config (~/.config/ask/ask.json). loadConfig and saveConfig are
// individually fine, but a load → mutate → save chain run from two
// goroutines races the file: the second saver clobbers whatever the
// first one persisted. The lock is package-scoped because the file
// itself is the contended resource — every goroutine on the process
// (MCP handlers on independent HTTP threads, the tea loop, the
// workflow tracker's broadcast goroutine) shares one config file.
var configFileMu sync.Mutex

// withConfigLock holds configFileMu around fn. Use it whenever the
// caller intends to load → mutate → save the config in one atomic
// sequence. Callers that only need to read may use loadConfig
// directly; callers that only need to write a wholly-fresh config may
// use saveConfig directly. The lock is what makes interleaved CRUD
// (e.g. concurrent workflow_edit MCP calls) durable instead of
// last-writer-wins.
func withConfigLock(fn func() error) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()
	return fn()
}

// neo4jDefaultHost is what the picker fills in when the user has not
// configured a host explicitly. memmy talks Bolt, and Bolt's
// well-known port is 7687.
const (
	neo4jDefaultHost     = "localhost"
	neo4jDefaultPort     = 7687
	neo4jDefaultDatabase = "neo4j"
)

type askConfig struct {
	// Provider is the agent backend ID ("anthropic", "openai",
	// "deepseek"). Empty means "use the first registered provider" —
	// currently anthropic.
	Provider  string            `json:"provider,omitempty"`
	DeepSeek  apiProviderConfig `json:"deepseek,omitempty"`
	Moonshot  apiProviderConfig `json:"kimi,omitempty"`
	Anthropic apiProviderConfig `json:"anthropic,omitempty"`
	OpenAI    apiProviderConfig `json:"openai,omitempty"`
	UI        uiConfig          `json:"ui,omitempty"`
	Memory    memoryConfig      `json:"memory,omitempty"`
	WebSearch webSearchConfig   `json:"webSearch,omitempty"`

	// MCPServers are user-global MCP servers attached to every
	// in-process agent session. Per-project entries (projectConfig)
	// override on name clash; `.mcp.json` at the project root sits
	// below both. See mcp_servers.go.
	MCPServers map[string]mcpServerConfig `json:"mcpServers,omitempty"`

	// Keybindings overrides the built-in global shortcuts (Ctrl+W
	// workflows, Ctrl+I issues, …). Stored as action → "ctrl+w" so
	// users can hand-edit the file without learning a binary format,
	// and so unknown actions in older builds are skipped instead of
	// crashing. Only entries that diverge from DefaultKeyMap are
	// persisted; an empty/missing map means "all defaults."
	Keybindings map[string]string `json:"keybindings,omitempty"`

	// RecentModels is the most-recently-used list surfaced at the top
	// of the Ctrl+M model picker (newest first, capped at
	// maxRecentModels). Every successful pick push-fronts here.
	RecentModels []recentModelRef `json:"recentModels,omitempty"`

	// Projects holds per-project settings keyed by the canonical
	// absolute path of the project root. Issue-tracking is the first
	// per-project surface; future per-project knobs (custom slash
	// commands, tab-default cwd, project labels, etc.) plug in here
	// without growing the top-level struct.
	//
	// Lives in the user-global config file rather than a per-checkout
	// file because the most security-sensitive field — a GitHub PAT —
	// must NOT be committed. Putting it in ~/.config/ask/ask.json
	// (mode 0600) keeps tokens out of git, out of dotfile-sync repos,
	// and in one place per user.
	Projects map[string]projectConfig `json:"projects,omitempty"`
}

// projectConfig is the per-cwd settings bag. Empty struct = defaults
// (no issue provider configured, etc.). Map zero values are fine —
// loadProjectConfig returns the zero value when the project key is
// absent, so callers don't have to nil-check.
type projectConfig struct {
	Issues    issuesConfig     `json:"issues,omitempty"`
	MCP       projectMCPConfig `json:"mcp,omitempty"`
	Workflows workflowsConfig  `json:"workflows,omitempty"`

	// MCPServers are per-project MCP servers for the in-process agent
	// sessions; they win over the user-global map and `.mcp.json` on
	// name clash. See mcp_servers.go.
	MCPServers map[string]mcpServerConfig `json:"mcpServers,omitempty"`
}

// projectMCPConfig holds the per-project remote-backend credentials.
// Two flavours live here today: GitHub (which is genuinely an MCP
// server we both use locally and inject into the chat agent) and
// Linear (which we hit directly via GraphQL — the name "MCP" is a
// historical leak; the slot is really "per-project per-backend
// credentials"). Future backends — ClickUp, GitLab — sit alongside
// as sibling fields.
//
// Decoupled from issuesConfig so the chat agent can have a GitHub
// MCP wired in without the issues UI being on, and conversely so
// a user enabling a backend's issue surface is forced to configure
// the credential slot first.
type projectMCPConfig struct {
	GitHub githubMCPConfig `json:"github,omitempty"`
	Linear linearMCPConfig `json:"linear,omitempty"`
}

// githubMCPConfig wires the GitHub MCP server. Endpoint defaults to
// the official Copilot MCP host when blank; Token is the user's PAT.
// Token is held in 0600 config — the user explicitly rejected env-var
// indirection ("just config file for that specific project setting"),
// so it lives here.
type githubMCPConfig struct {
	Endpoint string `json:"endpoint,omitempty"`
	Token    string `json:"token,omitempty"`
}

// linearMCPConfig wires the Linear backend. Linear's GraphQL API
// (https://api.linear.app/graphql) is the wire today — the hosted
// MCP at mcp.linear.app/mcp is OAuth-only, so for now we drive
// list/get/move via GraphQL with a personal API key. Endpoint
// defaults to the official GraphQL host when blank.
//
// TeamKey is the Linear team identifier (e.g. "ENG") that scopes
// list/kanban queries — Linear isn't tied to git remotes the way
// GitHub is, so we ask the user explicitly. Without a TeamKey the
// provider reports unconfigured. Token is the personal API key
// (lin_api_…) sent verbatim in the Authorization header (Linear
// expects no "Bearer" prefix for personal keys). Held in 0600
// config alongside the GitHub PAT — same trust model.
type linearMCPConfig struct {
	Endpoint string `json:"endpoint,omitempty"`
	Token    string `json:"token,omitempty"`
	TeamKey  string `json:"teamKey,omitempty"`
}

// workflowsConfig holds the per-project workflows definition list and
// the run-history sidecar. Workflows are user-defined chains of one-
// shot agent calls — pressing `f` on an issue dispatches a workflow
// and the chain runs each step against the same cwd, swapping
// provider/model per step. The structure is deliberately generic
// (nothing about issues here) so future surfaces can reuse the same
// machinery.
type workflowsConfig struct {
	// Items is the ordered list of user-defined workflows. Order is
	// the order they appear in the picker; the user reorders here
	// when they want a different default.
	Items []workflowDef `json:"items,omitempty"`

	// Sessions tracks the persisted per-target run outcomes (terminal
	// states only — `done` and `failed`). The key shape is
	// caller-defined; for issue workflows it's
	// "<provider>:<owner>/<repo>#<number>". `working` is in-memory
	// only (the workflow runtime tracker) so a process restart never
	// surfaces stale "in flight" status.
	Sessions map[string]workflowSession `json:"sessions,omitempty"`
}

// workflowDef is one named pipeline. Names must be unique per
// project; the builder screen enforces that on save. Steps run in
// order — each is a fresh one-shot subprocess with no message
// history carried across the boundary, only assistant text from the
// prior step (forwarded as a `Previous step output:` block in the
// next step's user prompt).
//
// A workflow lives in one of two scopes (see workflow_store.go):
// "user" defs persist under projectConfig.Workflows.Items in
// ~/.config/ask/ask.json; "repo" defs are one-file-per-workflow JSON
// under <projectRoot>/.ask/workflows/ so they can be committed and
// shared. Scope is runtime-only (json:"-") — the storage location IS
// the scope, so the on-disk shapes stay byte-identical to pre-scope
// workflows.
type workflowDef struct {
	Name string `json:"name"`

	// Description is a free-text statement of what the workflow is FOR
	// and when it should be used — the trigger conditions, in the
	// author's own words. It is surfaced verbatim in workflow_list so
	// the agent judges fit against the author's stated intent instead
	// of reverse-engineering purpose from step names (a real failure
	// mode: a "ship" workflow whose steps mention "open PR" got
	// misread as feature-only and wrongly declined for a refactor).
	// Optional — omitempty keeps pre-description workflows
	// byte-identical on disk.
	Description string `json:"description,omitempty"`

	Steps []workflowStep `json:"steps,omitempty"`

	Scope string `json:"-"`
}

const (
	// workflowScopeUser marks a workflow stored in the user's
	// ask.json (machine-local, the pre-scope default). The empty
	// string normalises to user so zero-value defs keep working.
	workflowScopeUser = "user"
	// workflowScopeRepo marks a workflow stored under
	// <projectRoot>/.ask/workflows/ (committed, shared with the team).
	workflowScopeRepo = "repo"
)

// workflowStep is one stage of a workflow. A step is one of two kinds,
// discriminated by Kind:
//
//   - Kind == "" (workflowStepKindAgent): a leaf agent step.
//     Provider+Model nail the agent CLI for that step (so a single
//     workflow can chain claude→codex→claude); empty Model defers to
//     the provider's saved default; Prompt is the user-typed
//     instruction.
//   - Kind == "loop" (workflowStepKindLoop): a loop container. Steps
//     holds the inner agent steps run in order each iteration;
//     MaxIterations caps the iteration count (0 ⇒
//     workflowLoopDefaultMaxIterations); ExitCondition is the free-text
//     goal injected into every inner step's prompt so the agent knows
//     when to break.
//
// Loops nest exactly one layer deep: a loop's inner Steps must all be
// agent steps. That invariant is enforced by validation (validateSteps)
// and by the builder never offering "+ New loop" inside a loop, rather
// than by the type system, so the on-disk shape stays a flat
// []workflowStep that all the pre-loop code keeps walking unchanged.
//
// An empty Kind keeps every workflow authored before loops existed
// byte-identical on disk: the zero value is an agent step.
type workflowStep struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`

	// Agent-step fields (Kind == "").
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Prompt   string `json:"prompt,omitempty"`

	// Loop-step fields (Kind == "loop").
	Steps         []workflowStep `json:"steps,omitempty"`
	MaxIterations int            `json:"maxIterations,omitempty"`
	ExitCondition string         `json:"exitCondition,omitempty"`
}

const (
	// workflowStepKindAgent is the zero-value step kind: a leaf agent
	// step that dispatches one provider subprocess. Stored as "" so
	// pre-loop workflows round-trip unchanged.
	workflowStepKindAgent = ""
	// workflowStepKindLoop marks a loop container step whose inner
	// Steps run repeatedly until an agent registers a break.
	workflowStepKindLoop = "loop"
)

const (
	// workflowLoopBreak ends the loop and resumes the chain after it.
	workflowLoopBreak = "break"
	// workflowLoopContinue runs another iteration of the loop.
	workflowLoopContinue = "continue"
)

// workflowLoopDefaultMaxIterations bounds a loop whose MaxIterations is
// left at 0 ("no explicit limit"). A loop still needs a hard ceiling so
// a chain of "continue" decisions can't spin forever; the user raises
// it explicitly when a workflow legitimately needs more passes.
const workflowLoopDefaultMaxIterations = 10

// isLoop reports whether the step is a loop container.
func (s workflowStep) isLoop() bool { return s.Kind == workflowStepKindLoop }

// effectiveMaxIterations returns the iteration cap actually enforced at
// runtime: the authored MaxIterations when positive, else the default.
func (s workflowStep) effectiveMaxIterations() int {
	if s.MaxIterations > 0 {
		return s.MaxIterations
	}
	return workflowLoopDefaultMaxIterations
}

// workflowSession is the disk-persisted run record. Only terminal
// statuses ("done", "failed") land here; `working` is process-local.
// Workflow records the workflow name so a user can tell at a glance
// which pipeline last touched the issue. StepIndex is the index of
// the step that finalised the run (the step that emitted `done` or
// the step that errored on `failed`). Times are RFC3339 — written by
// the runtime on transition.
type workflowSession struct {
	Workflow  string    `json:"workflow"`
	StepIndex int       `json:"stepIndex"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// issuesConfig is the per-project issue-tracking configuration.
// Provider names which IssueProvider implementation to dispatch to;
// the rest of the fields are provider-specific and only consulted
// when the matching Provider is selected. Backends that need a
// network credential read it from projectConfig.MCP rather than
// duplicating it here — e.g. github issues piggyback on
// projectConfig.MCP.GitHub. This shape lets us add a ClickUp /
// Linear / GitLab block as siblings to GitHub without migrating the
// existing on-disk config.
type issuesConfig struct {
	// Provider is the IssueProvider id ("github", "clickup", …) or
	// "" for "issues not configured for this project". The default
	// is the empty string, which surfaces the "Issues not
	// configured for this project" toast when the user opens the
	// issues screen.
	Provider string `json:"provider,omitempty"`
}

// githubMCPDefaultEndpoint is the official GitHub Copilot-hosted MCP
// server. Used when githubMCPConfig.Endpoint is empty.
const githubMCPDefaultEndpoint = "https://api.githubcopilot.com/mcp"

// githubMCPEndpointOrDefault applies the documented fallback so callers
// don't have to remember the constant.
func githubMCPEndpointOrDefault(c githubMCPConfig) string {
	if c.Endpoint == "" {
		return githubMCPDefaultEndpoint
	}
	return c.Endpoint
}

// linearGraphQLDefaultEndpoint is Linear's hosted GraphQL endpoint.
// Used when linearMCPConfig.Endpoint is empty. Self-hosted variants
// can override this — Linear doesn't ship a self-hosted product
// today but the override slot is cheap and matches GitHub's shape.
const linearGraphQLDefaultEndpoint = "https://api.linear.app/graphql"

// linearGraphQLEndpointOrDefault applies the documented fallback so
// callers don't have to remember the constant.
func linearGraphQLEndpointOrDefault(c linearMCPConfig) string {
	if c.Endpoint == "" {
		return linearGraphQLDefaultEndpoint
	}
	return c.Endpoint
}

// projectKey is the canonical key for projectConfig lookups —
// the resolved git repo root for cwd, or filepath.Clean(abs(cwd))
// when cwd isn't inside a git checkout. Resolving up to the repo
// root means a worktree (~/.claude/worktrees/<name>) and the main
// repo checkout (~/repo) both map to the same project entry, so
// the user only configures GitHub once per repo no matter which
// worktree they happen to be sitting in.
func projectKey(cwd string) string {
	if cwd == "" {
		return ""
	}
	return projectRoot(cwd)
}

// projectRoot walks up from cwd looking for a `.git` directory
// (NOT a file). The directory variant identifies the *main* repo
// checkout; a `.git` file lives in a worktree (and points at the
// main repo's `.git/worktrees/<name>` subdirectory). Returning the
// first directory we hit gives us:
//
//   - main checkout `~/repo`            → `~/repo`
//   - worktree under
//     `~/repo/.claude/worktrees/foo`    → `~/repo` (walks past the
//     worktree's .git file
//     since the file isn't a
//     directory, then up two
//     more levels)
//   - subdir of `~/repo/cmd/x`          → `~/repo`
//   - non-checkout dir `/tmp/scratch`   → `/tmp/scratch`
//
// Falling back to the original abs cwd when no .git is found
// keeps the function total — every caller gets a valid key, even
// when there's no repo around.
func projectRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return filepath.Clean(cwd)
	}
	abs = filepath.Clean(abs)
	dir := abs
	for {
		gitPath := filepath.Join(dir, ".git")
		if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		dir = parent
	}
}

// loadProjectConfig returns the saved projectConfig for cwd, or the
// zero value when no entry exists. Pure read — does not mutate the
// underlying map. Empty cwd returns the zero value too.
func loadProjectConfig(cfg askConfig, cwd string) projectConfig {
	key := projectKey(cwd)
	if key == "" {
		return projectConfig{}
	}
	return cfg.Projects[key]
}

// upsertProjectConfig writes pc into cfg.Projects under cwd's key,
// creating the map if necessary. Returns the modified askConfig (the
// caller saves it). When pc is the zero value, the entry is dropped
// instead of stored so the on-disk file stays minimal.
func upsertProjectConfig(cfg askConfig, cwd string, pc projectConfig) askConfig {
	key := projectKey(cwd)
	if key == "" {
		return cfg
	}
	if isProjectConfigEmpty(pc) {
		delete(cfg.Projects, key)
		if len(cfg.Projects) == 0 {
			cfg.Projects = nil
		}
		return cfg
	}
	if cfg.Projects == nil {
		cfg.Projects = make(map[string]projectConfig)
	}
	cfg.Projects[key] = pc
	return cfg
}

// isProjectConfigEmpty reports whether pc carries no user-meaningful
// data — used by upsertProjectConfig to drop the entry from
// cfg.Projects so the on-disk file stays minimal. Replaces a struct
// equality check that broke when workflowsConfig added a map field.
func isProjectConfigEmpty(pc projectConfig) bool {
	if pc.Issues != (issuesConfig{}) {
		return false
	}
	if pc.MCP != (projectMCPConfig{}) {
		return false
	}
	if len(pc.Workflows.Items) > 0 || len(pc.Workflows.Sessions) > 0 {
		return false
	}
	if len(pc.MCPServers) > 0 {
		return false
	}
	return true
}

// memoryConfig holds the persistent memory toggle and embedder
// credentials. Memory is intentionally per-machine (not per-project):
// the integration plan partitions data via the {project: cwd} tenant
// tuple instead, so one global toggle is the right granularity.
//
// GeminiKey is the API key for Gemini embeddings. When set, the live
// memory service uses a Gemini embedder; when empty, opening the
// service surfaces an error in the picker (memory cannot run on the
// fake embedder in production — its embeddings are deterministic but
// semantically meaningless). The fake embedder is reserved for tests.
//
// The on-disk file already lives at mode 0600 per saveConfig's
// contract, so the key is no more exposed than any other field. We
// will revisit env-var indirection / keychain when remote backends
// land.
type memoryConfig struct {
	Enabled   *bool       `json:"enabled,omitempty"`
	GeminiKey string      `json:"geminiKey,omitempty"`
	Neo4j     neo4jConfig `json:"neo4j,omitempty"`
}

// neo4jConfig holds the Neo4j backend connection parameters. memmy
// v0.2.0 swapped the local bbolt store for a Neo4j-backed one, so the
// embedded library now needs a URI / credentials / database to dial at
// every Open. All fields are optional — empty values fall through to
// the documented defaults at call time so a fresh ask install can talk
// to a default-config Neo4j without any picker interaction.
type neo4jConfig struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
}

// neo4jHostOrDefault, neo4jPortOrDefault, neo4jDatabaseOrDefault apply
// the documented fallbacks so callers (memory.go, picker, validators)
// never have to reason about empty fields. Splitting the defaulting
// out of the struct lets the on-disk JSON stay minimal — only fields
// that differ from the default are persisted.
func neo4jHostOrDefault(c neo4jConfig) string {
	if c.Host == "" {
		return neo4jDefaultHost
	}
	return c.Host
}

func neo4jPortOrDefault(c neo4jConfig) int {
	if c.Port == 0 {
		return neo4jDefaultPort
	}
	return c.Port
}

func neo4jDatabaseOrDefault(c neo4jConfig) string {
	if c.Database == "" {
		return neo4jDefaultDatabase
	}
	return c.Database
}

// neo4jBoltURI assembles the bolt:// URI memmy expects from the
// host/port pair. Always returns a syntactically valid URI; callers
// should run validateNeo4jHost / validateNeo4jPort on the inputs first.
func neo4jBoltURI(c neo4jConfig) string {
	return fmt.Sprintf("bolt://%s:%d", neo4jHostOrDefault(c), neo4jPortOrDefault(c))
}

// validateNeo4jHost screens the picker input. We accept bare hostnames
// and IPs but reject schemes / slashes (URI is built by the helper, not
// pasted whole) and reject empty / whitespace-only strings.
func validateNeo4jHost(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return errors.New("host is required")
	}
	if strings.ContainsAny(t, " /\\") {
		return errors.New("host must not contain spaces or slashes")
	}
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "bolt://") || strings.HasPrefix(low, "neo4j://") ||
		strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return errors.New("enter only the hostname, no scheme")
	}
	return nil
}

// validateNeo4jPort screens the picker input. Must be 1..65535. The
// validator runs on the *string* the user typed so we can return a
// useful error when they paste a non-number; the caller converts to
// int once the validator returns nil.
func validateNeo4jPort(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return errors.New("port is required")
	}
	for _, r := range t {
		if r < '0' || r > '9' {
			return errors.New("port must be a number")
		}
	}
	var n int
	if _, err := fmt.Sscanf(t, "%d", &n); err != nil || n < 1 || n > 65535 {
		return errors.New("port must be 1..65535")
	}
	return nil
}

// apiProviderConfig holds one in-process API provider's settings
// (deepseek, anthropic, openai). Unlike claude/codex there is no CLI
// holding credentials for us, so the API key lives here (0600 config,
// same trust level as the GitHub PAT and Linear key). An empty APIKey
// falls back to the provider's conventional environment variable at
// session start; an empty BaseURL means the provider's default
// endpoint.
type apiProviderConfig struct {
	SlashCommands []providerSlashEntry `json:"slashCommands,omitempty"`
	Model         string               `json:"model,omitempty"`
	Effort        string               `json:"effort,omitempty"`
	APIKey        string               `json:"apiKey,omitempty"`
	BaseURL       string               `json:"baseURL,omitempty"`
}

// deepseekDefaultBaseURL is the OpenAI-compatible endpoint. The /v1
// suffix is path-compatibility only (unrelated to model versions) and
// is what the OpenAI-style SDK expects to prefix /chat/completions.
const deepseekDefaultBaseURL = "https://api.deepseek.com/v1"
// The default is the international platform (platform.kimi.ai /
// platform.moonshot.ai) that issues the kimi-k2.x models ask ships as
// defaults. The China platform (api.moonshot.cn) is a SEPARATE account
// with its own keys — an international key sent to .cn 401s as
// "invalid authentication". China-platform users override base_url in
// their kimi config block.
const moonshotDefaultBaseURL = "https://api.moonshot.ai/v1"

// Conventional environment fallbacks consulted when the config field
// is empty.
const (
	deepseekEnvAPIKey  = "DEEPSEEK_API_KEY"
	moonshotEnvAPIKey  = "MOONSHOT_API_KEY"
	anthropicEnvAPIKey = "ANTHROPIC_API_KEY"
	openaiEnvAPIKey    = "OPENAI_API_KEY"
	braveEnvAPIKey     = "BRAVE_API_KEY"
)

// resolveAPIProviderKey returns the API key to use: an explicit config
// value wins, otherwise the provider's environment variable. Empty
// means unconfigured — session start surfaces a pointed error instead
// of a cryptic 401.
func resolveAPIProviderKey(c apiProviderConfig, envKey string) string {
	if c.APIKey != "" {
		return c.APIKey
	}
	return os.Getenv(envKey)
}

func resolveDeepSeekAPIKey(c apiProviderConfig) string {
	return resolveAPIProviderKey(c, deepseekEnvAPIKey)
}

func resolveKimiAPIKey(c apiProviderConfig) string {
	return resolveAPIProviderKey(c, moonshotEnvAPIKey)
}

func resolveAnthropicAPIKey(c apiProviderConfig) string {
	return resolveAPIProviderKey(c, anthropicEnvAPIKey)
}

func resolveOpenAIAPIKey(c apiProviderConfig) string {
	return resolveAPIProviderKey(c, openaiEnvAPIKey)
}

// webSearchConfig holds the generic web-search settings. Today the only
// knob is the Brave Search API key, used by the native Brave-backed
// web_search tool for providers without first-party search (DeepSeek and
// any other openaicompat backend). Anthropic and OpenAI sessions use
// their provider-executed web search instead and never consult this.
// Held in 0600 config alongside the provider keys — same trust model.
type webSearchConfig struct {
	BraveAPIKey string `json:"braveApiKey,omitempty"`
}

// resolveBraveAPIKey returns the Brave Search key: an explicit config
// value wins, otherwise the BRAVE_API_KEY environment variable. Empty
// means unconfigured — the web_search tool then returns a graceful
// notice telling the model to alert the user instead of failing hard.
func resolveBraveAPIKey(c webSearchConfig) string {
	if c.BraveAPIKey != "" {
		return c.BraveAPIKey
	}
	return os.Getenv(braveEnvAPIKey)
}

// resolveDeepSeekBaseURL returns the configured base URL or the
// default endpoint when unset.
func resolveDeepSeekBaseURL(c apiProviderConfig) string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return deepseekDefaultBaseURL
}

// resolveKimiBaseURL returns the configured base URL or the default
// Moonshot API endpoint when unset.
func resolveKimiBaseURL(c apiProviderConfig) string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return moonshotDefaultBaseURL
}

// recentModelRef is one Ctrl+M picker "Recently used" entry.
type recentModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// maxRecentModels caps the picker's "Recently used" section.
const maxRecentModels = 5

// recordRecentModel push-fronts a pick onto cfg.RecentModels,
// deduping and capping. No-op on blank ids; save errors are logged,
// never surfaced — losing a recents entry must not break a model
// switch.
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

type uiConfig struct {
	QuietMode   *bool `json:"quietMode,omitempty"`
	CursorBlink *bool `json:"cursorBlink,omitempty"`
	RenderDiffs *bool `json:"renderDiffs,omitempty"`
	// ToolOutput is the tri-state for tool-call rendering:
	// "full" | "short" | "off". Empty string defers to
	// defaultToolOutputMode.
	ToolOutput         string `json:"toolOutput,omitempty"`
	SkipAllPermissions *bool  `json:"skipAllPermissions,omitempty"`
	Worktree           *bool  `json:"worktree,omitempty"`
	Theme              string `json:"theme,omitempty"`
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ask", "ask.json"), nil
}

func loadConfig() (askConfig, error) {
	var cfg askConfig
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	_ = json.Unmarshal(data, &cfg)
	migrateLegacyToolOutput(&cfg, data)
	migrateLegacyIssuesGitHub(&cfg, data)
	return cfg, nil
}

// migrateLegacyToolOutput folds the deprecated "renderToolOutput" bool
// into the new tri-state "toolOutput" string so users who upgrade don't
// see their tool rendering reset on first launch. Runs only when the
// new key is absent — an explicit new setting always wins.
func migrateLegacyToolOutput(cfg *askConfig, data []byte) {
	if cfg.UI.ToolOutput != "" {
		return
	}
	var legacy struct {
		UI struct {
			RenderToolOutput *bool `json:"renderToolOutput,omitempty"`
		} `json:"ui,omitempty"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return
	}
	if legacy.UI.RenderToolOutput == nil {
		return
	}
	if *legacy.UI.RenderToolOutput {
		cfg.UI.ToolOutput = string(toolOutputShort)
	} else {
		cfg.UI.ToolOutput = string(toolOutputOff)
	}
}

// migrateLegacyIssuesGitHub lifts the old per-project
// `issues.github.{endpoint,token}` block into the new
// `mcp.github.{endpoint,token}` slot. The github MCP credentials used
// to be tucked under the issue provider's own config; we inverted that
// so the chat agent can have GitHub MCP access without issues being
// wired up, and so enabling github issues piggybacks on a real MCP
// configuration. For each project entry that has the legacy block, we
// preserve a populated endpoint/token unless the new slot already
// carries a non-empty value (an explicit new value always wins).
func migrateLegacyIssuesGitHub(cfg *askConfig, data []byte) {
	if len(cfg.Projects) == 0 {
		return
	}
	var legacy struct {
		Projects map[string]struct {
			Issues struct {
				GitHub struct {
					Endpoint string `json:"endpoint,omitempty"`
					Token    string `json:"token,omitempty"`
				} `json:"github,omitempty"`
			} `json:"issues,omitempty"`
		} `json:"projects,omitempty"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return
	}
	for key, lp := range legacy.Projects {
		pc, ok := cfg.Projects[key]
		if !ok {
			continue
		}
		if pc.MCP.GitHub.Endpoint == "" && lp.Issues.GitHub.Endpoint != "" {
			pc.MCP.GitHub.Endpoint = lp.Issues.GitHub.Endpoint
		}
		if pc.MCP.GitHub.Token == "" && lp.Issues.GitHub.Token != "" {
			pc.MCP.GitHub.Token = lp.Issues.GitHub.Token
		}
		cfg.Projects[key] = pc
	}
}

func saveConfig(cfg askConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".ask.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	removeTmp = false
	return nil
}
