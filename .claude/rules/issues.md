---
paths:
  - "cmd/ask/issues.go"
  - "cmd/ask/issue_search.go"
  - "cmd/ask/issue_provider.go"
  - "cmd/ask/issue_provider_github.go"
  - "cmd/ask/issue_provider_linear.go"
  - "cmd/ask/pr_provider_github.go"
  - "cmd/ask/mcp_linear.go"
  - "cmd/ask/mcp_linear_images.go"
---
# Issues and PRs screens

`screenIssues` and `screenPRs` share every piece of machinery in
cmd/ask/issues.go; they differ only in the `IssueProvider` installed on
their `issuesState` (`m.issues` / `m.prs`, seeded by
`ensureIssueState`) and in the PR screen's `m` merge key.

## State

- `issuesState` is the source of truth: `pageCache` (query fingerprint
  → chunk chain, `chunks[i].nextCursor == chunks[i+1].cursor`),
  `queryGen` (bumped on every new query; `issuePageLoadedMsg` with a
  stale `gen` is dropped), `loadCtx`/`cancelLoad` (a new query or
  screen re-entry cancels every in-flight fetch), `currentQuery`,
  `search`, selection, scrollbar, and the loading fields.
- `issueView` is the sub-view interface. `issueViewLayers` holds the
  cycle order and today contains only kanban; `cycleView` is kept so
  the next view type is one registry entry. Detail (`Enter`) is outside
  the cycle.
- Kanban (`kanbanIssueView`) keeps per-column `loaded`, `nextCursor`,
  `hasMore`, `fetching` mirrored from the cache. Every cache mutation
  must update both sides: `removeIssueFromCache` /
  `insertIssueIntoCache` on the state are always paired with the
  matching `column.loaded` edit. `rebuildColumnsFromSpecs` re-stitches
  columns from the cache so a rebuilt view lands where the user was.

## Loader

- `loading` swaps the body for a centred modal; `issueLoadingTickMsg`
  fires every `issueLoadingTickInterval` (33ms), bumps `loadingFrame`,
  and re-arms with `issueLoadingTickCmd` only while `loading` is true,
  so a stale tick no-ops. Entry paths that arm it: screen entry, a
  search-box submit for an uncached query, `reloadCurrentQuery`.
- The modal line is one `issueLoadingSpinnerFrames` glyph plus the
  `loadingMessage` picked once per load. `loadingFrame` is a term of
  `contentFingerprint`, otherwise the frame cache would freeze it.
- `loadErr` owns the screen until Enter/Esc (back to ask, cache
  discarded) or `ActionReload` (retry).

## Keys

- `Enter` opens detail (`loadIssueDetailCmd` hydrates when needed),
  `/` opens the search box (provider `QuerySyntaxHelp` underneath; Tab
  swaps raw/AI mode without rewriting the text), `f` runs a workflow
  on the focused issue (`dispatchIssueWorkflow`: focuses the live tab
  when one already runs for that `issueRef.Key()`), `m` on the PRs
  screen starts `startPRMergeFlow` (`IssueMerger.Mergeable` pre-flight
  → confirm → `Merge`), `ActionReload` (unbound by default; Ctrl+R is
  the PRs screen) runs `reloadCurrentQuery`, double Ctrl+C closes the
  tab.
- Kanban nav: ↑/↓/j/k rows, ←/→/h/l and Tab columns, PgUp/PgDn, g/G.
  Scrolling near the end of a column fetches the next chunk
  (`maybeFetchNextPage`, `fetching` guards a double fire).

## Carry and drop

- `Space` on a card calls `pickupCarry` (only when
  `provider.SupportsCarry()`): the issue leaves `column.loaded` AND
  the cached chunk, and sits on `kanbanIssueView.carry` with its
  origin column/row and status. While carrying, ←/→/h/l/Tab move the
  focused column under the carry (rendered with `carryStyle` at row 0
  of the focused column, consuming no data slot); every other key is
  absorbed (`updateKeyCarrying`); `Esc` calls `cancelCarry`.
- `Space` again calls `dropCarry`. A same-column drop is
  `cancelCarry`. A cross-column drop is optimistic: the issue is
  inserted at row 0 of the target column and cache with the status
  from `provider.KanbanIssueStatus(target)`, then `provider.MoveIssue`
  runs in a cmd with its own `issueMoveTimeout` (30s) context —
  independent of `loadCtx`, so a reload cannot cancel the mutation.
- `issueMoveDoneMsg` (update.go) is a no-op on success. On failure it
  rolls back by issue NUMBER, not by index: remove from the target
  column if still there, re-insert the snapshot at `originRowIdx` in
  the origin column unless already present, then toast the error. A
  reload between drop and rollback therefore degrades to a no-op.
- `cancelCarry` runs before every screen-leave path: `ActionReload`,
  `/`, and `discardIssueScreenState` (screen switch). Closing the tab
  drops the state with it.

## Providers

- `IssueProvider` (issue_provider.go): `ParseQuery`/`FormatQuery`
  (must round-trip), `KanbanColumns`, cursor-paged `ListIssues`
  (`Cursor == ""` is the first chunk; cursors are opaque), `GetIssue`,
  `MoveIssue`, `KanbanIssueStatus`, `IssueRef`, `SupportsCarry`.
  `IssueMerger` is the optional merge capability. `noneIssueProvider`
  is the unconfigured default; `activeIssueProvider` /
  `activePRProvider` resolve from project config.
- GitHub (issue_provider_github.go, pr_provider_github.go): the GitHub
  MCP server, one cached session per (endpoint, token), owner/repo
  from `git remote get-url origin`; `MoveIssue` is `issue_write` with
  state + state_reason. Linear (issue_provider_linear.go): direct
  GraphQL with a personal API key (`IssueRef.Separator` is `-`).
  mcp_linear.go exposes the same provider to the agent as the
  `linear_*` registry tools; mcp_linear_images.go fetches
  `uploads.linear.app` media with the key (no other host) and caps the
  bytes packed into a tool result.
