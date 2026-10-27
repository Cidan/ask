package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/watcher"
	"github.com/Cidan/ask/pkg/workflow"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	WatcherListToolDescription = `List watchers: scripts ask runs on a schedule that wake the model only when they print something. Each project has its own set; this lists the current project's unless all is set.

Returns each watcher's name, project, schedule, what it runs, where its output goes (a chat session, a named agent, or a workflow), and what it is doing now (next check, last check and its outcome, a run in progress).`

	WatcherGetToolDescription = `Get one watcher's full definition — schedule, command or stored script, working directory, instructions, target — plus its recent runs: when each ran, whether its script failed (exit code, stderr), what it printed, and the model's answer. Use it to debug a watcher that is failing or doing the wrong thing. The watcher is looked up in the current project unless project is given.`

	WatcherCreateToolDescription = `Create a watcher: a script ask runs on a schedule. When the script exits 0 and prints nothing, nothing happens. When it prints anything, the whole output goes to one model run together with the watcher's instructions, and the model decides what to do. A script that exits non-zero, or times out, is recorded as a failed check and wakes no model.

A watcher belongs to the project its cwd is in (default: the current project's root), and names are unique within a project. Its model run works in that directory exactly like a chat tab there: the project's CLAUDE.md and other instruction files, its rules, and its memory.

The script owns deciding what is worth reporting: print only what is new or needs attention, or the model runs on every check. It can keep its own state between checks in $ASK_WATCHER_STATE_DIR (a directory ask creates for it); $ASK_WATCHER_NAME is the watcher's name and $ASK_WATCHER_LAST_RUN the time of the previous check (RFC 3339, empty the first time). It runs in cwd with the user's environment.

Give it either command (run with bash -c in cwd) or script (the full file contents, starting with a #! line; ask stores it with the watcher and runs it directly).

schedule is one of:
- an interval, counted from the end of the previous check so checks never run back to back: "every 30s", "every 5m", "every 2h", "every 1d"
- an interval limited to days and hours: "every 10m weekdays 09:00-18:00", "every 1h mon,wed,fri", "every 15m 22:00-02:00"
- times of day: "daily 09:00", "weekdays 08:30,13:00", "weekends 10:00", "mon,thu 14:30", "mon-fri 9am"
- minutes past every hour: "hourly :15", "hourly :00,:30"
Times are local. A new interval watcher checks straight away; missed checks are never made up — at most one runs when ask comes back.

The output goes to one of:
- a chat session (the default): a normal ask session, told what to do by instructions (required)
- agent: a named agent from <available_agents>, running on its own instructions, tools, and model; instructions are added on top
- workflow: a workflow run with the output (and instructions, if any) as its source

Every project's watchers run in exactly one ask window at a time, whichever ask was opened first. The user sees them in the sidebar and on the /watchers screen.`

	WatcherEditToolDescription = `Edit a watcher in place. Pass only the fields to change; an omitted field is left alone. new_name renames it (its history moves with it); a cwd in another project moves it to that project. Setting command drops a stored script and setting script drops the command; setting agent clears workflow and setting workflow clears agent. Pass an empty string to clear a text field. paused stops and resumes its checks. The watcher is looked up in the current project unless project is given.`

	WatcherDeleteToolDescription = `Delete a watcher, its stored script, its state directory, and its run history. The watcher is looked up in the current project unless project is given.`

	WatcherRunToolDescription = `Run a watcher's check now instead of waiting for its schedule. The ask window that runs watchers starts it within a couple of seconds; afterwards the schedule counts from when this check ends. A watcher already running is left alone. The watcher is looked up in the current project unless project is given.`
)

type WatcherListInput struct {
	All bool `json:"all,omitempty" jsonschema:"list every project's watchers, not only the current project's"`
}

type WatcherSummaryView struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	Description string `json:"description,omitempty"`
	Schedule    string `json:"schedule"`
	Runs        string `json:"runs" jsonschema:"the command, or the path of the stored script"`
	Cwd         string `json:"cwd"`
	Target      string `json:"target" jsonschema:"what the output goes to"`
	Paused      bool   `json:"paused,omitempty"`
	Status      string `json:"status" jsonschema:"what the watcher is doing now"`
}

