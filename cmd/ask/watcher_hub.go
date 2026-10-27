package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
	"github.com/Cidan/ask/pkg/watcher"
	"github.com/Cidan/ask/pkg/workflow"
)

// watcherTickInterval is how often every window polls: the leader to start
// what is due and pick up requests, the rest to refresh what they show and
// to take the lead when it frees up.
const watcherTickInterval = time.Second

// watcherTickMsg drives the poll loop; watchersChangedMsg asks for a
// redraw after the hub's state moved.
type (
	watcherTickMsg     struct{}
	watchersChangedMsg struct{}
)

// watcherApprovalClosedMsg tells the tabs that approval requests from a
// watcher run are gone (answered elsewhere, or the run ended), so a modal
// still showing one closes.
type watcherApprovalClosedMsg struct {
	relays []chan approvalReply
}

// watcherHub runs watchers in the one ask process that holds the lead, and
// mirrors the shared state for every window to draw. A watcher's model run
// is a background agent session on a synthetic negative tab id that no tab
// owns: its messages stay on its own channel, and its approval requests
// park here until the user answers them from the /watchers screen.
type watcherHub struct {
	mu        sync.Mutex
	tickMu    sync.Mutex
	lead      *watcher.Lead
	suspended bool
	closed    bool
	defs      []watcher.Def
	state     watcher.State
	stamp     watcher.StateStamp
	live      map[string]*watcherLive
	nextTab   int
	shell     string
	// wg counts the check goroutines, so giving up the lead waits for the
	// stopped ones to record how they ended; relays counts the goroutines
	// carrying approval answers, which close waits for too.
	wg     sync.WaitGroup
	relays sync.WaitGroup
}

// resignWait bounds how long giving up the lead waits for stopped checks.
const resignWait = 1500 * time.Millisecond

// watcherLive is one check in flight in this process.
type watcherLive struct {
	// id is the watcher's key (watcher.Def.ID); name is for messages.
	id       string
	name     string
	tabID    int
	cancel   context.CancelFunc
	runID    string
	activity string
	sess     *agentSession
	// approvals queue the run's approval requests, oldest first.
	approvals []*watcherApproval
}

type watcherApproval struct {
	tool   string
	input  map[string]any
	reply  chan approvalReply
	relays []chan approvalReply
	done   chan struct{}
	once   sync.Once
}

func (ap *watcherApproval) close() { ap.once.Do(func() { close(ap.done) }) }

var globalWatcherHub = newWatcherHub()

func newWatcherHub() *watcherHub {
	return &watcherHub{
		live:    map[string]*watcherLive{},
		nextTab: -1000,
		shell:   tools.ResolveAgentShell(exec.LookPath, os.Getenv),
	}
}

func watcherTickCmd() tea.Cmd {
	return tea.Tick(watcherTickInterval, func(time.Time) tea.Msg { return watcherTickMsg{} })
}

// watcherPollCmd runs one hub tick off the UI goroutine.
func watcherPollCmd(h *watcherHub) tea.Cmd {
	return func() tea.Msg {
		h.tick(time.Now())
		return watchersChangedMsg{}
	}
}

func (h *watcherHub) notify() {
	postToProgram(watchersChangedMsg{})
}

// postToProgram hands msg to the program off the caller's goroutine: the hub is
// called from inside Update too (an approval parking, a key on /watchers),
// and a synchronous Program.Send from there never returns.
func postToProgram(msg tea.Msg) {
	go agentSendToProgram(msg)
}

// tick takes the lead when it is free, then either starts what is due (as
// leader) or refreshes the shared state (as follower). One tick at a time.
func (h *watcherHub) tick(now time.Time) {
	if !h.tickMu.TryLock() {
		return
	}
	defer h.tickMu.Unlock()
	h.mu.Lock()
	idle := h.suspended || h.closed
	leading := h.lead != nil
	h.mu.Unlock()
	if idle {
		return
	}
	defs := watcher.List()
	h.mu.Lock()
	h.defs = defs
	h.mu.Unlock()
	if !leading {
		lead, err := watcher.TryLead()
		if err != nil {
			debugLog("watcher lead: %v", err)
		}
		if lead != nil {
			h.becomeLeader(lead, now)
			leading = true
		}
	}
	if leading {
		h.leadTick(defs, now)
		return
	}
	h.followTick()
}

