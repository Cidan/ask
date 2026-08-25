# pkg/memory

Long-term memory as weighted **concepts**: a SQLite + sqlite-vec store of
`Concept{Scope, Kind, Topic, Title, Body, Weight}` rows embedded by a local
llama.cpp model. `Service` implements ADK's `memory.Service`
(`var _ adkmemory.Service = (*Service)(nil)`), so the runner's memory
service is this store; ask's own recall and write paths call it directly.

## Model

- **Concept**: one durable fact. `Title` is the one-line index entry that
  goes into prompts; `Body` is the detail, fetched on demand. `Kind` is
  `user` | `feedback` | `project` | `reference` (`Kinds`, `ValidKind`;
  invalid kinds become `project`). `Scope` is `ScopeFor(cwd)` (the project
  root, else the cleaned cwd) or `ScopeGlobal`. `Topic` is one to three
  lowercase words (`NormalizeTopic`).
- **Weight** (`weights.go`): starts at `WeightInitial` (1.0), capped at
  `WeightCap` (5.0), floored at `WeightFloor` (0.05). Decays lazily on
  every read with half-life `ConceptHalfLife` (100 days) — there is no
  sweeper and nothing is ever deleted by decay. Bumps: `ImplicitBump`
  (0.05) on every non-silent recall; `ExplicitBump` (0.5) through
  `Reinforce` / `Demote`, gated by `RefractoryPeriod` (60s per concept,
  `last_explicit`) and log-dampened toward the cap for positive deltas.
- **Topics** (`memory_topics`): per scope, `UNIQUE(scope, name)`, weighted
  like concepts with `TopicHalfLife` (60 days), bumped by `TouchTopic` and
  by every `Upsert` that names one. `Topics` / `TopicNames` return the
  project's plus global, strongest first, deduped by name.

## Store

- DB: `~/.config/ask/memory/memory.db` (`DefaultDBPath`), `_busy_timeout`
  5s, one open connection. Tables `memory_concepts` (`id, scope, kind,
  topic, title, body, weight, access_count, created_at, last_touched,
  last_explicit`, unix seconds), `memory_topics`, and the `vec0` virtual
  table `vec_concepts(embedding float[<EmbdSize>])` joined on rowid.
  Embedding text is `title + "\n" + body`.
- `migrateLegacy` runs once at open: rows of the pre-concept
  `project_memory` table become `project`-kind concepts (title from the
  first line, scope from `project_id` or global), then the old tables are
  dropped.
- `Options{DBPath, ModelPath, Embedder, Now}`; `Now` is the clock decay
  and refractory math read (tests advance it).

## Retrieval

`Recall(ctx, RecallQuery{Cwd, Query, Topic, K, Silent})`:
1. embed the query; pull up to `CandidateOversample` (50) rows with cosine
   distance `< MaxDistance` (0.4) in the project scope + global (every
   scope when `Cwd` is empty);
2. `Score = sim^RerankSimExp · weight^RerankWeightExp` on the decayed
   weight; sort;
3. `inferTopic`: the topic at least two of the top ten share, else
   `Query.Topic` — that is the turn's topic (`RecallResult.Topic`);
   same-topic candidates get `TopicBoost` (1.15) and the list is re-sorted;
4. cut to `K`; unless `Silent`, bump `weight` / `access_count` /
   `last_touched` on what is returned.

`Top(ctx, cwd, k)` is the session-start list: highest decayed weight,
project + global, no embedding, no bump. `Get`, `Upsert` (zero id inserts;
a known id rewrites and re-embeds with an implicit bump; an unknown id
inserts), `Reinforce` / `Demote` (`applied=false` inside the refractory
window), `Forget` (hard delete). `SearchMemory` is the ADK shape over
`Recall` with no scope. `AddSessionToMemory` reduces the session to its
last exchange (`TurnFromSession`) and hands it to the installed
`Extractor` (`SetExtractor`; errors when none is set).

## Prompt blocks (recall.go)

- `FormatConcepts(concepts, heading, note, bodies)`: `- #id [kind · topic ·
  global] title` per line, the first `bodies` concepts with their body
  indented beneath (`ConceptLine` for one line).
- `SystemBlock(ctx, cwd)`: `Top` `DefaultTopK` (15) with `DefaultBodies`
  (3) bodies under `## Project memory` — the `<project_memory>` block
  `pkg/engine/prompt.go` builds once per session.
- `RecallBlock(ctx, cwd, prompt, topic, exclude)`: per-turn recall,
  `DefaultRecallK` (15), returns the block and the inferred topic; ids in
  `exclude` (`TopIDs`, what the system block already shows) are dropped.
- `FileBlock(ctx, cwd, path)`: a silent 5-hit recall on the path, titles
  only, for the file tools.
- `DefaultHookTimeout` (8s) bounds all of them.

## Embedding

