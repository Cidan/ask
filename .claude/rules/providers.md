---
paths:
  - "pkg/providers/**"
  - "pkg/config/**"
  - "pkg/engine/run.go"
  - "pkg/engine/retry.go"
  - "pkg/engine/session.go"
  - "pkg/engine/workflow_run.go"
  - "cmd/ask/agent_provider.go"
  - "cmd/ask/agent_run.go"
  - "cmd/ask/config_fields.go"
  - "cmd/ask/config_modal.go"
  - "cmd/ask/model_catalog.go"
  - "cmd/ask/model_picker.go"
  - "cmd/ask/model_picker_view.go"
  - "cmd/ask/tab_title.go"
  - "cmd/ask/usage.go"
  - "cmd/ask/workflow_graph.go"
  - "cmd/ask/agent_tools_task.go"
---
# LLM providers

`pkg/providers/provider.go` is the single contract for an LLM provider.
Everything provider-shaped in the TUI and the engine is generated from
the registry; nothing outside `pkg/providers` may name a provider.

## The contract

- `providers.Provider` is a Go **interface**. Every method is required.
  Optional behaviour is a separate interface discovered by type
  assertion (`ModelLister`, `NativeWebSearchProvider`, `CheapModeler`
  today; `CheapModeler` names the cheapest model for background calls
  such as memory extraction when the catalog carries no prices —
  `providers.CheapestModel` consults it before list prices). Never model
  a provider as a struct
  of func fields with nil-means-default — that is the design this
  replaced, and it produced the same field nil-checked in one caller
  and called blind in another.
- `CostReporter` marks a provider whose calls report what they cost
  (Claude Code, OpenRouter); `providers.ReportsCost(id)` reads it so the
  sidebar shows a cost from the start even for a model the catalog cannot
  price.
- Models have one optional capability of their own: `HistoryRebaser`, for
  a model that holds the conversation outside the request (Claude Code's
  child). Auto-compaction applies to every provider alike and calls
  `providers.RebaseHistory(m)` whenever it moves its cut; such a model
  must then rebuild what it holds from its next request. Wrappers
  (`retryingModel`) forward it. There is no per-provider compaction
  opt-out.
- Pin every implementation: `var _ Provider = Name{}` (and
  `var _ ModelLister = Name{}` when it lists models).
- `Register` panics on a malformed provider (empty id; a setting key
  that is empty, duplicated, or one of `config.ProviderConfigReservedKeys`).
  That is a programming error and must stay a panic.

## Providers today

`builtin` in provider.go is `[]Provider{Vertex{}, OpenRouter{},
ClaudeCode{}}`; Vertex is `DefaultProviderID()`. All three implement
`ModelLister`.

- **Vertex** (`vertex.go`): Gemini through ADK's `gemini.NewModel` with
  `genai.BackendVertexAI`. Settings `project` (env `GOOGLE_CLOUD_PROJECT`,
  required — `BuildModel` fails without it), `location` (default
  `global`), `serviceAccountKey` (env `GOOGLE_APPLICATION_CREDENTIALS`,
  tilde-expanded; blank = ADC). `VertexPrepareCredentials` loads the key
  into `genai.ClientConfig.Credentials` — never mutate the process env.
  Default model `VertexDefaultModel` (`gemini-3.7-flash`). `ListVertexModels`
  and `FilterVertexModelOptions` drop Claude/Anthropic ids. Seams:
  `VertexCredentialsLoader`, `VertexPrepareCredentials`, `VertexModel`,
  `VertexNewClient`, `ListVertexModels`.
- **OpenRouter** (`openrouter.go`): thin config over `openai_compat.go`
  (`NewOpenAICompatModel`, the official `openai-go/v3` SDK speaking Chat
  Completions). Settings `apiKey` (Secret, env `OPENROUTER_API_KEY`) and
  `baseURL` (default `https://openrouter.ai/api/v1`). `ListOpenRouterModels`
  also caches each model's live metadata (`cacheOpenRouterMeta`) for
  capabilities and the reasoning encoder. Any future OpenAI-protocol
  provider is another thin config over `OpenAICompatConfig` — do not
  write a second Chat Completions translator.