// becomeLeader records this process as the one running watchers. Whatever
// a previous leader left mid-flight died with it: its phases reset, and its
// unfinished runs are marked stopped.
func (h *watcherHub) becomeLeader(lead *watcher.Lead, now time.Time) {
	h.mu.Lock()
	h.lead = lead
	h.mu.Unlock()
	debugLog("watchers: took the lead (pid %d)", os.Getpid())
	h.update(func(s *watcher.State) bool {
		s.Leader = watcher.Leader{PID: os.Getpid(), Since: now.UTC()}
		for _, st := range s.Watchers {
			st.Phase, st.Activity, st.Approval = watcher.PhaseIdle, "", ""
			for i := range st.Runs {
				if !st.Runs[i].Finished() {
					st.Runs[i].Status = watcher.RunStopped
					st.Runs[i].Error = "the ask window running it closed"
					st.Runs[i].EndedAt = now.UTC()
				}
			}
		}
		return true
	})
}

// leadTick prunes state for deleted watchers, honors stop and run-now
// requests, refreshes each watcher's next run, and starts what is due.
func (h *watcherHub) leadTick(defs []watcher.Def, now time.Time) {
	var due []watcher.Def
	var stops []string
	h.update(func(s *watcher.State) bool {
		changed := false
		known := map[string]bool{}
		for _, d := range defs {
			known[d.ID()] = true
		}
		for id := range s.Watchers {
			if !known[id] {
				delete(s.Watchers, id)
				changed = true
			}
		}
		for _, d := range defs {
			st := s.Watcher(d.ID())
			if h.isLive(d.ID()) {
				if st.StopRequested != "" {
					stops = append(stops, d.ID())
					st.StopRequested = ""
					changed = true
				}
				continue
			}
			if st.StopRequested != "" {
				st.StopRequested = ""
				changed = true
			}
			var next time.Time
			if !d.Paused {
				if sch, err := watcher.ParseSchedule(d.Schedule); err == nil {
					next = sch.Next(st.LastEnded, d.CreatedAt)
				}
			}
			if !next.Equal(st.NextRun) {
				st.NextRun = next
				changed = true
			}
			if d.Paused || next.IsZero() {
				if !st.RunRequested.IsZero() {
					st.RunRequested = time.Time{}
					changed = true
				}
				continue
			}
			if !next.After(now) || !st.RunRequested.IsZero() {
				due = append(due, d)
				st.RunRequested = time.Time{}
				st.Phase = watcher.PhaseScript
				changed = true
			}
		}
		return changed
	})
	for _, id := range stops {
		h.stopLive(id)
	}
	for _, d := range due {
		h.start(d)
	}
}

// followTick re-reads the shared state when another window changed it.
func (h *watcherHub) followTick() {
	stamp, ok := watcher.CurrentStateStamp()
	h.mu.Lock()
	same := ok && stamp == h.stamp
	h.mu.Unlock()
	if same {
		return
	}
	s, err := watcher.LoadState()
	if err != nil {
		debugLog("watcher state: %v", err)
		return
	}
	h.mu.Lock()
	h.state = s
	h.stamp = stamp
	h.mu.Unlock()
}

// update applies fn to the shared state and, when it reports a change,
// writes it and refreshes this window's copy.
func (h *watcherHub) update(fn func(*watcher.State) bool) {
	var snapshot watcher.State
	changed := false
	err := watcher.MutateState(func(s *watcher.State) error {
		changed = fn(s)
		snapshot = s.Clone()
		if !changed {
			return errUnchanged
		}
		return nil
	})
	if err != nil && !errors.Is(err, errUnchanged) {
		debugLog("watcher state: %v", err)
		return
	}
	stamp, _ := watcher.CurrentStateStamp()
	h.mu.Lock()
	h.state = snapshot
	h.stamp = stamp
	h.mu.Unlock()
}

