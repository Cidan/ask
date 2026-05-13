package main

import (
	"fmt"
	"sync"
	"time"

	xansi "github.com/charmbracelet/x/ansi"
)

// Workflow status constants. Empty string is "no record" — used by
// the kanban card renderer to short-circuit before reaching for a
// glyph. Only `done` and `failed` ever land on disk; `working` is
// process-local because there's nothing to resume across restarts.
const (
	workflowStatusWorking = "working"
	workflowStatusDone    = "done"
	workflowStatusFailed  = "failed"
)

// workflowStatusChangedMsg is broadcast by the runtime tracker every
// time an issue's workflow status flips. Issues screens listening for
// this message rebuild their card icons immediately so the kanban
// reflects "working" the moment a workflow tab spawns and the
// terminal status the moment the chain finalises. Empty status means
// the entry was cleared (e.g. cancelled before a single step ran).
type workflowStatusChangedMsg struct {
	issueKey string
	status   string
}

// workflowTrackerEntry is one in-memory record for an issue's
// workflow. `working` entries carry the spawning TabID so a second
// `f` press on the same issue can locate the live tab and focus it
// rather than spawning a duplicate. Terminal statuses leave TabID
// at zero — the tab they ran in may already be closed.
type workflowTrackerEntry struct {
	Status    string
	TabID     int
	Workflow  string
	StepIndex int
	StartedAt time.Time
	UpdatedAt time.Time
}

// workflowTrackerHandle is the singleton state. All mutating methods
// serialise through `mu` so concurrent kanban renders never tear
// against runtime transitions. Reads through `lookup` populate the
// cache on first hit so subsequent renders avoid the disk round-trip.
type workflowTrackerHandle struct {
	mu      sync.Mutex
	entries map[string]workflowTrackerEntry
}

var workflowTrackerSingleton = &workflowTrackerHandle{
	entries: make(map[string]workflowTrackerEntry),
}

// workflowTracker returns the package-wide singleton. The runtime
// tracker is intentionally a singleton (not a per-tab field): a
// single issue can only be the focus of one workflow tab at a time,
// and the kanban needs a flat lookup that doesn't care which tab is
// asking.
func workflowTracker() *workflowTrackerHandle { return workflowTrackerSingleton }

// resetWorkflowTrackerForTest wipes the singleton in-memory map.
// Tests use this to start from a clean slate without process restart.
// Production code never calls this.
func resetWorkflowTrackerForTest() {
	h := workflowTrackerSingleton
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = map[string]workflowTrackerEntry{}
}

// lookup returns the runtime entry for key, hydrating from disk on
// the first miss so subsequent renders are O(1). cwd is the project
// root used to find the matching projectConfig.Workflows.Sessions
// entry. ok=false when neither memory nor disk has a record.
func (h *workflowTrackerHandle) lookup(cwd, key string) (workflowTrackerEntry, bool) {
	h.mu.Lock()
	if e, ok := h.entries[key]; ok {
		h.mu.Unlock()
		return e, true
	}
	h.mu.Unlock()
	cfg, err := loadConfig()
	if err != nil {
		return workflowTrackerEntry{}, false
	}
	pc := loadProjectConfig(cfg, cwd)
	sess, ok := pc.Workflows.Sessions[key]
	if !ok {
		return workflowTrackerEntry{}, false
	}
	e := workflowTrackerEntry{
		Status:    sess.Status,
		Workflow:  sess.Workflow,
		StepIndex: sess.StepIndex,
		StartedAt: sess.StartedAt,
		UpdatedAt: sess.UpdatedAt,
	}
	h.mu.Lock()
	h.entries[key] = e
	h.mu.Unlock()
	return e, true
}

// markWorking flags the issue as in-flight in tab `tabID` running
// `workflow`. Drops any previously-persisted terminal record so a
// re-run isn't visually shadowed by an old icon. Broadcasts the
// transition so live screens repaint.
func (h *workflowTrackerHandle) markWorking(cwd, key, workflow string, tabID int) {
	now := time.Now().UTC()
	h.mu.Lock()
	h.entries[key] = workflowTrackerEntry{
		Status:    workflowStatusWorking,
		TabID:     tabID,
		Workflow:  workflow,
		StepIndex: 0,
		StartedAt: now,
		UpdatedAt: now,
	}
	h.mu.Unlock()
	h.deleteDiskSession(cwd, key)
	broadcastWorkflowStatus(key, workflowStatusWorking)
}

