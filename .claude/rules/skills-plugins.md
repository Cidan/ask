---
paths:
  - "pkg/plugin/**"
  - "pkg/engine/skills.go"
  - "pkg/engine/skill_store.go"
  - "pkg/engine/subagents.go"
  - "pkg/engine/plugins.go"
  - "pkg/tools/extensions.go"
  - "cmd/ask/skills_browser*.go"
  - "cmd/ask/agent_tools_task.go"
---
# Skills, subagents, plugins, marketplaces

One distribution format: the Claude Code plugin marketplace
(`pkg/plugin`). A marketplace is a git repo, a local directory, or a bare
`marketplace.json` URL holding `.claude-plugin/marketplace.json`; a plugin
is a directory with `.claude-plugin/plugin.json` plus `skills/`, `agents/`,
`commands/`, and ask's `workflows/*.json`. A plugin ask publishes installs
in Claude Code unchanged; Claude Code ignores `workflows/` and the
`provider:` key on agents. `.claude/rules` and `@`-links are not part of
this — see `pkg/engine/CLAUDE.md`.

## Format (`manifest.go`, `contents.go`, `source.go`)

- `Entry.Source` kinds: `path` (relative to the marketplace), `github`,
  `git`, `url`, `git-subdir` (pinnable by `Ref`/`SHA`). `Author` is a
  string or an object; `PathList` is one path or a list; `ValidateName`
  is the one kebab-case rule for every name.
- `strict` defaults to true (plugin.json is the authority, entry paths
  merge in); `strict:false` lets the entry stand alone so a bare SKILL.md
  directory needs no plugin.json (how `anthropics/skills` ships).
- `ResolveContents` defaults to `skills/`, `agents/`, `commands/`,
  `workflows/`; a `SKILL.md` at the plugin root is a single-skill plugin;
  `commands/*.md` load as skills (`Skill.Command`); paths escaping the
  plugin dir are dropped.
- A plugin's MCP servers come from two shapes, both honored:
  `Contents.MCPFiles` are `.mcp.json`-format files (`{"mcpServers":{…}}`) —
  the plugin-root `.mcp.json` (Claude compat), every `mcps/*.json` (ask's
  directory convention), and any `mcpServers` manifest/entry *paths*.
  `Contents.InlineMCP` carries an inline `mcpServers` *object* declared
  directly in plugin.json or the marketplace entry (the raw `{"name":{…}}`
  map, preserved by `MCPServersField.Inline`; `MarshalJSON` round-trips the
  Claude shape so a persisted entry doesn't decode the Go struct blob back as
  a spurious inline object). `pkg/tools.PluginMCPServers` decodes both
  (inline wins on a name clash; expands `${CLAUDE_PLUGIN_ROOT}`) and
  `ListMCPServers` folds them into the session's servers — see
  `.claude/rules/tools.md`.
- Marketplace sources (`ParseMarketplaceSource`): `owner/repo`, git URL,
  directory, marketplace.json URL. URL marketplaces are never `Writable()`.

## State (`store.go`, `install.go`) — ask-private

| What | user | project |
|------|------|---------|
| marketplaces | `~/.config/ask/plugins/known_marketplaces.json`, clones in `marketplaces/<name>/` | `<root>/.ask/plugins.json` `marketplaces` |
| enabled plugins | `~/.config/ask/plugins/installed_plugins.json` | `<root>/.ask/plugins.json` `enabled` |
| plugin copies | `~/.config/ask/plugins/cache/<mkt>/<plugin>/<version>/` (entry version, else short sha) | same cache; `SyncProject` fetches what the project file names |
| publications | `~/.config/ask/plugins/published.json` | `<root>/.ask/plugins.json` `published` |

`Scope` is `user` (default) or `project`. `EnabledPlugins(cwd)` = user
installs + project file, sorted by `name@marketplace`; a project-enabled
plugin this machine has not fetched is `Missing`. `UninstallPlugin`
deletes the cached copy once no scope references it. Seams: `RunGit`,
`HTTPClient`, `ClaudeHome`, `Now`.

## Discovery (`pkg/engine/skills.go`, `subagents.go`, `skill_store.go`)

- Skill roots, later wins on a bare-name clash: user `~/.config/ask/skills`,
  `~/.agents/skills`, `~/.claude/skills`; project `.agents/skills`,
  `.claude/skills`, `.ask/skills` under cwd, then the project root. Agent
  roots: user `~/.config/ask/agents`, `~/.claude/agents`; project
  `.claude/agents`, `.ask/agents`.
- Plugin items come last as `plugin:name` (`BareName` keeps the short
  form) and never clash. `Origin{Scope, Plugin}` is provenance;
  `Origin.Editable()` is false for plugin copies — republish or copy into
  user/project scope.
- New definitions land in `~/.config/ask/{skills,agents}` or
  `<root>/.ask/{skills,agents}` (`SkillsUserDir` …); `UpdateSkill` /
  `UpdateAgent` rewrite the discovered file in place and keep frontmatter
  keys they do not model.