var errUnchanged = errors.New("unchanged")

// updateWatcher applies fn to one watcher's status and redraws. A check
// that outlives this window's lead (Ctrl+Z, exit) writes nothing: the next
// leader settles what it left behind.
func (h *watcherHub) updateWatcher(id string, fn func(*watcher.Status)) {
	h.mu.Lock()
	leading := h.lead != nil
	h.mu.Unlock()
	if !leading {
		return
	}
	h.update(func(s *watcher.State) bool {
		fn(s.Watcher(id))
		return true
	})
	h.notify()
}

func (h *watcherHub) isLive(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.live[id] != nil
}

// start launches one check of d in the background.
func (h *watcherHub) start(d watcher.Def) {
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	if h.live[d.ID()] != nil || h.lead == nil {
		h.mu.Unlock()
		cancel()
		return
	}
	live := &watcherLive{id: d.ID(), name: d.Name, tabID: h.nextTab, cancel: cancel}
	h.nextTab--
	h.live[d.ID()] = live
	h.wg.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.wg.Done()
		h.runCheck(ctx, d, live)
	}()
}

// runCheck runs d's script and, when it printed something, hands the
// output to the model. The schedule counts from when all of it ends.
func (h *watcherHub) runCheck(ctx context.Context, d watcher.Def, live *watcherLive) {
	defer live.cancel()
	started := time.Now().UTC()
	var lastCheck time.Time
	h.updateWatcher(d.ID(), func(st *watcher.Status) {
		lastCheck = st.LastCheck
		st.Phase, st.Activity, st.Approval = watcher.PhaseScript, "", ""
		st.LastCheck = started
	})
	res := watcher.RunScript(ctx, watcher.SpecFor(d, h.shell, lastCheck))
	debugLog("watcher %s: script exit=%d bytes=%d failed=%v", d.Name, res.ExitCode, res.StdoutBytes, res.Failed())

	switch {
	case res.Failed():
		status := watcher.RunFailed
		if res.Cancelled {
			status = watcher.RunStopped
		}
		h.finish(d, live, func(st *watcher.Status) {
			st.LastNote = ""
			st.AddRun(watcher.Run{
				ID:          newWatcherRunID(),
				StartedAt:   started,
				EndedAt:     time.Now().UTC(),
				Status:      status,
				Error:       res.FailureText(),
				ExitCode:    res.ExitCode,
				Output:      watcher.Clip(res.Stdout, watcher.MaxStoredOutput),
				OutputBytes: res.StdoutBytes,
				Stderr:      watcher.ClipTail(res.Stderr, watcher.MaxStoredStderr),
			})
		})
		return
	case strings.TrimSpace(res.Stdout) == "":
		h.finish(d, live, func(st *watcher.Status) { st.LastNote = "no output" })
		return
	}

	run := watcher.Run{
		ID:          newWatcherRunID(),
		StartedAt:   started,
		Status:      watcher.RunRunning,
		Output:      watcher.Clip(res.Stdout, watcher.MaxStoredOutput),
		OutputBytes: res.StdoutBytes,
		Stderr:      watcher.ClipTail(res.Stderr, watcher.MaxStoredStderr),
		Target:      d.TargetLabel(),
	}
	h.mu.Lock()
	live.runID = run.ID
	h.mu.Unlock()
	h.updateWatcher(d.ID(), func(st *watcher.Status) {
		st.LastNote = ""
		st.Phase = watcher.PhaseModel
		st.AddRun(run)
	})

	out := watcherModelRun(ctx, h, d, live, watcher.Prompt(d, res, started))
	h.finish(d, live, func(st *watcher.Status) {
		st.UpdateRun(run.ID, func(r *watcher.Run) {
			r.EndedAt = time.Now().UTC()
			r.Provider, r.Model = out.provider, out.model
			r.SessionID, r.SessionCwd = out.sessionID, out.sessionCwd
			r.Response = watcher.Clip(out.response, watcher.MaxStoredResponse)
			r.CostUSD, r.CostKnown = out.costUSD, out.costKnown
			switch {
			case ctx.Err() != nil:
				r.Status = watcher.RunStopped
				r.Error = "stopped"
			case out.err != nil:
				r.Status = watcher.RunFailed
				r.Error = out.err.Error()
			default:
				r.Status = watcher.RunDone
			}
		})
	})
}