// markStep advances the in-memory step index without changing status.
// Used by the step runner each time a step boundary is crossed so the
// banner has a fresh value. Re-broadcasts the same status so any
// per-step badges (future feature) refresh too.
func (h *workflowTrackerHandle) markStep(key string, stepIdx int) {
	h.mu.Lock()
	e, ok := h.entries[key]
	if !ok {
		h.mu.Unlock()
		return
	}
	e.StepIndex = stepIdx
	e.UpdatedAt = time.Now().UTC()
	h.entries[key] = e
	status := e.Status
	h.mu.Unlock()
	broadcastWorkflowStatus(key, status)
}

// markFinal records a terminal status and persists it. status MUST be
// workflowStatusDone or workflowStatusFailed; any other value is a
// silent no-op. Preserves StartedAt across the working→terminal
// transition so the disk record reflects total run duration.
func (h *workflowTrackerHandle) markFinal(cwd, key, workflow, status string, stepIdx int) {
	if status != workflowStatusDone && status != workflowStatusFailed {
		return
	}
	now := time.Now().UTC()
	h.mu.Lock()
	startedAt := now
	if prev, ok := h.entries[key]; ok && !prev.StartedAt.IsZero() {
		startedAt = prev.StartedAt
	}
	h.entries[key] = workflowTrackerEntry{
		Status:    status,
		Workflow:  workflow,
		StepIndex: stepIdx,
		StartedAt: startedAt,
		UpdatedAt: now,
	}
	h.mu.Unlock()
	h.upsertDiskSession(cwd, key, workflowSession{
		Workflow:  workflow,
		StepIndex: stepIdx,
		Status:    status,
		StartedAt: startedAt,
		UpdatedAt: now,
	})
	broadcastWorkflowStatus(key, status)
}

// clear drops the in-memory entry without touching disk. Used when
// the user backs out of a freshly-spawned tab before any step ran —
// e.g. closing the tab while it's still showing the picker.
func (h *workflowTrackerHandle) clear(key string) {
	h.mu.Lock()
	_, had := h.entries[key]
	delete(h.entries, key)
	h.mu.Unlock()
	if had {
		broadcastWorkflowStatus(key, "")
	}
}

// activeTabFor returns (tabID, true) when `key` has an in-memory
// working entry. Used by the `f` press path to focus an existing
// workflow tab instead of spawning a duplicate. (0, false) when no
// live workflow is running for the issue.
func (h *workflowTrackerHandle) activeTabFor(key string) (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[key]
	if !ok || e.Status != workflowStatusWorking {
		return 0, false
	}
	return e.TabID, true
}

// activeWorkflowNames returns the set of workflow names that have at
// least one running session. The builder screen uses this to gate
// destructive edits: rename/delete on an in-flight workflow is
// blocked until the run finishes.
func (h *workflowTrackerHandle) activeWorkflowNames() map[string]struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]struct{})
	for _, e := range h.entries {
		if e.Status == workflowStatusWorking && e.Workflow != "" {
			out[e.Workflow] = struct{}{}
		}
	}
	return out
}

// upsertDiskSession persists `sess` under projectConfig.Workflows.Sessions
// for cwd. Errors land in debugLog only — losing a status record is
// preferable to crashing the runtime. Held under configFileMu so a
// concurrent /workflows builder commit or MCP workflow_edit call can't
// race the load → mutate → save chain and lose either side's update.
func (h *workflowTrackerHandle) upsertDiskSession(cwd, key string, sess workflowSession) {
	err := withConfigLock(func() error {
		cfg, err := loadConfig()
		if err != nil {
			return fmt.Errorf("load: %w", err)
		}
		pc := loadProjectConfig(cfg, cwd)
		if pc.Workflows.Sessions == nil {
			pc.Workflows.Sessions = make(map[string]workflowSession)
		}
		pc.Workflows.Sessions[key] = sess
		cfg = upsertProjectConfig(cfg, cwd, pc)
		return saveConfig(cfg)
	})
	if err != nil {
		debugLog("workflowTracker upsert: %v", err)
	}
}

// deleteDiskSession removes any persisted record for key. Called from
// markWorking so a fresh run doesn't render the icon for a stale
// terminal state from a previous attempt. Held under configFileMu for
// the same reason as upsertDiskSession.
func (h *workflowTrackerHandle) deleteDiskSession(cwd, key string) {
	err := withConfigLock(func() error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pc := loadProjectConfig(cfg, cwd)
		if _, ok := pc.Workflows.Sessions[key]; !ok {
			return nil
		}
		delete(pc.Workflows.Sessions, key)
		if len(pc.Workflows.Sessions) == 0 {
			pc.Workflows.Sessions = nil
		}
		cfg = upsertProjectConfig(cfg, cwd, pc)
		return saveConfig(cfg)
	})
	if err != nil {
		debugLog("workflowTracker delete: %v", err)
	}
}

