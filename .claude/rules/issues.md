---
paths:
  - "cmd/ask/issues.go"
  - "cmd/ask/github_auth.go"
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
- `issueView` is the sub-view interface. `issueViewLayers` is the view
  registry and today contains only kanban; no key cycles views. Detail
  (`Enter`) is outside the registry.
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
  discarded) or `ActionReload` (retry). Only a new load's dispatch
  clears it: a later column's first-chunk success in the same load
  does not, or one failing column would flash its error and then sit
  empty. Every page-load error is `debugLog`ged with its cursor.

## Keys

- `Enter` opens detail (`loadIssueDetailCmd` hydrates when needed),
  `/` opens the search box (provider `QuerySyntaxHelp` underneath; Tab
  swaps raw/AI mode without rewriting the text), `f` runs a workflow
  on the focused issue (`dispatchIssueWorkflow`: focuses the live tab
  when one already runs for that `issueRef.Key()`), `t` on the issues
  screen only (`triageIssueInNewTab`) emits `openPromptTabMsg`: the
  app opens a tab in this tab's cwd (`openPromptTab`, as Ctrl+T would)
  and sends `issueTriagePrompt(ref)` as its first turn through
  `submitPromptMsg` (the composer's Enter path); `issueTrackerName`
  maps provider ids to the tracker's name and new backends extend it.
  `m` on the PRs
  screen starts `startPRMergeFlow` (`IssueMerger.Mergeable` pre-flight
  → confirm → `Merge`), `ActionReload` (unbound by default; Ctrl+R is
  the PRs screen) runs `reloadCurrentQuery`, double Ctrl+C closes the
  tab. A screen's own key (Ctrl+I on issues, Ctrl+R on PRs) goes back
  to ask exactly like Ctrl+O (`discardIssueScreenState`).
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
  MCP server, one cached session per (endpoint, token); `MoveIssue` is
  `issue_write` with state + state_reason. The go-sdk negotiates
  protocol 2026-07-28 with api.githubcopilot.com, where the server
  rejects a `tools/call` lacking the `Mcp-Param-owner` / `-repo`
  headers its schemas declare (`x-mcp-header`); the SDK only sends
  them for tools the session has listed, so `dialGitHubMCP` pages
  through `Tools` before returning. A stateful in-process test
  server stays on 2025-11-25 and never checks — header tests use
  `StreamableHTTPOptions{Stateless: true}`. Server contract, read
  from its live `tools/list`: `list_issues` takes `state`
  OPEN|CLOSED (absent = both), `orderBy` + `direction`, and
  `after=<endCursor>`; `search_issues` / `search_pull_requests` page
  by number with no `after`, so their cursor is the next page number
  (`githubSearchPage` / `githubSearchNextPage`, from `total_count`);
  `issue_read` wraps results as `{"issue":…}` / `{"comments":[…]}`;
  `assignee` / `assignees` are bare login strings while `user` is an
  object, so `githubAPIUserField` decodes either. A known envelope key
  (`issues`, `items`, …) that is present but does not decode is a
  parse error, never an empty page.
  Every call starts with
  `resolveGitHubTarget` (github_auth.go), which follows the project's
  `MCP.GitHub.Auth`: blank is the saved PAT plus owner/repo from
  `git remote get-url origin` (github.com only); `gh` is the gh CLI —
  the repo from `gh repo view --json owner,name,url` (any host gh is
  logged in to; memoised per cwd + `git remote -v`, failures not
  cached) and the token from `gh auth token --hostname <host>` on
  every call, so a re-login rebuilds the session. `Configured` under
  gh does no gh work (it runs on the UI goroutine); gh failures land
  in the load-error modal verbatim. `runGH` is the test seam; no test
  spawns gh. The chat agent's `github` MCP server comes from
  `projectGitHubMCP`, resolved at session attach/reconcile when
  `ProviderSessionArgs.ProjectMCP` is set, never in `sessionArgs`.
  Linear (issue_provider_linear.go): direct
  GraphQL with a personal API key (`IssueRef.Separator` is `-`).
  mcp_linear.go exposes the same provider to the agent as the
  `linear_*` registry tools; mcp_linear_images.go fetches
  `uploads.linear.app` media with the key (no other host) and caps the
  bytes packed into a tool result.