// finish ends a check: the watcher goes idle, the schedule counts from now,
// and anything still waiting on the user is withdrawn. The check leaves the
// live set last, so whoever sees it gone sees its outcome.
func (h *watcherHub) finish(d watcher.Def, live *watcherLive, fn func(*watcher.Status)) {
	h.mu.Lock()
	pending := live.approvals
	live.approvals = nil
	h.mu.Unlock()
	h.withdraw(pending)
	ended := time.Now().UTC()
	h.updateWatcher(d.ID(), func(st *watcher.Status) {
		fn(st)
		st.Phase, st.Activity, st.Approval = watcher.PhaseIdle, "", ""
		st.LastEnded = ended
		st.NextRun = time.Time{}
		if sch, err := watcher.ParseSchedule(d.Schedule); err == nil && !d.Paused {
			st.NextRun = sch.Next(ended, d.CreatedAt)
		}
	})
	h.mu.Lock()
	if h.live[d.ID()] == live {
		delete(h.live, d.ID())
	}
	h.mu.Unlock()
}

// withdraw answers approvals nobody will answer now and closes any modal
// still showing them.
func (h *watcherHub) withdraw(pending []*watcherApproval) {
	var relays []chan approvalReply
	for _, ap := range pending {
		ap.close()
		select {
		case ap.reply <- approvalReply{allow: false}:
		default:
		}
		relays = append(relays, ap.relays...)
	}
	if len(relays) > 0 {
		postToProgram(watcherApprovalClosedMsg{relays: relays})
	}
}

func newWatcherRunID() string { return newUUIDv4()[:8] }

// setActivity records what a running model is doing; unchanged text costs
// nothing.
func (h *watcherHub) setActivity(live *watcherLive, activity string) {
	activity = strings.TrimSpace(firstLine(activity))
	h.mu.Lock()
	same := live.activity == activity
	live.activity = activity
	h.mu.Unlock()
	if same {
		return
	}
	h.updateWatcher(live.id, func(st *watcher.Status) {
		if st.Phase == watcher.PhaseModel {
			st.Activity = activity
		}
	})
}

// requestRun asks the leader for a check now; it is a file write, so it
// works from any window.
func (h *watcherHub) requestRun(id string) error {
	err := watcher.MutateState(func(s *watcher.State) error {
		s.Watcher(id).RunRequested = time.Now().UTC()
		return nil
	})
	h.followTickAfterWrite()
	return err
}

// requestStop asks the leader to stop the watcher's running check.
func (h *watcherHub) requestStop(id string) error {
	if h.isLive(id) {
		h.stopLive(id)
		return nil
	}
	err := watcher.MutateState(func(s *watcher.State) error {
		st := s.Watcher(id)
		if r, ok := st.LatestRun(); ok && !r.Finished() {
			st.StopRequested = r.ID
		} else {
			st.StopRequested = "check"
		}
		return nil
	})
	h.followTickAfterWrite()
	return err
}

// markSeen records that the user looked at the watcher's newest run;
// opening an older one changes nothing.
func (h *watcherHub) markSeen(id, runID string) {
	h.mu.Lock()
	st := h.state.Watchers[id]
	already := st != nil && st.SeenRun == runID
	h.mu.Unlock()
	if already {
		return
	}
	_ = watcher.MutateState(func(s *watcher.State) error {
		st := s.Watcher(id)
		if latest, ok := st.LatestRun(); ok && latest.ID == runID {
			st.SeenRun = runID
		}
		return nil
	})
	h.followTickAfterWrite()
}

func (h *watcherHub) followTickAfterWrite() {
	if s, err := watcher.LoadState(); err == nil {
		stamp, _ := watcher.CurrentStateStamp()
		h.mu.Lock()
		h.state = s
		h.stamp = stamp
		h.mu.Unlock()
	}
}

