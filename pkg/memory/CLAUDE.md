# pkg/memory

Long-term project memory: a SQLite + sqlite-vec store of text entries
embedded by a local llama.cpp model. `Service` implements ADK's
`memory.Service` (`var _ adkmemory.Service = (*Service)(nil)`), so the
ADK runner and its memory tools use it directly.

## Store

- DB: `~/.config/ask/memory/memory.db` (`DefaultDBPath`), opened with
  `mattn/go-sqlite3` after `sqlite_vec.Auto()`. Tables: `project_memory`
  (`id`, `project_id`, `text_payload`, `last_recalled_at`) and the
  `vec0` virtual table `vec_memory(embedding float[<EmbdSize>])`, joined
  on rowid.
- `project_id` is `config.ProjectRoot(cwd)`, falling back to `cwd`.
- `Recall`: cosine distance `< 0.4`, ascending, `LIMIT k`, scoped to the
  project when `cwd` is non-empty (all projects when empty); hits get
  `last_recalled_at` bumped. `Sweep` deletes rows not recalled for 30
  days and runs once in a goroutine at open.

## Embedding

- `Embedder` interface (`Embed`, `EmbdSize`, `Close`). `EmbeddingModel`
  (`embed.go`) is llama.cpp through cgo, `n_ctx` 2048, input truncated
  to 2048 tokens.
- Model: `embeddinggemma-300M-Q8_0.gguf` at
  `~/.local/share/ask/models/` (`DefaultModelPath`); `make download-model`
  fetches it from `ggml-org/embeddinggemma-300M-GGUF`. Loaded only by
  `NewService` when `Options.Embedder` is nil.
- `NewFakeEmbedder(dim)` is the deterministic test embedder (exported;
  `memory_test.go` carries its own normalised private copy).

## API

- `Options{DBPath, ModelPath, Embedder}` → `NewService`. Methods:
  `IsOpen`, `Close`, `Index(ctx, cwd, text)`, `Recall(ctx, cwd, prompt,
  k) []RecallHit`, `Sweep`, `AddSessionToMemory` (every non-thought text
  part of every event; cwd from session state `cwd`, then `project_id`),
  `SearchMemory` (ADK shape, no project filter).
- Process-wide default: `Open(opts)`, `Close`, `IsOpen`, `Index`,
  `Recall`, `Sweep`, `Default`, `SetDefault`.
- `recall.go`: `DefaultRecallK = 5`, `DefaultHookTimeout = 8s`,
  `FormatRecallContext(hits, heading)` (numbered markdown list),
  `SystemBlock(ctx, cwd)` (query "current project context"),
  `PromptContext(ctx, cwd, prompt)`.

## Lifecycle and injection points

- `cmd/ask/main.go` calls `openMemoryService` (`aliases.go` →
  `memory.Open(memory.Options{})`, all defaults) at startup and defers
  `closeMemoryService`. A failed open prints to stderr and ask runs
  without memory. There is no config flag.
- System prompt: `pkg/engine/prompt.go` appends `SystemBlock` as
  `<project_memory>` (2s timeout) when `IsOpen`.
- Per turn: ADK's `preload_memory` (`tools.PreloadMemoryTool`, core)
  searches the first text part of the user message through
  `ctx.SearchMemory` and appends the hits to the instructions.
  `engine.RunnerBuilder` (`pkg/engine/run.go`) hands the runner
  `memory.Default()` when open, else `adkmemory.InMemoryService()`.
- On demand: `load_memory` (`tools.LoadMemoryTool`, core) queries
  through `engine.NewStandaloneAgentContext(ctx).SearchMemory`.
- Per file: `tools.WrapFileToolsWithMemory` wraps `read`/`edit`/`write`
  in `MemoryAwareTool`; a non-error result gets `Recall(cwd, file_path)`
  appended via `engine.AppendToolResultText` under `Memory for <path>`.
- Writing: `memory_index` (`tools.MemoryIndexTool`, deferred registry,
  approval-gated) → `memory.Index`.
- Workflows: `engine.IngestWorkflowMemory` → `AddSessionToMemory` on a
  clean finish only (`pkg/engine/workflow_run.go`,
  `cmd/ask/workflow_graph.go`).

Closed service: `SystemBlock`/`PromptContext` return `""`,
`MemoryAwareTool` passes through, `SearchMemory` returns empty, the
runner uses ADK's in-memory service; `Index`/`Recall` return
`memory service closed`, so `memory_index` errors.

## Build and test

- `embed.go`'s `#cgo` lines point at `build/llama.cpp/{include,ggml/include}`
  and the static libs under `build/llama.cpp/build/{src,ggml/src}`;
  `make setup-llama` clones and builds them. Nothing in this package
  builds without that.
- sqlite-vec's cgo includes `sqlite3.h`, which lives at the repo root
  (`sqlite3.h`, `sqlite3ext.h`) and is found only through the Makefile's
  `CGO_CFLAGS=-I$(PWD)`. Run `go build`/`go test` through `make` or with
  the Makefile's three `CGO_*` exports in the environment.
- Tests use `NewFakeEmbedder` + a `t.TempDir()` DB. The default service
  is global: tests that `Open` it must `Close` it first and defer `Close`
  (`pkg/tools/memory_test.go`, `pkg/engine/run_test.go`).
  `TestRealModel_IfAvailable` loads the real gguf from the real `$HOME`
  and skips when it is absent.