// broadcastWorkflowStatus delivers a workflowStatusChangedMsg to the
// live tea.Program so every tab's Update sees the change. No-op when
// the program isn't registered (early startup, tests without a
// program).
//
// The Send is deferred onto a goroutine because every caller in this
// package fires from inside Update (markWorking from openWorkflowTab,
// markFinal from advanceWorkflowStep / closeTab, …). Calling
// tea.Program.Send synchronously from inside Update can stall the
// main loop if the program's input channel doesn't have headroom —
// the writer (Update) waits on the reader, who is currently busy
// running Update. mcpBridge.broadcast is the only other Send caller
// in this codebase and it always runs on a fresh HTTP-handler
// goroutine, which is implicitly safe; we mirror that here.
func broadcastWorkflowStatus(key, status string) {
	p := teaProgramPtr.Load()
	if p == nil {
		return
	}
	msg := workflowStatusChangedMsg{issueKey: key, status: status}
	go p.Send(msg)
}

// workflowDefByName looks up the named workflow under cwd's project
// entry. Returns false when no match. Shared between the picker and
// the runtime so the two can't drift on naming rules.
func workflowDefByName(cwd, name string) (workflowDef, bool) {
	cfg, err := loadConfig()
	if err != nil {
		return workflowDef{}, false
	}
	pc := loadProjectConfig(cfg, cwd)
	for _, w := range pc.Workflows.Items {
		if w.Name == name {
			return w, true
		}
	}
	return workflowDef{}, false
}

// workflowKeyPrefix returns ("<provider>:<owner/repo>#", true) when
// s has a configured issue provider that can resolve the project
// scope. Used by the kanban / detail renderers to avoid calling
// IssueProvider.IssueRef per card. ok=false on unresolved providers
// so the caller silently skips the status-icon path.
func workflowKeyPrefix(s *issuesState) (string, bool) {
	if s == nil || s.provider == nil {
		return "", false
	}
	r, err := s.provider.IssueRef(s.projectCfg, s.cwd, issue{})
	if err != nil {
		return "", false
	}
	return r.Provider + ":" + r.Project + "#", true
}

// workflowStatusForIssue is the row-render-time lookup. Returns the
// glyph string ("" / ▸ / ✓ / ✗) for the issue under cwd, using the
// pre-computed keyPrefix to avoid resolving owner/repo per row.
// hasPrefix=false short-circuits to "" so the renderer doesn't have
// to gate on it at every call site.
func workflowStatusForIssue(s *issuesState, prefix string, hasPrefix bool, n int) string {
	if !hasPrefix {
		return ""
	}
	key := prefix + itoaInt(n)
	e, ok := workflowTracker().lookup(s.cwd, key)
	if !ok {
		return ""
	}
	return workflowStatusGlyph(e.Status)
}

// itoaInt is a tiny inline strconv.Itoa used by workflowStatusForIssue
// and the workflow picker; both files want to stay light on imports.
// (strconv is already imported elsewhere; this just avoids reaching
// for it from workflows.go where it isn't.)
func itoaInt(n int) string {
	return itoa(n)
}

// formatIssueCard renders one card row: optional workflow status
// glyph, "#<number>", title, truncated to width. Pulled out of the
// kanban render so the same shape can serve future per-issue
// surfaces (per-assignee swimlanes, milestone grids).
func formatIssueCard(it issue, keyPrefix string, hasKeyPrefix bool, s *issuesState, width int) string {
	glyph := workflowStatusForIssue(s, keyPrefix, hasKeyPrefix, it.number)
	var card string
	if glyph != "" {
		card = fmt.Sprintf("%s #%d  %s", glyph, it.number, it.title)
	} else {
		card = fmt.Sprintf("#%d  %s", it.number, it.title)
	}
	return xansi.Truncate(card, width, "…")
}

// workflowStatusGlyph returns the single-cell status glyph the
// kanban card / detail view prepends to the issue number for a
// given workflow status. Empty status returns "" so the card
// renders flush against the issue number for issues with no
// workflow history. Styling is applied at the call site so the
// glyph can pick up the surrounding row's foreground when needed.
func workflowStatusGlyph(status string) string {
	switch status {
	case workflowStatusWorking:
		return promptStyle.Render("▸")
	case workflowStatusDone:
		return promptStyle.Render("✓")
	case workflowStatusFailed:
		return errStyle.Render("✗")
	}
	return ""
}

// projectWorkflows returns the list of workflows defined under cwd's
// project entry, in the order they appear on disk. The picker / `f`
// dispatcher consume this; an empty slice means the user has no
// pipelines configured.
func projectWorkflows(cwd string) []workflowDef {
	cfg, err := loadConfig()
	if err != nil {
		return nil
	}
	pc := loadProjectConfig(cfg, cwd)
	return pc.Workflows.Items
}