func (h *watcherHub) stopLive(id string) {
	h.mu.Lock()
	live := h.live[id]
	var sess *agentSession
	tabID := 0
	if live != nil {
		sess = live.sess
		tabID = live.tabID
		live.cancel()
	}
	h.mu.Unlock()
	if sess != nil {
		sess.interruptTurn()
	}
	if tabID != 0 {
		globalCoordinator.Cancel(tabID)
	}
}

// ownsTab reports whether tabID belongs to one of this process's watcher
// runs.
func (h *watcherHub) ownsTab(tabID int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.live {
		if l.tabID == tabID {
			return true
		}
	}
	return false
}

// parkApproval holds a watcher run's approval request until the user
// answers it from the /watchers screen. False when no run owns the tab.
func (h *watcherHub) parkApproval(msg approvalRequestMsg) bool {
	h.mu.Lock()
	var live *watcherLive
	for _, l := range h.live {
		if l.tabID == msg.tabID {
			live = l
		}
	}
	if live == nil {
		h.mu.Unlock()
		return false
	}
	live.approvals = append(live.approvals, &watcherApproval{
		tool:  msg.toolName,
		input: msg.input,
		reply: msg.reply,
		done:  make(chan struct{}),
	})
	head := live.approvals[0].tool
	h.mu.Unlock()
	debugLog("watcher %s: approval parked tool=%s", live.name, msg.toolName)
	h.updateWatcher(live.id, func(st *watcher.Status) {
		st.Phase = watcher.PhaseApproval
		st.Approval = head
	})
	return true
}

// pendingApproval reports the tool the named watcher's run waits on, if
// this process holds the request.
func (h *watcherHub) pendingApproval(id string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.live[id]; l != nil && len(l.approvals) > 0 {
		return l.approvals[0].tool, true
	}
	return "", false
}

// blockedReason names a watcher run in this process that waits on the
// user, for herdr; "" when none does.
func (h *watcherHub) blockedReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.live {
		if len(l.approvals) > 0 {
			return "watcher " + l.name + ": approve " + l.approvals[0].tool
		}
	}
	return ""
}

// openApproval hands the oldest approval request of the named watcher's
// run to a modal: the modal answers on a relay channel, and the first
// answer from any modal reaches the run.
func (h *watcherHub) openApproval(id string) (approvalRequestMsg, bool) {
	h.mu.Lock()
	l := h.live[id]
	if l == nil || len(l.approvals) == 0 {
		h.mu.Unlock()
		return approvalRequestMsg{}, false
	}
	ap := l.approvals[0]
	relay := make(chan approvalReply, 1)
	ap.relays = append(ap.relays, relay)
	h.relays.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.relays.Done()
		select {
		case r := <-relay:
			h.answerApproval(id, ap, r)
		case <-ap.done:
		}
	}()
	return approvalRequestMsg{toolName: ap.tool, input: ap.input, reply: relay}, true
}

func (h *watcherHub) answerApproval(id string, ap *watcherApproval, r approvalReply) {
	h.mu.Lock()
	l := h.live[id]
	next := ""
	if l != nil {
		for i, q := range l.approvals {
			if q == ap {
				l.approvals = append(l.approvals[:i], l.approvals[i+1:]...)
				break
			}
		}
		if len(l.approvals) > 0 {
			next = l.approvals[0].tool
		}
	}
	h.mu.Unlock()
	ap.close()
	select {
	case ap.reply <- r:
	default:
	}
	postToProgram(watcherApprovalClosedMsg{relays: ap.relays})
	h.updateWatcher(id, func(st *watcher.Status) {
		if next != "" {
			st.Phase, st.Approval = watcher.PhaseApproval, next
		} else if st.Phase == watcher.PhaseApproval {
			st.Phase, st.Approval = watcher.PhaseModel, ""
		}
	})
}

// suspend gives the lead up while ask is stopped with Ctrl+Z: a stopped
// process cannot run checks, and holding the lock would stall them in
// every other window. Its checks stop rather than resume later beside the
// new leader's.
func (h *watcherHub) suspend() {
	h.mu.Lock()
	h.suspended = true
	h.mu.Unlock()
	h.resign()
}

