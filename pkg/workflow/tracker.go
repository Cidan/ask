package workflow

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/config"
)

// Workflow status constants.
const (
	StatusWorking = "working"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// TrackerEntry is one in-memory or persisted runtime status entry for an issue or target.
type TrackerEntry struct {
	Status    string    `json:"status"`
	TabID     int       `json:"tab_id,omitempty"`
	Workflow  string    `json:"workflow"`
	StepIndex int       `json:"step_index"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StatusListener receives notifications whenever a workflow status changes.
type StatusListener func(key string, status string)

// Tracker manages workflow runtime statuses across tabs and projects.
type Tracker struct {
	mu       sync.Mutex
	entries  map[string]TrackerEntry
	listener StatusListener
}

// DefaultTracker is the package-wide tracker instance.
var DefaultTracker = NewTracker()

// GlobalTracker returns the package-wide default tracker singleton.
func GlobalTracker() *Tracker {
	return DefaultTracker
}

// NewTracker instantiates a new thread-safe workflow tracker.
func NewTracker() *Tracker {
	return &Tracker{
		entries: make(map[string]TrackerEntry),
	}
}

// SetListener attaches a listener function called on every status change.
func (t *Tracker) SetListener(l StatusListener) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.listener = l
}

// ResetForTest clears the in-memory tracker entries for test isolation.
func (t *Tracker) ResetForTest() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[string]TrackerEntry)
}

// Lookup returns the runtime entry for key, hydrating from disk if missing in memory.
func (t *Tracker) Lookup(cwd, key string) (TrackerEntry, bool) {
	t.mu.Lock()
	if e, ok := t.entries[key]; ok {
		t.mu.Unlock()
		return e, true
	}
	t.mu.Unlock()

	pc, err := config.LoadProject(cwd)
	if err != nil {
		return TrackerEntry{}, false
	}
	sess, ok := pc.Workflows.Sessions[key]
	if !ok {
		return TrackerEntry{}, false
	}
	e := TrackerEntry{
		Status:    sess.Status,
		Workflow:  sess.Workflow,
		StepIndex: sess.StepIndex,
		StartedAt: sess.StartedAt,
		UpdatedAt: sess.UpdatedAt,
	}
	t.mu.Lock()
	t.entries[key] = e
	t.mu.Unlock()
	return e, true
}

// MarkWorking marks a workflow run as actively running in tab tabID.
func (t *Tracker) MarkWorking(cwd, key, workflow string, tabID int) {
	now := time.Now().UTC()
	t.mu.Lock()
	t.entries[key] = TrackerEntry{
		Status:    StatusWorking,
		TabID:     tabID,
		Workflow:  workflow,
		StepIndex: 0,
		StartedAt: now,
		UpdatedAt: now,
	}
	listener := t.listener
	t.mu.Unlock()

	t.deleteDiskSession(cwd, key)
	if listener != nil {
		listener(key, StatusWorking)
	}
}

// MarkStep advances the in-memory step index without altering status.
func (t *Tracker) MarkStep(key string, stepIdx int) {
	t.mu.Lock()
	e, ok := t.entries[key]
	if !ok {
		t.mu.Unlock()
		return
	}
	e.StepIndex = stepIdx
	e.UpdatedAt = time.Now().UTC()
	t.entries[key] = e
	status := e.Status
	listener := t.listener
	t.mu.Unlock()

	if listener != nil {
		listener(key, status)
	}
}

// MarkFinal records and persists a terminal status (StatusDone or StatusFailed).
func (t *Tracker) MarkFinal(cwd, key, workflow, status string, stepIdx int) {
	if status != StatusDone && status != StatusFailed {
		return
	}
	now := time.Now().UTC()
	t.mu.Lock()
	startedAt := now
	if prev, ok := t.entries[key]; ok && !prev.StartedAt.IsZero() {
		startedAt = prev.StartedAt
	}
	t.entries[key] = TrackerEntry{
		Status:    status,
		Workflow:  workflow,
		StepIndex: stepIdx,
		StartedAt: startedAt,
		UpdatedAt: now,
	}
	listener := t.listener
	t.mu.Unlock()

	t.upsertDiskSession(cwd, key, config.WorkflowSession{
		Workflow:  workflow,
		StepIndex: stepIdx,
		Status:    status,
		StartedAt: startedAt,
		UpdatedAt: now,
	})
	if listener != nil {
		listener(key, status)
	}
}

// Clear drops the in-memory entry for key without modifying disk.
func (t *Tracker) Clear(key string) {
	t.mu.Lock()
	_, had := t.entries[key]
	delete(t.entries, key)
	listener := t.listener
	t.mu.Unlock()

	if had && listener != nil {
		listener(key, "")
	}
}

// ActiveTabFor returns (tabID, true) if key currently has an active in-memory working entry.
func (t *Tracker) ActiveTabFor(key string) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	if !ok || e.Status != StatusWorking {
		return 0, false
	}
	return e.TabID, true
}

// ActiveWorkflowNames returns the set of workflow names that currently have running sessions.
func (t *Tracker) ActiveWorkflowNames() map[string]struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]struct{})
	for _, e := range t.entries {
		if e.Status == StatusWorking && e.Workflow != "" {
			out[e.Workflow] = struct{}{}
		}
	}
	return out
}

func (t *Tracker) upsertDiskSession(cwd, key string, sess config.WorkflowSession) {
	if cwd == "" {
		return
	}
	_ = config.WithConfigLock(func() error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if cfg.Projects == nil {
			cfg.Projects = make(map[string]config.ProjectConfig)
		}
		canonical, err := filepath.Abs(cwd)
		if err != nil {
			canonical = cwd
		}
		pc := cfg.Projects[canonical]
		if pc.Workflows.Sessions == nil {
			pc.Workflows.Sessions = make(map[string]config.WorkflowSession)
		}
		pc.Workflows.Sessions[key] = sess
		cfg.Projects[canonical] = pc
		return config.Save(cfg)
	})
}

func (t *Tracker) deleteDiskSession(cwd, key string) {
	if cwd == "" {
		return
	}
	_ = config.WithConfigLock(func() error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if cfg.Projects == nil {
			return nil
		}
		canonical, err := filepath.Abs(cwd)
		if err != nil {
			canonical = cwd
		}
		pc, ok := cfg.Projects[canonical]
		if !ok || pc.Workflows.Sessions == nil {
			return nil
		}
		if _, exists := pc.Workflows.Sessions[key]; !exists {
			return nil
		}
		delete(pc.Workflows.Sessions, key)
		if len(pc.Workflows.Sessions) == 0 {
			pc.Workflows.Sessions = nil
		}
		cfg.Projects[canonical] = pc
		return config.Save(cfg)
	})
}