type WatcherListOutput struct {
	Watchers []WatcherSummaryView `json:"watchers"`
}

type WatcherGetInput struct {
	Name    string `json:"name" jsonschema:"watcher name"`
	Project string `json:"project,omitempty" jsonschema:"the watcher's project (any directory in it); default the current project"`
}

type WatcherView struct {
	Name           string `json:"name"`
	Project        string `json:"project"`
	Description    string `json:"description,omitempty"`
	Schedule       string `json:"schedule"`
	Command        string `json:"command,omitempty"`
	Script         string `json:"script,omitempty" jsonschema:"the stored script's contents"`
	Cwd            string `json:"cwd"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Instructions   string `json:"instructions,omitempty"`
	Agent          string `json:"agent,omitempty"`
	Workflow       string `json:"workflow,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Paused         bool   `json:"paused,omitempty"`
}

type WatcherRunView struct {
	ID        string  `json:"id"`
	StartedAt string  `json:"started_at"`
	EndedAt   string  `json:"ended_at,omitempty"`
	Status    string  `json:"status" jsonschema:"running, done, failed, or stopped"`
	Error     string  `json:"error,omitempty"`
	ExitCode  int     `json:"exit_code"`
	Output    string  `json:"output,omitempty" jsonschema:"what the script printed (cut when long)"`
	Stderr    string  `json:"stderr,omitempty" jsonschema:"the tail of the script's stderr"`
	Response  string  `json:"response,omitempty" jsonschema:"the model's final answer"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
}

type WatcherGetOutput struct {
	Watcher WatcherView      `json:"watcher"`
	Status  string           `json:"status"`
	Runs    []WatcherRunView `json:"runs"`
}

type WatcherCreateInput struct {
	Name           string `json:"name" jsonschema:"watcher name: lowercase letters, digits, '-' and '_'; unique within its project"`
	Description    string `json:"description,omitempty" jsonschema:"what the watcher is for, shown to the user"`
	Schedule       string `json:"schedule" jsonschema:"when to check, e.g. 'every 5m', 'every 10m weekdays 09:00-18:00', 'daily 09:00', 'hourly :15'"`
	Command        string `json:"command,omitempty" jsonschema:"shell command to run (bash -c, in cwd); give this or script"`
	Script         string `json:"script,omitempty" jsonschema:"full script contents starting with a #! line, stored with the watcher; give this or command"`
	Cwd            string `json:"cwd,omitempty" jsonschema:"absolute working directory for the script and the model run; its project is the watcher's (default: the current project's root)"`
	Instructions   string `json:"instructions,omitempty" jsonschema:"what the model should do with the output; required unless agent or workflow is set"`
	Agent          string `json:"agent,omitempty" jsonschema:"named agent to hand the output to"`
	Workflow       string `json:"workflow,omitempty" jsonschema:"workflow to run on the output"`
	Provider       string `json:"provider,omitempty" jsonschema:"provider id for the model run (default: the configured provider, or the agent's)"`
	Model          string `json:"model,omitempty" jsonschema:"model id for the model run"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"seconds the script may run before it is killed (default 120, max 1800)"`
	Paused         bool   `json:"paused,omitempty" jsonschema:"create it paused"`
}

type WatcherCreateOutput struct {
	Watcher WatcherView `json:"watcher"`
}

type WatcherEditInput struct {
	Name           string  `json:"name" jsonschema:"watcher to edit"`
	Project        string  `json:"project,omitempty" jsonschema:"the watcher's project (any directory in it); default the current project"`
	NewName        string  `json:"new_name,omitempty" jsonschema:"rename it"`
	Description    *string `json:"description,omitempty"`
	Schedule       *string `json:"schedule,omitempty"`
	Command        *string `json:"command,omitempty" jsonschema:"replaces the command (drops a stored script)"`
	Script         *string `json:"script,omitempty" jsonschema:"replaces the stored script (drops the command)"`
	Cwd            *string `json:"cwd,omitempty" jsonschema:"new working directory; one in another project moves the watcher there"`
	Instructions   *string `json:"instructions,omitempty"`
	Agent          *string `json:"agent,omitempty" jsonschema:"hand the output to this agent (clears workflow)"`
	Workflow       *string `json:"workflow,omitempty" jsonschema:"run this workflow on the output (clears agent)"`
	Provider       *string `json:"provider,omitempty"`
	Model          *string `json:"model,omitempty"`
	TimeoutSeconds *int    `json:"timeout_seconds,omitempty"`
	Paused         *bool   `json:"paused,omitempty"`
}

type WatcherEditOutput struct {
	Watcher WatcherView `json:"watcher"`
}

type WatcherNameInput struct {
	Name    string `json:"name" jsonschema:"watcher name"`
	Project string `json:"project,omitempty" jsonschema:"the watcher's project (any directory in it); default the current project"`
}

type WatcherDeleteOutput struct {
	Deleted bool `json:"deleted"`
}

type WatcherRunOutput struct {
	Requested bool `json:"requested"`
}

// WatcherTools returns the registry tools that manage watchers. Creating
// and editing one schedules code to run unattended, so both ask for
// approval.
func WatcherTools(env *ToolEnv) []Tool {
	cwd := func() string { return env.Cwd }
	approve := func(ctx context.Context, tool string, input map[string]any) *mcp.CallToolResult {
		if denied := env.ApprovalDenied(ctx, tool, input); denied != "" {
			return watcherError(denied)
		}
		return nil
	}
	return []Tool{
		NativeBridgeTool("watcher_list", WatcherListToolDescription,
			func(_ context.Context, in WatcherListInput) (*mcp.CallToolResult, WatcherListOutput, error) {
				return WatcherListCore(cwd(), in)
			}),
		NativeBridgeTool("watcher_get", WatcherGetToolDescription,
			func(_ context.Context, in WatcherGetInput) (*mcp.CallToolResult, WatcherGetOutput, error) {
				return WatcherGetCore(cwd(), in)
			}),
		NativeBridgeTool("watcher_create", WatcherCreateToolDescription,
			func(ctx context.Context, in WatcherCreateInput) (*mcp.CallToolResult, WatcherCreateOutput, error) {
				if res := approve(ctx, "watcher_create", watcherApprovalInput(in.Name, in.Schedule, in.Command, in.Script)); res != nil {
					return res, WatcherCreateOutput{}, nil
				}
				return WatcherCreateCore(cwd(), in)
			}),
		NativeBridgeTool("watcher_edit", WatcherEditToolDescription,
			func(ctx context.Context, in WatcherEditInput) (*mcp.CallToolResult, WatcherEditOutput, error) {
				if res := approve(ctx, "watcher_edit", watcherApprovalInput(in.Name, deref(in.Schedule), deref(in.Command), deref(in.Script))); res != nil {
					return res, WatcherEditOutput{}, nil
				}
				return WatcherEditCore(cwd(), in)
			}),
		NativeBridgeTool("watcher_delete", WatcherDeleteToolDescription,
			func(_ context.Context, in WatcherNameInput) (*mcp.CallToolResult, WatcherDeleteOutput, error) {
				return WatcherDeleteCore(cwd(), in)
			}),
		NativeBridgeTool("watcher_run", WatcherRunToolDescription,
			func(_ context.Context, in WatcherNameInput) (*mcp.CallToolResult, WatcherRunOutput, error) {
				return WatcherRunCore(cwd(), in)
			}),
	}
}

func watcherApprovalInput(name, schedule, command, script string) map[string]any {
	in := map[string]any{"name": name}
	if schedule != "" {
		in["schedule"] = schedule
	}
	if command != "" {
		in["command"] = command
	}
	if script != "" {
		in["script"] = script
	}
	return in
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func watcherError(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}

func watcherText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// resolveWatcher finds the watcher a tool call names: in the given
// project (any directory in it), else in the session's project, else the
// one watcher of that name anywhere.
func resolveWatcher(sessionCwd, project, name string) (watcher.Def, error) {
	name = strings.TrimSpace(name)
	if p := strings.TrimSpace(project); p != "" {
		return watcher.Get(watcher.Ref{Project: watcher.ProjectOf(p), Name: name})
	}
	return watcher.Resolve(watcher.ProjectOf(sessionCwd), name)
}

func watcherView(d watcher.Def) WatcherView {
	v := WatcherView{
		Name:           d.Name,
		Project:        d.Project,
		Description:    d.Description,
		Schedule:       d.Schedule,
		Command:        d.Command,
		Cwd:            d.Cwd,
		TimeoutSeconds: int(d.Timeout() / time.Second),
		Instructions:   d.Instructions,
		Agent:          d.Agent,
		Workflow:       d.Workflow,
		Provider:       d.Provider,
		Model:          d.Model,
		Paused:         d.Paused,
	}
	if d.HasScript {
		v.Script, _ = watcher.ReadScript(d.Ref())
	}
	return v
}

func WatcherListCore(sessionCwd string, in WatcherListInput) (*mcp.CallToolResult, WatcherListOutput, error) {
	project := watcher.ProjectOf(sessionCwd)
	state, _ := watcher.LoadState()
	out := WatcherListOutput{Watchers: []WatcherSummaryView{}}
	var shown []watcher.Def
	elsewhere := 0
	for _, d := range watcher.List() {
		if in.All || d.Project == project {
			shown = append(shown, d)
		} else {
			elsewhere++
		}
	}
	var b strings.Builder
	switch {
	case len(shown) == 0 && in.All:
		b.WriteString("No watchers are defined.")
	case len(shown) == 0:
		fmt.Fprintf(&b, "No watchers in %s.", project)
	case in.All:
		fmt.Fprintf(&b, "%d watcher(s) across all projects:\n", len(shown))
	default:
		fmt.Fprintf(&b, "%d watcher(s) in %s:\n", len(shown), project)
	}
	lastProject := ""
	for _, d := range shown {
		status := watcher.Summary(d, state.Watchers[d.ID()])
		out.Watchers = append(out.Watchers, WatcherSummaryView{
			Name:        d.Name,
			Project:     d.Project,
			Description: d.Description,
			Schedule:    d.Schedule,
			Runs:        d.RunsLabel(),
			Cwd:         d.Cwd,
			Target:      d.TargetLabel(),
			Paused:      d.Paused,
			Status:      status,
		})
		if in.All && d.Project != lastProject {
			fmt.Fprintf(&b, "\n%s:\n", d.Project)
			lastProject = d.Project
		}
		fmt.Fprintf(&b, "\n%s — %s, runs %s in %s, output to %s\n", d.Name, d.Schedule, d.RunsLabel(), d.Cwd, d.TargetLabel())
		if d.Description != "" {
			fmt.Fprintf(&b, "  %s\n", d.Description)
		}
		fmt.Fprintf(&b, "  Status: %s\n", status)
	}
	if !in.All && elsewhere > 0 {
		fmt.Fprintf(&b, "\n%d more in other projects (all: true lists them).", elsewhere)
	}
	return watcherText(strings.TrimRight(b.String(), "\n")), out, nil
}

func WatcherGetCore(sessionCwd string, in WatcherGetInput) (*mcp.CallToolResult, WatcherGetOutput, error) {
	d, err := resolveWatcher(sessionCwd, in.Project, in.Name)
	if err != nil {
		return watcherError(err.Error()), WatcherGetOutput{}, nil
	}
	state, _ := watcher.LoadState()
	st := state.Watchers[d.ID()]
	out := WatcherGetOutput{Watcher: watcherView(d), Status: watcher.Summary(d, st), Runs: []WatcherRunView{}}
	var b strings.Builder
	fmt.Fprintf(&b, "Watcher: %s\nProject: %s\n", d.Name, d.Project)
	if d.Description != "" {
		fmt.Fprintf(&b, "Description: %s\n", d.Description)
	}
	fmt.Fprintf(&b, "Schedule: %s\nWorking directory: %s\nTimeout: %ds\nOutput goes to: %s\n",
		d.Schedule, d.Cwd, out.Watcher.TimeoutSeconds, d.TargetLabel())
	if d.Provider != "" || d.Model != "" {
		fmt.Fprintf(&b, "Model: %s %s\n", d.Provider, d.Model)
	}
	if d.Paused {
		b.WriteString("Paused: yes\n")
	}
	if d.Command != "" {
		fmt.Fprintf(&b, "Command: %s\n", d.Command)
	} else {
		fmt.Fprintf(&b, "Script (%s):\n%s\n", watcher.ScriptPath(d.Ref()), out.Watcher.Script)
	}
	if d.Instructions != "" {
		fmt.Fprintf(&b, "Instructions:\n%s\n", d.Instructions)
	}
	fmt.Fprintf(&b, "Status: %s\n", out.Status)
	if st != nil {
		for i, r := range st.Runs {
			if i >= 5 {
				break
			}
			view := WatcherRunView{
				ID:        r.ID,
				StartedAt: r.StartedAt.Local().Format(time.RFC3339),
				Status:    string(r.Status),
				Error:     r.Error,
				ExitCode:  r.ExitCode,
				Output:    watcher.Clip(r.Output, 2000),
				Stderr:    r.Stderr,
				Response:  watcher.Clip(r.Response, 2000),
				CostUSD:   r.CostUSD,
			}
			if !r.EndedAt.IsZero() {
				view.EndedAt = r.EndedAt.Local().Format(time.RFC3339)
			}
			out.Runs = append(out.Runs, view)
			fmt.Fprintf(&b, "\nRun %s at %s: %s", r.ID, view.StartedAt, r.Status)
			if r.Error != "" {
				fmt.Fprintf(&b, " — %s", r.Error)
			}
			b.WriteString("\n")
			if view.Output != "" {
				fmt.Fprintf(&b, "Output:\n%s\n", view.Output)
			}
			if r.Stderr != "" {
				fmt.Fprintf(&b, "Stderr:\n%s\n", r.Stderr)
			}
			if view.Response != "" {
				fmt.Fprintf(&b, "Model answer:\n%s\n", view.Response)
			}
		}
	}
	return watcherText(strings.TrimRight(b.String(), "\n")), out, nil
}

// checkWatcherTargets verifies what a definition points at exists where it
// will run: its directory, its agent and workflow as seen from that
// directory, and its provider.
func checkWatcherTargets(d watcher.Def) error {
	if fi, err := os.Stat(d.Cwd); err != nil || !fi.IsDir() {
		return fmt.Errorf("working directory %s does not exist", d.Cwd)
	}
	if d.Agent != "" {
		if _, ok := engine.FindSubagent(d.Cwd, d.Agent); !ok {
			return fmt.Errorf("no agent named %q is defined for %s", d.Agent, d.Cwd)
		}
	}
	if d.Workflow != "" {
		if _, err := workflow.ResolveByName(d.Cwd, d.Workflow, ""); err != nil {
			return fmt.Errorf("workflow %q: %w", d.Workflow, err)
		}
	}
	if d.Provider != "" {
		if _, ok := providers.Get(d.Provider); !ok {
			return fmt.Errorf("unknown provider %q", d.Provider)
		}
	}
	return nil
}

func WatcherCreateCore(sessionCwd string, in WatcherCreateInput) (*mcp.CallToolResult, WatcherCreateOutput, error) {
	cwd := strings.TrimSpace(in.Cwd)
	switch {
	case cwd == "":
		cwd = watcher.ProjectOf(sessionCwd)
	case !filepath.IsAbs(cwd):
		cwd = filepath.Join(sessionCwd, cwd)
	}
	d := watcher.Def{
		Name:         strings.TrimSpace(in.Name),
		Description:  strings.TrimSpace(in.Description),
		Schedule:     strings.TrimSpace(in.Schedule),
		Command:      strings.TrimSpace(in.Command),
		Cwd:          filepath.Clean(cwd),
		TimeoutSecs:  in.TimeoutSeconds,
		Instructions: strings.TrimSpace(in.Instructions),
		Agent:        strings.TrimSpace(in.Agent),
		Workflow:     strings.TrimSpace(in.Workflow),
		Provider:     strings.TrimSpace(in.Provider),
		Model:        strings.TrimSpace(in.Model),
		Paused:       in.Paused,
		CreatedAt:    time.Now().UTC(),
	}
	if err := checkWatcherTargets(d); err != nil {
		return watcherError(err.Error()), WatcherCreateOutput{}, nil
	}
	var script *string
	if in.Script != "" {
		script = &in.Script
	}
	saved, err := watcher.Save(watcher.Ref{}, d, script)
	if err != nil {
		return watcherError(err.Error()), WatcherCreateOutput{}, nil
	}
	sch, _ := watcher.ParseSchedule(saved.Schedule)
	msg := fmt.Sprintf("created watcher %s in %s: %s, output to %s", saved.Name, saved.Project, sch, saved.TargetLabel())
	if saved.Paused {
		msg += " (paused)"
	}
	return watcherText(msg), WatcherCreateOutput{Watcher: watcherView(saved)}, nil
}

func WatcherEditCore(sessionCwd string, in WatcherEditInput) (*mcp.CallToolResult, WatcherEditOutput, error) {
	d, err := resolveWatcher(sessionCwd, in.Project, in.Name)
	if err != nil {
		return watcherError(err.Error()), WatcherEditOutput{}, nil
	}
	old := d.Ref()
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	if n := strings.TrimSpace(in.NewName); n != "" {
		d.Name = n
	}
	set(&d.Description, in.Description)
	set(&d.Schedule, in.Schedule)
	set(&d.Instructions, in.Instructions)
	set(&d.Provider, in.Provider)
	set(&d.Model, in.Model)
	if in.Cwd != nil {
		cwd := strings.TrimSpace(*in.Cwd)
		if !filepath.IsAbs(cwd) {
			cwd = filepath.Join(sessionCwd, cwd)
		}
		d.Cwd = filepath.Clean(cwd)
	}
	if in.TimeoutSeconds != nil {
		d.TimeoutSecs = *in.TimeoutSeconds
	}
	if in.Paused != nil {
		d.Paused = *in.Paused
	}
	var script *string
	if in.Command != nil {
		d.Command = strings.TrimSpace(*in.Command)
		if d.Command != "" {
			script = new(string)
		}
	}
	if in.Script != nil {
		script = in.Script
		if *in.Script != "" {
			d.Command = ""
		}
	}
	if in.Agent != nil {
		d.Agent = strings.TrimSpace(*in.Agent)
		if d.Agent != "" {
			d.Workflow = ""
		}
	}
	if in.Workflow != nil {
		d.Workflow = strings.TrimSpace(*in.Workflow)
		if d.Workflow != "" {
			d.Agent = ""
		}
	}
	if err := checkWatcherTargets(d); err != nil {
		return watcherError(err.Error()), WatcherEditOutput{}, nil
	}
	saved, err := watcher.Save(old, d, script)
	if err != nil {
		return watcherError(err.Error()), WatcherEditOutput{}, nil
	}
	msg := "updated watcher " + saved.Name
	if saved.Project != old.Project {
		msg += ", now in " + saved.Project
	}
	return watcherText(msg), WatcherEditOutput{Watcher: watcherView(saved)}, nil
}

func WatcherDeleteCore(sessionCwd string, in WatcherNameInput) (*mcp.CallToolResult, WatcherDeleteOutput, error) {
	d, err := resolveWatcher(sessionCwd, in.Project, in.Name)
	if err != nil {
		return watcherError(err.Error()), WatcherDeleteOutput{}, nil
	}
	if err := watcher.Delete(d.Ref()); err != nil {
		return watcherError(err.Error()), WatcherDeleteOutput{}, nil
	}
	return watcherText("deleted watcher " + d.Name + " from " + d.Project), WatcherDeleteOutput{Deleted: true}, nil
}

func WatcherRunCore(sessionCwd string, in WatcherNameInput) (*mcp.CallToolResult, WatcherRunOutput, error) {
	d, err := resolveWatcher(sessionCwd, in.Project, in.Name)
	if err != nil {
		return watcherError(err.Error()), WatcherRunOutput{}, nil
	}
	if d.Paused {
		return watcherError("watcher " + d.Name + " is paused; unpause it with watcher_edit first"), WatcherRunOutput{}, nil
	}
	err = watcher.MutateState(func(s *watcher.State) error {
		s.Watcher(d.ID()).RunRequested = time.Now().UTC()
		return nil
	})
	if err != nil {
		return watcherError(err.Error()), WatcherRunOutput{}, nil
	}
	return watcherText("requested a check of " + d.Name + "; the ask window that runs watchers starts it within a couple of seconds"), WatcherRunOutput{Requested: true}, nil
}