- **Claude Code** (`claudecode.go`, `claudecode_wire.go`,
  `claudecode_child.go`, `claudecode_model.go`, `claudecode_seed.go`,
  `claudecode_accounting.go`): forks `claude -p` in
  stream-json mode and runs it with ask's tools, not Claude's. Not the
  Anthropic API — a subprocess. Setting `binary` (env `ASK_CLAUDE_BIN`,
  default `claude`); no `Secret` field — auth lives in the binary
  (Claude subscription login, or `ANTHROPIC_API_KEY` in the child's env,
  which passes through). `Configured` = the binary resolves on PATH.
  `BuildModel` returns `claudeCodeModel`, a `model.LLM` that spawns one
  child on the first `GenerateContent` and runs it in lock-step with
  ADK's function-call loop. Invariants: `--tools ""` strips every
  built-in (AskUserQuestion included); ask is an sdk-type MCP server over
  the child's stdio (`--mcp-config … --strict-mcp-config --allowedTools
  mcp__ask`), so every `tools/call` is answered by ADK executing ask's
  tool and the reply riding the next `GenerateContent`'s
  `FunctionResponse`. **Native web-search fallback**: Claude Code implements
  `providers.NativeWebSearchProvider`. When no Brave key is configured
  (`ModelBuilder` records this via `WithWebSearchAvailable`, read in
  `BuildModel`), ask's own Brave-backed `web_search` is *unavailable*, so
  the session omits it (`nativeWebSearchActive` in
  `cmd/ask/agent_observed_tools.go`) and `ccArgv` instead makes the child's
  built-in `WebSearch` available and pre-approved (`--tools WebSearch
  --allowedTools mcp__ask,WebSearch`); a Brave key flips it back off. That
  one tool runs *inside* the child, not over the MCP bridge, so the model
  observes its `tool_use` (assistant frame, name not `mcp__…`) and
  `tool_result` (user frame) off the stream and forwards them to an
  `ObservedToolSink` (injected at build time via `WithObservedToolSink`;
  the session renders them as ordinary `toolCallMsg`/`toolResultMsg`). It
  is observe-only: not gated by ask's approval modal (read-only search),
  and native tokens are already billed into the turn `usage`. The sink is
  wired for the interactive session (`StartSession`); tab-title and
  workflow-step children build without one, so their native calls run but
  aren't rendered. Claude's own CLAUDE.md/auto-memory/skills are
  switched off (`--setting-sources "" --settings
  '{"autoMemoryEnabled":false}'`) so only ask's `BuildSystemPrompt`
  reaches the model. **The CLI's own compaction is off**
  (`DISABLE_COMPACT=1` in `ccSessionEnv`): `--setting-sources ""` does
  not stop it reading `autoCompactEnabled` from the legacy
  `~/.claude.json`, and a child that compacted itself would hold a history
  ask never sent. **History:** the child runs `--no-session-persistence`
  and holds the conversation in memory; a running child is sent only
  what follows the last request content it was sent (`consumed`, a mark
  keyed on role + first part, because request hooks add one-off contents
  and parts that the next request no longer carries — falling back to
  the child's latest reply). Whenever it must start from history — the
  first call of a resumed or materialized session, a request sharing
  nothing with the child (a workflow loop's next iteration), or the call
  after `RebaseHistory` (ask's compaction moved its cut) — the old child
  is killed and a new one is started with `--resume <tmp>.jsonl`: the
  request history written as a Claude Code transcript
  (`writeClaudeSeedFile`; lines need only `type`, `uuid`, `parentUuid`,
  `sessionId`, `timestamp`, `message`), with real `tool_use` /
  `tool_result` blocks, tool ids minted fresh and paired, thoughts
  dropped (no signatures), consecutive user contents merged tool-results
  first. The newest user content starts the turn over stdin; a history
  ending mid-turn on a tool result is seeded whole and restarted with
  `ccSeedNudge` — an unanswered `tool_use` at the end of a seed is re-run
  by the CLI, never answered. The child is
  killed through the `io.Closer` capability (`engine.CloseModel`, forwarded
  by `retryingModel`); the system-prompt and seed temp files go with it.
  `ListModels` forks a short-lived child, reads the
  account's models out of the `initialize` control response's `models`
  array (`probeClaudeCodeModels`), and caches each model's live metadata
  (`cacheClaudeCodeMeta`, layered by `ModelMetaFor` through
  `mergeProviderNative`); it falls back to the static catalog on a probe
  failure so the picker is never empty. Seams: `ccDial` (swap for a
  scripted `ccConn` in tests — frame level, no process) and
  `ClaudeCodeStart` (swap for a fake `ClaudeCodeProcess` speaking NDJSON —
  what `pkg/engine`'s end-to-end compaction test uses). Opt-in live tests
  (`ASK_CC_LIVE=1`, run against the real `$HOME` via
  `testhome.UseRealHome`) cover a real rebuild mid-turn and the usage and
  cost accounting against the CLI's own. Known v1 limits: the
  system prompt is captured at spawn (a mid-session change — only
  workflow state blocks — is not restarted; each workflow step gets a
  fresh child anyway); tab-title and workflow-step children are closed
  by their run, chat/session children by `Session.Close`.

## Usage and cost (`usage.go`)

- **Every adapter fills genai usage metadata with Gemini's semantics**:
  `PromptTokenCount` counts every input token, cached ones included;
  `CachedContentTokenCount` is the cached-read subset;
  `CandidatesTokenCount` excludes thinking, which is
  `ThoughtsTokenCount` (billed as output); `TotalTokenCount` is the
  context the model holds after the call. `UsageFromMetadata` reads that
  into a `providers.Usage`.
- `providers.Usage` is the one accounting record: `Provider`/`Model`,
  `ContextTokens`, `InputTokens` (full-rate input only),
  `CacheReadTokens`, `CacheWriteTokens`, `OutputTokens` (thinking
  included), `ThinkingTokens`, `CostUSD` + `CostSource` (`reported` by the
  provider — authoritative; `priced` from the catalog; empty = unknown).
  It rides `LLMResponse.CustomMetadata` (`AttachUsage` / `UsageOf`, which
  also decodes it back from a stored event). An adapter that knows more
  than the metadata can hold attaches its own; `engine.ModelBuilder`'s
  wrapper completes and prices the rest.
- `StepCostUSD` / `PriceUsage` price cache reads and writes at their own
  rates; a model that lists none is charged the input rate for them,
  never nothing.
- **OpenRouter** (`openai_compat.go`): usage always carries `cost` (USD
  credits) and `prompt_tokens_details.cache_write_tokens`; both land in
  the record (reported cost). Completion tokens include reasoning, so
  candidates are completion − reasoning.
- **Claude Code** (`claudecode_accounting.go`): a step's calls are read
  off the stream — `message_start` for input and cache buckets,
  `message_delta` for the real output count (assistant frames carry a
  1–3 token placeholder). A step sums its calls; its context is the last
  call's input plus output. `result.total_cost_usd` is cumulative over
  the child's life; each turn is charged the difference (`takeResult`),
  as a reported cost on the turn's final response. An interrupt ends the
  CLI's turn with a result frame of its own: `interruptLocked` counts it
  (`staleResults`) so the next read skips it rather than ending the next
  turn empty, and its cost is carried to the next response. Replacing a
  child (`stopLocked(settle)`) interrupts a turn in progress and reads its
  result first, so a rebuilt child's predecessor's spend is not lost.
  The context reading is the CLI's own count
  (`get_context_usage`) plus the call's reply.

## Retry

`engine.ModelBuilder` wraps every model in `retryingModel`
(`pkg/engine/retry.go`). Transient failures (429, 5xx, overload, network,
timeouts, and OpenRouter's HTTP 400 `Provider returned error` wrapper)
retry without a bound, backoff from `config.AgentRetryOptions` capped at
`retryMaxDelay` (30s), until success, cancellation, or a non-transient
error. 400/401/403/404, bad key, context-length errors, and unknown
errors surface at once. A retry only happens before any content was
streamed. `isTransientError` is the one classifier — extend it, do not
add retry loops elsewhere.

## Model metadata

`ModelMetaFor(providerID, modelID)` (`model_meta.go`) layers, lowest
first, each layer overwriting only the fields it actually knows (`merge`):

1. static catalog (`catalog.go`, `CatalogModel`): built-in ids (default
   first), context window, max output, image support, reasoning levels,
   list price. Offline floor. Strict on provider id.
2. models.dev (`modelsdev.go`, `ModelsDevMeta`): `LoadModelsDev` =
   memory → disk cache `~/.config/ask/cache/models-dev.json`
   (`ModelsDevCacheTTL` 24h) → network, with a stale-cache fallback on a
   failed fetch; only the providers in `modelsDevProviderIDs`
   (`vertex`→`google-vertex`, `openrouter`) are parsed. Seams:
   `ModelsDevURL`, `ModelsDevHTTPClient`, `ModelsDevCachePath`.
3. the provider's live listing, merged through `mergeProviderNative`
   (OpenRouter today).

Standing precedence policy, for every provider:

- **Descriptions: models.dev first; provider text only fills a gap.**
  OpenRouter truncates descriptions server-side and Vertex publishes none.
- **Every other fact** (limits, pricing, modalities, reasoning levels,
  dates) — live listing wins over models.dev, which wins over the static
  catalog. A new provider's native layer goes through
  `mergeProviderNative` and inherits both rules; never special-case a
  provider's description. Wrong models.dev data is fixed upstream.
- Vertex's API returns nothing beyond an id, so models.dev is its only
  source of descriptions, limits, and prices.

`StepCostUSD` prices token usage through `ModelMetaLookup` (the test
seam) against the same data, so the cost meter (`cmd/ask/usage.go`) and
the picker detail pane never disagree. Unknown price ⇒ `ok=false`, never
a fake $0.

## Catalog cache (cmd/ask)

`model_catalog.go` holds the process-wide cache behind the picker and the
workflows builder. `modelCatalogRefreshCmd(force)` is single-flight and
re-runs only on `force` or while models.dev has never loaded;
`loadModelCatalogCmd` fetches models.dev and every `ModelLister`
concurrently, installs ids + per-provider error notes
(`cacheModelOptions`, `modelCatalogNotes`), then broadcasts
`modelCatalogLoadedMsg`. Triggered by Ctrl+M, Ctrl+R in the picker, and
entering the workflows screen.

## Steering prompt

`SteeringPrompt(SteeringOptions{InWorkflow, Cwd})` (`steering.go`) is the
coder prompt tail; the in-workflow variant drops the confirmation gates
and adds the `end_turn` requirement, and a `.claude/worktrees/` cwd adds
the stay-inside-the-worktree clause.

## Adding a provider

1. `pkg/providers/<name>.go`: a type implementing `Provider`. Declare
   its configuration as `[]SettingField{Key, Title, Hint, Secret,
   EnvKey, Default, Validate}`. The credential is the one `Secret`
   field; env fallbacks go on `EnvKey`; defaults on `Default`; input
   checks on `Validate` (keep validators in the provider file, exported
   `Validate<Name><Field>`). Read fields with `SettingValue(pc, field)`
   — stored → env → default — never with `pc.Fields[...]` directly.
2. Add it to `builtin` in `provider.go`. Order is the registration
   order; the first entry is `DefaultProviderID()`.
3. Tests: identity/options/limits, `Configured`, `BuildModel`
   fail-fast without credentials, settings order, validators — see
   `vertex_test.go` / `openrouter_test.go` for the shape.

That is the whole change. If adding a provider needs an edit in
`cmd/ask` or `pkg/config`, the abstraction has regressed — fix the
abstraction, do not add the edit.

## Configuration

- A provider's block lives at `Config.Providers[<id>]`
  (`config.ProviderConfig`: `Model`, `SlashCommands`, and `Fields` by
  setting key). On disk it is flat: `"providers": {"vertex": {"model":
  "…", "project": "…"}}`. Do not add per-provider fields to
  `config.Config`.
- Read/write through `cfg.ProviderConfig(id)`, `cfg.SetProviderConfig`,
  `pc.WithField` (copy-on-write), `providers.LoadSettings` /
  `providers.SaveSettings`. Effort is global (`Config.Effort`).
- Legacy top-level `vertex` / `openrouter` blocks are migrated by
  `MigrateLegacyProviderBlocks`; do not extend that list for new
  providers — they never had a legacy block.

## Resolving provider and model

- Never write a provider id literal as a fallback. Use
  `providers.DefaultProviderID()`.
- Never call a provider-specific canonicalizer (`CanonicalVertexModelID`)
  from generic code. Use `p.CanonicalModelID` or, for the
  explicit → configured → default chain, `providers.ResolveModelID`.
- Build models only through `engine.ModelBuilder` — it applies the
  transient-retry decorator and is the test seam. `StartSession` builds
  the model up front so a provider that cannot run fails at session
  start with its own message.
- There is no genai client on the contract. Anything that needs a
  model (tab titles, sub-agents, workflow steps) takes the ADK
  `model.LLM` from `ModelBuilder`.

## UI

- `cmd/ask/config_fields.go` is the only field-editor picker. A
  provider's `/config` row and screen come from `providers.All()` and
  `p.Settings()`; Web Search rides the same picker with its own
  store. Do not clone a picker for a new provider or a new settings
  screen — open `openFieldsPicker` with a `[]SettingField`.
- The model picker's key prompt is `providers.SecretField(p)`; a
  provider without a `Secret` setting never prompts.
- `ProviderConfigured(cfg, id)` is the one gate for "can this provider
  run" (workflow steps, the ⚠ in the skills browser, the `/config`
  row summary).