- `Embedder` interface (`Embed`, `EmbdSize`, `Close`). `EmbeddingModel`
  (`embed.go`) is llama.cpp through cgo, `n_ctx` 2048, input truncated to
  2048 tokens. Model: `embeddinggemma-300M-Q8_0.gguf` at
  `~/.local/share/ask/models/` (`DefaultModelPath`); `make download-model`
  fetches it. Loaded only by `NewService` when `Options.Embedder` is nil.
- `NewFakeEmbedder(dim)` is the deterministic test embedder: a hashed,
  L2-normalised bag of words, so texts sharing words are near and
  unrelated texts are not. Use `512` dims in tests — smaller dims collide
  — and write test strings with strong word overlap, since bag-of-words
  cosine is harsher than the real model.

## Lifecycle and injection points

- `cmd/ask/aliases.go` `openMemoryService` → `memory.Open(Options{})` at
  startup, then `engine.EnsureMemoryExtractor()`; `closeMemoryService`
  stops the extractor before `memory.Close()`. A failed open prints to
  stderr and ask runs without memory. No config flag for the store; the
  extraction model is `ask.json` `memory.{provider,model}`.
- Session start: `pkg/engine/prompt.go` appends `SystemBlock` as
  `<project_memory>` (2s timeout) when `IsOpen`.
- Per turn: `tools.PreloadMemoryTool` (`MemoryRecallHook`, core, name
  `preload_memory`) is an ADK request processor: once per invocation it
  runs `RecallBlock` on the turn's user text (topic from the tab, ids in
  the system block excluded) and appends `<memory>…</memory>` as an extra
  part on a copy of the turn's user message — never the system
  instruction, so the cached prefix survives — and reports the inferred
  topic through `onTopic`. Later requests of the same invocation reuse the
  cached block.
- Per file: `tools.WrapFileToolsWithMemory` wraps `read`/`edit`/`write`;
  a non-error result gets `FileBlock` appended via
  `engine.AppendToolResultText`.
- On demand: `load_memory` (core) — query → `Recall` (10, bodies for 5),
  or `id` → `Get`. Registry: `memory_index` (explicit `Upsert`,
  approval-gated), `memory_reinforce`, `memory_demote`, `memory_forget`
  (approval-gated). `tools.MemoryTools` is the set.
- Writing: `engine.MemoryExtractor` (`pkg/engine/memory_extract.go`), the
  one background worker. `EnqueueMemoryTurn` after every TUI turn
  (`agentSession.enqueueMemoryTurn`), every headless `engine.Run` unless
  `RunOptions.SkipMemory` (sub-agents), and every clean workflow finish
  (`IngestWorkflowMemory`). Each job makes one small call
  (`memoryExtractInstruction`, pinned under 1200 bytes by test) on
  `MemoryExtractModel` — `memory.provider`/`memory.model` from config,
  else the session's provider and `providers.CheapestModel` — with the
  prompt head (2000 runes), the answer tail (1500), touched files, the 8
  nearest existing concepts, and the topic list; the JSON reply's
  `new`/`update <id>` concepts are upserted (an `update` to an id not in
  the nearest list inserts) and its topic is touched and handed to
  `OnTopic`. Queue of 32, drop-oldest when full, `Close` cancels the job
  in flight; `Drain` is the test seam. Usage goes to `OnUsage` (the TUI
  posts a `costMsg`).

Closed service: every prompt block returns `""`, `MemoryAwareTool` passes
through, the recall hook is a no-op, `SearchMemory` returns empty,
`EnsureMemoryExtractor` returns nil and `EnqueueMemoryTurn` false; the
tools error with `memory service closed`.

## Build and test

- `embed.go`'s `#cgo` lines point at `build/llama.cpp/{include,ggml/include}`
  and the static libs under `build/llama.cpp/build/{src,ggml/src}`;
  `make setup-llama` clones and builds them. Nothing in this package
  builds without that.
- sqlite-vec's cgo includes `sqlite3ext.h` → `sqlite3.h`; no system
  header is assumed, the two live in `third_party/sqlite/` and are found
  only through the Makefile's `CGO_CFLAGS=-I$(PWD)/third_party/sqlite`.
  Run `go build`/`go test` through `make` or with the Makefile's three
  `CGO_*` exports in the environment.
- Tests use `NewFakeEmbedder(512)` + a `t.TempDir()` DB and a fake clock
  through `Options.Now`. The default service is global: tests that `Open`
  it must `Close` it first and defer `Close` (`pkg/tools/memory_test.go`,
  `pkg/engine/memory_extract_test.go`, `cmd/ask/tab_topic_test.go`), and
  tests that install an extractor `engine.SetMemoryExtractor(nil)` on
  cleanup. `TestRealModel_IfAvailable` loads the real gguf from the real
  `$HOME` and skips when it is absent.