func (h *watcherHub) resume() {
	h.mu.Lock()
	h.suspended = false
	h.mu.Unlock()
}

// close stops everything for good (ask is exiting): checks stop, approvals
// still open are withdrawn, and their relays end.
func (h *watcherHub) close() {
	h.mu.Lock()
	h.closed = true
	var pending []*watcherApproval
	for _, l := range h.live {
		pending = append(pending, l.approvals...)
		l.approvals = nil
	}
	h.mu.Unlock()
	h.resign()
	h.withdraw(pending)
	waitBounded(&h.relays, resignWait)
}

// waitBounded waits for wg, at most d.
func waitBounded(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func (h *watcherHub) resign() {
	// A tick in flight could start a check behind our back; let it land.
	h.tickMu.Lock()
	defer h.tickMu.Unlock()
	h.mu.Lock()
	lead := h.lead
	var ids []string
	for id := range h.live {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.stopLive(id)
	}
	if lead == nil {
		return
	}
	if !waitBounded(&h.wg, resignWait) {
		debugLog("watchers: resigning with checks still stopping")
	}
	h.update(func(s *watcher.State) bool {
		if s.Leader.PID != os.Getpid() {
			return false
		}
		s.Leader = watcher.Leader{}
		return true
	})
	h.mu.Lock()
	h.lead = nil
	h.mu.Unlock()
	lead.Release()
}

// watcherSnapshot is what the sidebar and the /watchers screen draw.
type watcherSnapshot struct {
	defs      []watcher.Def
	state     watcher.State
	leading   bool
	approvals map[string]bool
}

func (h *watcherHub) snapshot() watcherSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	snap := watcherSnapshot{
		defs:      append([]watcher.Def(nil), h.defs...),
		state:     h.state.Clone(),
		leading:   h.lead != nil,
		approvals: map[string]bool{},
	}
	for id, l := range h.live {
		if len(l.approvals) > 0 {
			snap.approvals[id] = true
		}
	}
	return snap
}

// refreshDefs re-reads the definitions now, for a screen that just changed
// one.
func (h *watcherHub) refreshDefs() {
	defs := watcher.List()
	h.mu.Lock()
	h.defs = defs
	h.mu.Unlock()
}

func (s watcherSnapshot) status(id string) *watcher.Status {
	if st := s.state.Watchers[id]; st != nil {
		return st
	}
	return &watcher.Status{}
}

// watcherModelOutcome is how a watcher's model run ended.
type watcherModelOutcome struct {
	provider   string
	model      string
	sessionID  string
	sessionCwd string
	response   string
	costUSD    float64
	costKnown  bool
	err        error
}

// watcherModelRun hands a check's output to the watcher's target. Swappable
// so tests can stand in for the model.
var watcherModelRun = func(ctx context.Context, h *watcherHub, d watcher.Def, live *watcherLive, prompt string) watcherModelOutcome {
	if d.Target() == watcher.TargetWorkflow {
		return h.runWorkflowTarget(ctx, d, live, prompt)
	}
	return h.runSessionTarget(ctx, d, live, prompt)
}

