package watcher

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const (
	stateFileName = "watchers.state.json"
	stateLockName = "watchers.state.lock"
	stateVersion  = 1

	// MaxRuns is how many runs each watcher keeps in the shared state.
	MaxRuns = 20
	// MaxStoredOutput caps the script output a run keeps for display; the
	// model gets the whole output, up to MaxOutputBytes.
	MaxStoredOutput = 16 * 1024
	// MaxStoredResponse caps the model's final answer a run keeps.
	MaxStoredResponse = 8 * 1024
	// MaxStoredStderr caps the tail of stderr a run keeps.
	MaxStoredStderr = 4 * 1024
)

// Phase is what a watcher is doing right now.
type Phase string

const (
	PhaseIdle     Phase = ""
	PhaseScript   Phase = "script"
	PhaseModel    Phase = "model"
	PhaseApproval Phase = "approval"
)

// RunStatus is where a run stands.
type RunStatus string

const (
	RunRunning RunStatus = "running"
	RunDone    RunStatus = "done"
	RunFailed  RunStatus = "failed"
	RunStopped RunStatus = "stopped"
)

// Run is one check that did something: its script failed, or it printed
// output that went to the model. Checks that printed nothing are not runs;
// they only move Status.LastCheck.
type Run struct {
	ID          string    `json:"id"`
	StartedAt   time.Time `json:"startedAt"`
	EndedAt     time.Time `json:"endedAt,omitempty"`
	Status      RunStatus `json:"status"`
	Error       string    `json:"error,omitempty"`
	ExitCode    int       `json:"exitCode"`
	Output      string    `json:"output,omitempty"`
	OutputBytes int       `json:"outputBytes,omitempty"`
	Stderr      string    `json:"stderr,omitempty"`
	Target      string    `json:"target,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model,omitempty"`
	// SessionID and SessionCwd locate the model run's session on disk, so
	// any window can show its transcript and continue it as a chat.
	SessionID  string  `json:"sessionID,omitempty"`
	SessionCwd string  `json:"sessionCwd,omitempty"`
	Response   string  `json:"response,omitempty"`
	CostUSD    float64 `json:"costUSD,omitempty"`
	CostKnown  bool    `json:"costKnown,omitempty"`
}

// Finished reports whether the run has ended.
func (r Run) Finished() bool { return r.Status != RunRunning }

// Status is one watcher's live state and recent runs.
type Status struct {
	Phase    Phase  `json:"phase,omitempty"`
	Activity string `json:"activity,omitempty"`
	// Approval names the tool a running model is waiting on the user to
	// approve.
	Approval string `json:"approval,omitempty"`
	// LastCheck is when the script last started; LastEnded when the last
	// check (and its model run) ended. The schedule counts from LastEnded.
	LastCheck time.Time `json:"lastCheck,omitempty"`
	LastEnded time.Time `json:"lastEnded,omitempty"`
	// LastNote describes the last check when it produced no run ("no
	// output"); empty when it did.
	LastNote string    `json:"lastNote,omitempty"`
	NextRun  time.Time `json:"nextRun,omitempty"`
	Runs     []Run     `json:"runs,omitempty"`
	// RunRequested and StopRequested are requests from any window to the
	// one that runs watchers: run now, and stop the run with that id.
	RunRequested  time.Time `json:"runRequested,omitempty"`
	StopRequested string    `json:"stopRequested,omitempty"`
	// SeenRun is the newest run the user has looked at.
	SeenRun string `json:"seenRun,omitempty"`
}

// LatestRun is the newest run, if any.
func (st *Status) LatestRun() (Run, bool) {
	if st == nil || len(st.Runs) == 0 {
		return Run{}, false
	}
	return st.Runs[0], true
}

// Unseen reports whether the newest run finished and the user has not
// looked at it yet.
func (st *Status) Unseen() bool {
	r, ok := st.LatestRun()
	return ok && r.Finished() && r.ID != st.SeenRun
}

// AddRun records a new run, newest first, keeping MaxRuns.
func (st *Status) AddRun(r Run) {
	st.Runs = append([]Run{r}, st.Runs...)
	if len(st.Runs) > MaxRuns {
		st.Runs = st.Runs[:MaxRuns]
	}
}

// UpdateRun applies fn to the run with id; false when it is gone.
func (st *Status) UpdateRun(id string, fn func(*Run)) bool {
	for i := range st.Runs {
		if st.Runs[i].ID == id {
			fn(&st.Runs[i])
			return true
		}
	}
	return false
}

// Leader identifies the ask process that runs watchers.
type Leader struct {
	PID   int       `json:"pid,omitempty"`
	Since time.Time `json:"since,omitempty"`
}

// State is shared by every ask window: the one that runs watchers writes
// it, the rest read it to show the same status, and any window may file a
// request (run now, stop, seen).
type State struct {
	Version  int                `json:"version"`
	Leader   Leader             `json:"leader"`
	Watchers map[string]*Status `json:"watchers,omitempty"`
}

// Watcher returns the named watcher's status, creating it.
func (s *State) Watcher(name string) *Status {
	if s.Watchers == nil {
		s.Watchers = map[string]*Status{}
	}
	st, ok := s.Watchers[name]
	if !ok {
		st = &Status{}
		s.Watchers[name] = st
	}
	return st
}

// Clone deep-copies the state so a reader can hold it without a lock.
func (s State) Clone() State {
	out := State{Version: s.Version, Leader: s.Leader}
	if s.Watchers != nil {
		out.Watchers = make(map[string]*Status, len(s.Watchers))
		for name, st := range s.Watchers {
			cp := *st
			cp.Runs = append([]Run(nil), st.Runs...)
			out.Watchers[name] = &cp
		}
	}
	return out
}

// StatePath is ~/.config/ask/watchers.state.json.
func StatePath() string {
	base := configDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, stateFileName)
}

// LoadState reads the shared state; a missing file is an empty state.
func LoadState() (State, error) {
	path := StatePath()
	if path == "" {
		return State{Version: stateVersion}, errors.New("no home directory for watcher state")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: stateVersion}, nil
	}
	if err != nil {
		return State{Version: stateVersion}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{Version: stateVersion}, err
	}
	s.Version = stateVersion
	return s, nil
}

// MutateState runs fn on the shared state under a cross-process lock and
// writes the result.
func MutateState(fn func(*State) error) error {
	base := configDir()
	if base == "" {
		return errors.New("no home directory for watcher state")
	}
	return withFileLock(filepath.Join(base, stateLockName), func() error {
		s, err := LoadState()
		if err != nil {
			s = State{Version: stateVersion}
		}
		if err := fn(&s); err != nil {
			return err
		}
		data, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		return writeFileAtomic(StatePath(), append(data, '\n'), 0o600)
	})
}

// StateStamp identifies a version of the state file on disk, so a reader
// re-reads it only when it changed.
type StateStamp struct {
	ModTime time.Time
	Size    int64
}

// CurrentStateStamp stats the state file; ok is false when it is missing.
func CurrentStateStamp() (StateStamp, bool) {
	fi, err := os.Stat(StatePath())
	if err != nil {
		return StateStamp{}, false
	}
	return StateStamp{ModTime: fi.ModTime(), Size: fi.Size()}, true
}

// Clip keeps at most max bytes of s, cut at a rune boundary, marking the cut.
func Clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !runeStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… [truncated]"
}

// ClipTail keeps at most the last max bytes of s.
func ClipTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && !runeStart(s[cut]) {
		cut++
	}
	return "[truncated] …\n" + s[cut:]
}

func runeStart(b byte) bool { return b&0xC0 != 0x80 }
