---
paths:
  - "pkg/providers/**"
  - "pkg/config/**"
  - "pkg/engine/run.go"
  - "pkg/engine/session.go"
  - "pkg/engine/workflow_run.go"
  - "cmd/ask/agent_provider.go"
  - "cmd/ask/agent_run.go"
  - "cmd/ask/config_fields.go"
  - "cmd/ask/config_modal.go"
  - "cmd/ask/model_picker.go"
  - "cmd/ask/tab_title.go"
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
  assertion (`ModelLister` today). Never model a provider as a struct
  of func fields with nil-means-default — that is the design this
  replaced, and it produced the same field nil-checked in one caller
  and called blind in another.
- Pin every implementation: `var _ Provider = Name{}` (and
  `var _ ModelLister = Name{}` when it lists models).
- `Register` panics on a malformed provider (empty id; a setting key
  that is empty, duplicated, or one of `config.ProviderConfigReservedKeys`).
  That is a programming error and must stay a panic.

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