// runSessionTarget runs the prompt as one turn of a background session: a
// normal ask session, or the watcher's named agent. Permissions follow the
// configured Skip All Permissions. The run works in the watcher's own
// directory, never a fresh worktree: a check that prints every few minutes
// would leave a worktree and a branch behind each time.
func (h *watcherHub) runSessionTarget(ctx context.Context, d watcher.Def, live *watcherLive, prompt string) watcherModelOutcome {
	cfg, _ := loadConfig()
	providerID, modelID := d.Provider, d.Model
	var agent *subagentDef
	if d.Agent != "" {
		def, ok := engine.FindSubagent(d.Cwd, d.Agent)
		if !ok {
			return watcherModelOutcome{err: fmt.Errorf("no agent named %q is defined for %s", d.Agent, d.Cwd)}
		}
		agent = &def
		if providerID == "" && def.Provider != "" {
			providerID = def.Provider
			if modelID == "" {
				modelID = def.Model
			}
		} else if modelID == "" && def.Model != "" && (def.Provider == "" || def.Provider == providerID) {
			modelID = def.Model
		}
	}
	if providerID == "" {
		providerID = cfg.Provider
	}
	prov := providerByID(providerID)
	if prov == nil {
		return watcherModelOutcome{err: errors.New("no provider registered")}
	}
	settings := prov.LoadSettings()
	if modelID == "" && (d.Provider == "" || d.Provider == prov.ID()) {
		modelID = settings.Model
	}
	out := watcherModelOutcome{provider: prov.ID()}
	args := ProviderSessionArgs{
		Cwd:                d.Cwd,
		TabID:              live.tabID,
		Model:              modelID,
		Effort:             settings.Effort,
		SkipAllPermissions: watcherSkipPermissions(cfg),
		NewSessionID:       newUUIDv4(),
		ProjectMCP:         projectGitHubMCP(d.Cwd),
		Background:         true,
		Agent:              agent,
	}
	proc, ch, err := prov.StartSession(args)
	if err != nil {
		out.err = err
		return out
	}
	sess, ok := proc.payload.(*agentSession)
	if !ok {
		proc.kill()
		out.err = errors.New("watcher run: provider session is not an agent session")
		return out
	}
	defer func() {
		proc.kill()
		for range ch {
		}
	}()
	out.model, out.sessionID, out.sessionCwd = sess.modelID, sess.sessionID, args.Cwd
	h.mu.Lock()
	live.sess = sess
	runID := live.runID
	h.mu.Unlock()
	// Recorded now, not at the end, so the run's transcript can be read
	// while it is still going.
	h.updateWatcher(d.ID(), func(st *watcher.Status) {
		st.UpdateRun(runID, func(r *watcher.Run) {
			r.Provider, r.Model, r.SessionID, r.SessionCwd = out.provider, out.model, out.sessionID, out.sessionCwd
		})
	})
	stopWatch := context.AfterFunc(ctx, func() { sess.interruptTurn() })
	defer stopWatch()

	if err := sess.queueTurn(prompt); err != nil {
		out.err = err
		return out
	}
	// A turn can end with background sub-agents still running, or finished
	// with reports the model never read. The session closes when the run
	// ends, which would kill them, so the run waits for them and hands
	// their reports to the model as one more turn.
	var waiting []*tools.Job
	var agentsDone <-chan struct{}
	sawDone := false
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				if !sawDone {
					out.err = errors.New("the model run ended without finishing")
				}
				return out
			}
			switch m := msg.(type) {
			case streamStatusMsg:
				h.setActivity(live, m.status)
			case todoUpdatedMsg:
				for _, t := range m.todos {
					if t.Status == "in_progress" {
						h.setActivity(live, nonEmpty(t.ActiveForm, t.Content))
					}
				}
			case usageMsg:
				if m.costKnown {
					out.costUSD += m.costUSD
					out.costKnown = true
				}
			case costMsg:
				out.costUSD += m.costUSD
				out.costKnown = true
			case providerDoneMsg:
				sawDone = true
				out.response = m.res.Result
				switch {
				case m.err != nil:
					out.err = m.err
				case m.res.IsError:
					out.err = errors.New(nonEmpty(m.res.Result, "the model run failed"))
					out.response = ""
				}
			case turnCompleteMsg:
				if !sawDone {
					out.err = errors.New("the model run ended without finishing")
					return out
				}
				if out.err != nil || ctx.Err() != nil {
					return out
				}
				waiting = sess.env.Jobs.UndeliveredAgents()
				if len(waiting) == 0 {
					return out
				}
				debugLog("watcher %s: turn ended with %d background agents outstanding", d.Name, len(waiting))
				noun := "agents"
				if len(waiting) == 1 {
					noun = "agent"
				}
				h.setActivity(live, fmt.Sprintf("waiting for %d background %s", len(waiting), noun))
				agentsDone = jobsDone(ctx, waiting)
			}
		case <-agentsDone:
			agentsDone = nil
			if ctx.Err() != nil {
				return out
			}
			sawDone = false
			if err := sess.queueTurn(agentReportsTurn(waiting)); err != nil {
				out.err = err
				return out
			}
		}
	}
}