- Every mutation calls `BumpSkillsGeneration`; the per-session ADK
  `skill.Source` (`NewSkillSource` / `NewSkillToolset`) rescans on the
  next request.

## Skills (Agent Skills standard)

- `SKILL.md` frontmatter: `name` (must match the directory), `description`
  (required — missing skips the package), `user-invocable: false` hides
  the `/name` command, `disable-model-invocation: true` drops it from the
  prompt trigger list.
- `SkillsPromptBlock` emits `<available_skills>` with name, description,
  location only; the body stays on disk until the model reads it.
- `ExpandSkillInvocation` turns `/name args` into a `<loaded_skill>`
  block (`$ARGUMENTS` substituted, `@`-linked docs appended).
  `ProbeInit` (`cmd/ask/agent_provider.go`) registers user-invocable
  skills as slash commands.

## Subagents

- Markdown with frontmatter `name`, `description` (required), `tools`
  (comma list), `model`, `provider`; the body is the prompt, `@`-linked
  docs appended under `## @-linked docs`.
- Grants (`SubagentToolNames`): empty or `*` ⇒ `AllSubagentTools` (coding
  core + `search_tools`/`invoke_tool`, `web_search`, `workflow_*`; never
  `task` or the modal pair); unknown names are dropped, nothing valid
  falls back to the full set.
- `task` (`cmd/ask/agent_tools_task.go`) runs a def through a nested
  `engine.Run` on the parent's provider/model; `provider:` repins (model
  resets to that provider's configured/default unless `model:` is set);
  `NewSubagentToolEnv` skips permissions and the todos gate. No `agent:`
  ⇒ the same core set on the parent model. Do not move to ADK's
  `agenttool` — it drops ask's plugins, memory service, and job UI.

## Model-facing tools (`pkg/tools/extensions.go`, registry only)

`skill_list`, `skill_get`, `skill_create`, `skill_edit`, `skill_delete`,
`agent_create`, `agent_edit`, `agent_delete`, `marketplace_list`,
`marketplace_search`, `marketplace_add`, `plugin_install`,
`plugin_uninstall`, `skill_publish`, `skill_pull`. Approval-gated:
`marketplace_add`, `plugin_install`, `skill_publish`, `skill_pull`. Every
successful mutation bumps the generation and emits
`ExtensionsChangedEvent` → `extensionsChangedMsg`, broadcast by `tabs.go`
so every tab re-runs `ProbeInit` and an open browser rebuilds.
`WorkflowProviderWarnings` names steps whose provider fails
`ProviderConfigured`: ⚠ in the browser, launch refused in `tabs.go`.

## Publishing is a link, not an install (`publish.go`, `published.go`)

- `Publish` copies items into `<mkt>/plugins/<name>/`, writes plugin.json,
  upserts the catalog entry as a generic JSON document (unknown fields
  survive), records a `Publication{Kind, Name, Scope, Marketplace, Plugin,
  File, Version, Hash, …}`. Version: explicit > `BumpPatch` of the
  existing > `1.0.0`.
- Git-backed: `pull --ff-only` before, commit, push after, unless
  `NoPush`; a plain directory just gets the files.
- The local copy is the source of truth. `Status` compares local and
  marketplace hashes to the recorded one: in sync · local changes
  (publish again) · marketplace newer (`Pull` / `skill_pull`) · diverged
  (either, after confirm) · missing.

## Import from Claude (`claude.go`)

`ReadClaudeState` reads `~/.claude/plugins/known_marketplaces.json`,
`~/.claude/settings.json`, `<root>/.claude/settings.json`
(`enabledPlugins`, `extraKnownMarketplaces`); `ImportFromClaude` registers
and installs them into ask's store. The only read of Claude Code's plugin
state; nothing under `~/.claude` is written.

## Browser (`cmd/ask/skills_browser.go`, Ctrl+S / `/skills`)

Lenses on Tab: Installed (Project / User / one group per plugin; rows
tagged skill · agent · wf; plus an **MCP Servers** group listing every
configured server from all sources with source · transport · on/off and
an auth indicator — space toggles enable/disable via a user/project scope
chooser, ^o authorizes an OAuth server, ^d/Delete signs it out) and
Marketplace (one group per marketplace,
Enter drills into a plugin; tail rows add / import / create). Plain keys
type into the search, so actions are Ctrl+letter: Ctrl+G install, Ctrl+D
or Delete remove (own item → delete after confirm; plugin-backed →
uninstall), Ctrl+N / Ctrl+E pre-fill `skill_create` / `skill_edit`,
Ctrl+P publish (marketplace chooser; confirm when the marketplace copy
changed), Ctrl+U pull, Ctrl+A add marketplace, Ctrl+X remove marketplace,
Ctrl+R refresh + `SyncProject`. ↑/↓ only for the list, PgUp/PgDn the
detail, Esc / Ctrl+C close or leave a drill. Without the browser:
`/skills add marketplace <src> [project]`, `/skills remove marketplace
<name>`, `/skills import claude`, `/skills refresh`. Mutations are async
cmds resolving to `skillsBrowserOpDoneMsg`.
