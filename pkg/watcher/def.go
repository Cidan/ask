// Package watcher defines watchers: scripts ask runs on a schedule. A run
// whose script prints nothing does nothing; a run whose script prints
// anything hands that output, with the watcher's instructions, to one model
// run — a chat session, a named agent, or a workflow.
//
// This package owns the definitions and their store, the schedule grammar,
// the state every ask window shares, the lock that picks the one window
// that runs watchers, and the script runner. Running the model is the TUI's
// job (cmd/ask).
package watcher

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Cidan/ask/pkg/config"
)

const (
	// DefaultTimeout bounds a script that sets no timeout of its own.
	DefaultTimeout = 2 * time.Minute
	// MaxTimeout is the longest a script may run before it is killed.
	MaxTimeout = 30 * time.Minute
)

// Def is one watcher. The script is either Command, run by the shell, or a
// script file stored beside the definition (HasScript), run directly.
//
// A watcher belongs to a project: the project its working directory is in
// (ProjectOf), set by Save. Names are unique within a project; a watcher
// is named by its Ref (project and name), or as one string by its ID.
type Def struct {
	Project      string    `json:"project"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	Schedule     string    `json:"schedule"`
	Command      string    `json:"command,omitempty"`
	Cwd          string    `json:"cwd"`
	TimeoutSecs  int       `json:"timeoutSeconds,omitempty"`
	Instructions string    `json:"instructions,omitempty"`
	Agent        string    `json:"agent,omitempty"`
	Workflow     string    `json:"workflow,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`
	Paused       bool      `json:"paused,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`

	// HasScript reports a script file stored with the definition. It is
	// derived from the store, never serialized.
	HasScript bool `json:"-"`
}

// TargetKind is what a watcher's output is handed to.
type TargetKind int

const (
	// TargetChat is a normal ask session running the watcher's instructions.
	TargetChat TargetKind = iota
	// TargetAgent is a named agent definition.
	TargetAgent
	// TargetWorkflow is a workflow run on the output.
	TargetWorkflow
)

// Target reports what the watcher hands its output to.
func (d Def) Target() TargetKind {
	switch {
	case d.Workflow != "":
		return TargetWorkflow
	case d.Agent != "":
		return TargetAgent
	}
	return TargetChat
}

// TargetLabel names the target for display: "chat session", "agent x",
// "workflow y".
func (d Def) TargetLabel() string {
	switch d.Target() {
	case TargetWorkflow:
		return "workflow " + d.Workflow
	case TargetAgent:
		return "agent " + d.Agent
	}
	return "chat session"
}

// Timeout is how long the script may run.
func (d Def) Timeout() time.Duration {
	if d.TimeoutSecs <= 0 {
		return DefaultTimeout
	}
	return time.Duration(d.TimeoutSecs) * time.Second
}

// RunsLabel is what the watcher runs, for display: the command, or the
// stored script.
func (d Def) RunsLabel() string {
	if d.Command != "" {
		return d.Command
	}
	return ScriptPath(d.Ref())
}

// Ref names a watcher: its project and its name in that project.
type Ref struct {
	Project string
	Name    string
}

// Ref is the watcher's reference.
func (d Def) Ref() Ref { return Ref{Project: d.Project, Name: d.Name} }

// ID is the watcher's key in the shared state and everywhere one string
// has to name it: "<project dir>/<name>".
func (r Ref) ID() string { return ProjectDir(r.Project) + "/" + r.Name }

// ID is the watcher's key (Ref.ID).
func (d Def) ID() string { return d.Ref().ID() }

// ProjectOf is the project a directory belongs to: the root of its git
// checkout — an ask worktree maps back to the checkout it was made from —
// else the directory itself.
func ProjectOf(dir string) string {
	if dir == "" {
		return ""
	}
	marker := string(filepath.Separator) + ".claude" + string(filepath.Separator) + "worktrees" + string(filepath.Separator)
	if i := strings.Index(dir, marker); i >= 0 {
		dir = dir[:i]
	}
	return config.ProjectRoot(dir)
}

// ProjectDir is the store directory a project's watchers live in: the
// project's base name, sanitized, and a hash of its path, so two projects
// with one name never share it.
func ProjectDir(project string) string {
	var b strings.Builder
	for _, r := range filepath.Base(project) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-.")
	if base == "" {
		base = "project"
	}
	sum := sha256.Sum256([]byte(project))
	return base + "-" + hex.EncodeToString(sum[:4])
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidName reports whether name can name a watcher: lowercase letters,
// digits, dashes, and underscores, starting with a letter or digit, at
// most 63 characters. Names are file names in the store.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("watcher name %q is invalid: use lowercase letters, digits, '-' and '_', starting with a letter or digit (at most 63 characters)", name)
	}
	return nil
}

// Validate checks a definition before it is stored.
func (d Def) Validate() error {
	if err := ValidName(d.Name); err != nil {
		return err
	}
	if _, err := ParseSchedule(d.Schedule); err != nil {
		return err
	}
	command := strings.TrimSpace(d.Command)
	switch {
	case command == "" && !d.HasScript:
		return errors.New("a watcher needs a command or a script")
	case command != "" && d.HasScript:
		return errors.New("a watcher runs a command or a script, not both")
	}
	if d.Cwd == "" || !filepath.IsAbs(d.Cwd) {
		return fmt.Errorf("watcher working directory must be an absolute path, got %q", d.Cwd)
	}
	if d.Project == "" || !filepath.IsAbs(d.Project) {
		return fmt.Errorf("watcher project must be an absolute path, got %q", d.Project)
	}
	if d.TimeoutSecs < 0 || time.Duration(d.TimeoutSecs)*time.Second > MaxTimeout {
		return fmt.Errorf("timeout must be between 1 and %d seconds", int(MaxTimeout/time.Second))
	}
	if d.Agent != "" && d.Workflow != "" {
		return errors.New("a watcher hands its output to an agent or a workflow, not both")
	}
	if d.Workflow != "" && (d.Provider != "" || d.Model != "") {
		return errors.New("provider and model do not apply to a workflow target: its steps pick their own")
	}
	if d.Target() == TargetChat && strings.TrimSpace(d.Instructions) == "" {
		return errors.New("a watcher that hands its output to a chat session needs instructions telling the model what to do with it")
	}
	return nil
}
