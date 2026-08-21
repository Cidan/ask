package workflow

import (
	"strings"
	"time"

	"google.golang.org/adk/v2/session"
)

// Progress turns the ADK event stream of a running workflow graph into
// RunnerListener callbacks.
//
// Every callback is driven by something that actually happened in the
// stream: a step is "started" when an event authored by its agent
// arrives, and "done" when that step's successor starts or the run ends
// cleanly. Steps that never ran are never reported, and a run that fails
// partway reports only the steps that got that far — the previous
// implementation closed out every remaining step as completed and
// hardcoded a successful FinishData, so a chain that died at step 1 of 5
// showed 5/5 green.
type Progress struct {
	compiled *Compiled
	def      Def
	src      Source
	tabID    int
	listener RunnerListener
	tracker  *Tracker
	cwd      string

	started map[int]bool
	done    map[int]bool
	// summaries holds the latest end_turn summary seen for each step,
	// used as the step's completion line.
	summaries map[int]string
	// text holds the latest assistant text per step, the fallback when
	// a step ends without calling end_turn.
	text map[int]string

	current    int
	haveCurr   bool
	loopIters  map[int]int
	finishData *FinishData
	state      *RunState
}

// NewProgress builds a Progress for one run and emits OnWorkflowStarted.
func NewProgress(compiled *Compiled, def Def, src Source, cwd string, tabID int, listener RunnerListener, tracker *Tracker) *Progress {
	if listener == nil {
		listener = NoopRunnerListener{}
	}
	p := &Progress{
		compiled:  compiled,
		def:       def,
		src:       src,
		cwd:       cwd,
		tabID:     tabID,
		listener:  listener,
		tracker:   tracker,
		started:   map[int]bool{},
		done:      map[int]bool{},
		summaries: map[int]string{},
		text:      map[int]string{},
		loopIters: map[int]int{},
		current:   -1,
		state: &RunState{
			Workflow:  def,
			Source:    src,
			StartedAt: time.Now().UTC(),
		},
	}
	p.listener.OnWorkflowStarted(tabID, def, src)
	if tracker != nil {
		tracker.MarkWorking(cwd, src.Key(), def.Name, tabID)
	}
	return p
}

// State exposes the run state accumulated so far.
func (p *Progress) State() *RunState { return p.state }

// Observe consumes one event from the workflow's ADK stream.
func (p *Progress) Observe(ev *session.Event) {
	if p == nil || ev == nil || ev.Author == "" || ev.Author == "user" {
		return
	}
	idx, ok := p.compiled.StepIndex(ev.Author)
	if !ok {
		return
	}

	p.enter(idx)
	p.captureText(idx, ev)
	p.captureToolSignals(idx, ev)
}

// enter marks a step started, closing out the previous one. A workflow
// graph runs its nodes in order, so the arrival of an event for a later
// step is the signal that the earlier one finished.
func (p *Progress) enter(idx int) {
	if p.haveCurr && p.current == idx {
		return
	}
	if p.haveCurr && p.current != idx {
		p.complete(p.current)
	}
	p.current = idx
	p.haveCurr = true
	p.state.StepIdx = idx

	if p.started[idx] {
		// Re-entering a loop node: a new iteration of the same step.
		p.loopIters[idx]++
		if step := p.step(idx); step != nil && step.IsLoop() {
			p.listener.OnNote(p.tabID, LoopNoteLine(step.Name,
				"iteration "+itoa(p.loopIters[idx]+1), ""))
		}
		return
	}
	p.started[idx] = true
	step := p.step(idx)
	if step == nil {
		return
	}
	p.listener.OnWorkflowStepStarted(p.tabID, idx, step.Name, step.Provider, step.Model)
	if step.IsLoop() {
		p.listener.OnNote(p.tabID, LoopNoteLine(step.Name, "started",
			"max "+itoa(p.def.EffectiveMaxIterations(*step))+" iteration(s)"))
	}
}

func (p *Progress) captureText(idx int, ev *session.Event) {
	if ev.LLMResponse.Content == nil {
		return
	}
	var b strings.Builder
	for _, part := range ev.LLMResponse.Content.Parts {
		if part == nil || part.Thought || part.Text == "" {
			continue
		}
		b.WriteString(part.Text)
	}
	if t := strings.TrimSpace(b.String()); t != "" {
		p.text[idx] = t
	}
}

// captureToolSignals reads the two tool calls that carry workflow
// meaning: end_turn's summary (the step's log line) and finish_workflow's
// completion report.
func (p *Progress) captureToolSignals(idx int, ev *session.Event) {
	if ev.LLMResponse.Content == nil {
		return
	}
	for _, part := range ev.LLMResponse.Content.Parts {
		if part == nil || part.FunctionCall == nil {
			continue
		}
		switch part.FunctionCall.Name {
		case "end_turn":
			if s, ok := part.FunctionCall.Args["summary"].(string); ok && strings.TrimSpace(s) != "" {
				p.summaries[idx] = strings.TrimSpace(s)
			}
		case "exit_loop":
			if step := p.step(idx); step != nil && step.IsLoop() {
				p.listener.OnNote(p.tabID, LoopNoteLine(step.Name, "break", ""))
			}
		case "finish_workflow":
			desc, _ := part.FunctionCall.Args["description"].(string)
			p.finishData = &FinishData{
				Description: desc,
				Artifacts:   stringSlice(part.FunctionCall.Args["artifacts"]),
			}
		}
	}
}

func (p *Progress) complete(idx int) {
	if p.done[idx] {
		return
	}
	p.done[idx] = true
	step := p.step(idx)
	if step == nil {
		return
	}
	summary := p.summaries[idx]
	if summary == "" {
		summary = firstLine(p.text[idx])
	}
	p.listener.OnWorkflowStepDone(p.tabID, idx, summary)
}

// Finish closes the run. A nil err completes the in-flight step and
// reports success; a non-nil err leaves every unfinished step unfinished
// and reports the failure.
func (p *Progress) Finish(err error) *RunState {
	if p == nil {
		return nil
	}
	if err != nil {
		p.state.Failed = true
		p.state.FailedReason = err.Error()
		p.listener.OnWorkflowFailed(p.tabID, err.Error())
		if p.tracker != nil {
			p.tracker.MarkFinal(p.cwd, p.src.Key(), p.def.Name, StatusFailed, p.state.StepIdx)
		}
		return p.state
	}

	if p.haveCurr {
		p.complete(p.current)
	}
	p.state.Done = true
	p.state.FinishData = p.finishData

	desc := ""
	var arts []string
	if p.finishData != nil {
		desc = p.finishData.Description
		arts = p.finishData.Artifacts
	}
	p.listener.OnWorkflowDone(p.tabID, desc, arts)
	if p.tracker != nil {
		p.tracker.MarkFinal(p.cwd, p.src.Key(), p.def.Name, StatusDone, p.state.StepIdx)
	}
	return p.state
}

// SetFinishData records the run's completion report when it is read
// directly off the tool environment rather than seen in the event
// stream — the finish_workflow tool parks it there as it runs.
func (p *Progress) SetFinishData(fd *FinishData) {
	if p == nil || fd == nil {
		return
	}
	p.finishData = fd
}

func (p *Progress) step(idx int) *Step {
	if idx < 0 || idx >= len(p.def.Steps) {
		return nil
	}
	return &p.def.Steps[idx]
}

func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
