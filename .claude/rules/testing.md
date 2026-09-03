---
paths:
  - "**/*_test.go"
---
# Tests

Every feature, bug fix, and refactor ships with tests in the matching
`_test.go` of the package it touches. A change that grows the code
without growing the tests is incomplete.

## Running

`pkg/memory/embed.go` is cgo against `build/llama.cpp`, so the package
does not compile without the Makefile's environment:

```
make test                      # builds llama.cpp and fetches the embedding model on first run
```

or, once `build/llama.cpp` exists, directly:

```
export CGO_LDFLAGS="-L$PWD/build/llama.cpp/build/src -L$PWD/build/llama.cpp/build/ggml/src -lllama -lggml -lstdc++ -lm"
export CGO_CXXFLAGS="-I$PWD/build/llama.cpp/include -I$PWD/build/llama.cpp/ggml/include"
export CGO_CFLAGS="-I$PWD/third_party/sqlite"
go test ./...
```

Measured uncached: about 10s wall, of which `cmd/ask` is about 8.5s,
`pkg/engine` about 1s, every other package under 0.4s. Keep it there —
anything slower than that gets a fake, not a longer budget. The one
real-model memory test skips itself unless
`~/.local/share/ask/models/embeddinggemma-300M-Q8_0.gguf` exists.

## What a test asserts on

- Model state, emitted `tea.Msg` values, returned structs, serialized
  JSON bytes, files on disk, exec argv. Never styled output strings or
  view snapshots — rendering is verified by running the app.
- Prefer a few larger scenarios that walk a whole flow over many
  one-line cases, but cover each branch of a complex function.

## Isolation

- `t.TempDir()` for every file-system test; `t.Chdir(...)` when cwd
  matters.
- `isolateHome(t)` (`cmd/ask/testhelpers_test.go`) pins `$HOME` to a
  temp dir for anything that touches config, sessions, skills, plugins,
  or memory paths. Never let a test read or write the user's real
  `~/.config/ask`, `~/.claude`, or `~/.local/share/ask`.
- No subprocesses. The only `exec.Command` calls in tests are `git` and
  `jj` in `cmd/ask/testhelpers_test.go` (`runGit`, `runJJ`, `initGitRepo`,
  `initJJRepo`, gated by `gitAvailable` / `jjAvailable`) and
  `cmd/ask/worktree_lifecycle_test.go`.
- Network goes through `net/http/httptest` servers (MCP, Brave, OpenRouter,
  models.dev, marketplaces) — never a real host.

## Seams and fakes

Swap the package-level var in the test and restore it with `t.Cleanup`.

| Seam | Where | Replaces |
|------|-------|----------|
| `engine.ModelBuilder` | `pkg/engine/run.go` | the ADK model for a provider (scripted `mockLLM` / `scriptedModel` / `fakeLLM`) |
| `engine.GenerateStream` | `pkg/engine/run.go` | GenAI streaming |
| `engine.AgentGitStatus` | `pkg/engine/prompt.go` | git status in `<env>` |
| `agentSendToProgram` | `cmd/ask/agent_tools.go` | modal routing into the Bubble Tea program |
| `generateTabTitleText` | `cmd/ask/tab_title.go` | the tab-title model call |
| `workflowStepModel` | `cmd/ask/workflow_graph.go` | per-step models (`stubWorkflowStepModel`) |
| `tools.RunShell` | `pkg/tools/bash.go` | shell execution for `bash` |
| `tools.BraveSearchClient` | `pkg/tools/web.go` | Brave HTTP client |
| `tools.MCPOAuthOpenBrowser`, `tools.MCPTransportFor` | `pkg/tools/mcp_oauth.go`, `pkg/tools/mcp.go` | browser launch, MCP transport |
| `providers.ModelMetaLookup` | `pkg/providers/model_meta.go` | model metadata for pricing |
| `providers.VertexPrepareCredentials`, `VertexModel`, `ListVertexModels` | `pkg/providers/vertex.go` | Vertex auth, model, listing |
| `providers.ListOpenRouterModels`, `OpenRouterModelBuilder` | `pkg/providers/openrouter.go` | OpenRouter listing, model |
| `providers.ModelsDevURL`, `ModelsDevHTTPClient`, `ModelsDevCachePath` | `pkg/providers/modelsdev.go` | models.dev fetch + cache |
| `plugin.RunGit`, `plugin.HTTPClient`, `plugin.ClaudeHome`, `plugin.Now` | `pkg/plugin` | git, HTTP, `~/.claude`, clock |

Shared helpers in `cmd/ask/testhelpers_test.go`: `fakeProvider`
(`newFakeProvider`, `withRegisteredProviders`), `mockADKModel`,
`newTestModel`, `newTestToolEnv`, `runTool`, `drainCh`, `drainBatch`,
`waitFor`, `writeTestFile`, `testAgentCtx`.

## Where tests live

- `cmd/ask/*_test.go` — the TUI: update dispatch, screens, overlays,
  sidebar, pickers, workflows UI, provider adapter, session store,
  tool glue, worktree/cwd guards, keymap, cost meter.
- `pkg/engine` — runner, session service, prompt assembly, `@`-links,
  rules, skills, subagents, retry, workflow graph integration.
- `pkg/tools` — every native tool, the registry, MCP client, OAuth,
  server config, `contract_test.go` (typed-struct
  contract for all tools).
- `pkg/providers` — registry contract, Vertex, OpenRouter, openai-compat,
  catalog, metadata layering, models.dev client.
- `pkg/workflow` — compile, progress, tracker, store, plugin scope.
- `pkg/plugin` — marketplace format, install/publish/import.
- `pkg/config`, `pkg/diff`, `pkg/memory`, root `ask_test.go`.