// jobsDone closes once every job has finished, or ctx is done.
func jobsDone(ctx context.Context, jobs []*tools.Job) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, j := range jobs {
			select {
			case <-j.DoneCh:
			case <-ctx.Done():
				return
			}
		}
	}()
	return done
}

// agentReportsTurn is the turn that hands finished background agents'
// reports to the model, marking each delivered.
func agentReportsTurn(jobs []*tools.Job) string {
	var b strings.Builder
	b.WriteString("The background agents you started have finished. Their reports follow; carry on with your instructions using them.\n")
	for _, j := range jobs {
		output, truncated, _, res := j.Snapshot()
		status := "finished"
		if res.ExitCode != 0 {
			status = "failed"
		}
		body := tools.TruncateMiddle(strings.TrimSpace(output))
		if truncated {
			body += "\n(the report exceeded the in-memory cap; middle portions were dropped)"
		}
		fmt.Fprintf(&b, "\n<agent_report job=%q task=%q status=%q>\n%s\n</agent_report>\n", j.ID, j.Command, status, body)
		j.MarkDelivered()
	}
	return strings.TrimRight(b.String(), "\n")
}

// runWorkflowTarget runs the watcher's workflow with the prompt as its
// source, in the background.
func (h *watcherHub) runWorkflowTarget(ctx context.Context, d watcher.Def, live *watcherLive, prompt string) watcherModelOutcome {
	wf, err := workflow.ResolveByName(d.Cwd, d.Workflow, "")
	if err != nil {
		return watcherModelOutcome{err: fmt.Errorf("workflow %q: %w", d.Workflow, err)}
	}
	cfg, _ := loadConfig()
	src := workflow.NewTextSource(live.tabID, prompt)
	src.TextLabel = "watcher " + d.Name + " output"
	listener := &watcherWorkflowListener{hub: h, live: live, steps: len(wf.Steps)}
	stopWatch := context.AfterFunc(ctx, func() { globalCoordinator.Cancel(live.tabID) })
	defer stopWatch()
	state, err := globalCoordinator.runWorkflowGraph(ctx, d.Cwd, live.tabID, wf, src, listener,
		workflowGraphOpts{background: true, skipPermissions: watcherSkipPermissions(cfg)})
	out := watcherModelOutcome{provider: "workflow", model: wf.Name, err: err, response: listener.log()}
	if state != nil && state.FinishData != nil && state.FinishData.Description != "" {
		out.response = strings.TrimSpace(out.response + "\n\n" + state.FinishData.Description)
	}
	return out
}

// watcherSkipPermissions is the configured Skip All Permissions, which a
// watcher's model run follows.
func watcherSkipPermissions(cfg askConfig) bool {
	return cfg.UI.SkipAllPermissions != nil && *cfg.UI.SkipAllPermissions
}

// watcherWorkflowListener turns a background workflow run's progress into
// the watcher's activity and a step log.
type watcherWorkflowListener struct {
	hub   *watcherHub
	live  *watcherLive
	steps int
	mu    sync.Mutex
	lines []string
}

func (l *watcherWorkflowListener) OnWorkflowStarted(int, workflow.Def, workflow.Source) {}

func (l *watcherWorkflowListener) OnWorkflowStepStarted(_ int, stepIdx int, stepName, _, _ string) {
	l.hub.setActivity(l.live, fmt.Sprintf("step %d/%d · %s", stepIdx+1, l.steps, stepName))
}

func (l *watcherWorkflowListener) OnWorkflowStepDone(_ int, stepIdx int, summary string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf("Step %d: %s", stepIdx+1, strings.TrimSpace(summary)))
}

func (l *watcherWorkflowListener) OnWorkflowDone(int, string, []string) {}

func (l *watcherWorkflowListener) OnWorkflowFailed(int, string) {}

func (l *watcherWorkflowListener) OnNote(int, string) {}

func (l *watcherWorkflowListener) log() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
