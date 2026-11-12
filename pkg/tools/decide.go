package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"
)

const (
	// DecideMaxItems bounds items mode: each item is its own request.
	DecideMaxItems = 100
	// DecideMaxCalls bounds the tool calls one decide runs.
	DecideMaxCalls = 50
	// DecideMaxFileBytes bounds one file added to the input.
	DecideMaxFileBytes  = 50 << 20
	decideConcurrency   = 8
	decideMaxShownProbs = 5
	decideItemPreview   = 80
	decideLabelMax      = 100

	DecideToolDescription = `Answer closed-set questions with calibrated probabilities from a fast decision model: no generated text, about 200 ms and a fraction of a cent per call. A question is yes_no (the probability of yes), choice (one of 2-255 options), or score (2-255 ordered levels, lowest first); one call takes up to 1000 questions, each answered independently of the others. Use it to triage, filter, route, rank, or check many things at once — search hits, files, issues, log lines, test failures, screenshots, PDFs — instead of reading each yourself, and to settle a judgment you would otherwise guess at. calls runs tools for you (read, grep, glob, ls, bash, fetch, web_search, MCP tools, …; up to 50, in parallel) and hands their results to the decision model as JSON, together with the project rules and memory those tools bring in — the raw output never enters your context, you get only the answers. With scope "each", every question is asked of each call's result on its own; with "all" (the default), of all of them together. input is shared text; files adds images, audio, video, PDFs, or text files; items asks every question of each item on its own (up to 100). Every decide returns an id: read or grep decision:<id> (all calls) or decision:<id>/<n> (call n) to see the raw output behind an answer. The model sees a question's text, never its key, so write the whole question there and keep each to one thing. Not for open-ended answers or extracting values that are not in a list. Calls that write files, change the session, or start agents are refused.`

	// DecideNoModelNotice is the non-error reply when no decision model is
	// configured.
	DecideNoModelNotice = "No decision model is configured, so decide is unavailable. Finish the task without it, and tell the user they can add a Gemini API key under /config → Decisions to enable it."
)

// DecideParams is the decide tool's input.
type DecideParams struct {
	Input       string           `json:"input,omitempty" jsonschema:"text every question is asked about; with items or scope each, the context shared by every request"`
	Files       []string         `json:"files,omitempty" jsonschema:"paths of images, audio, video, PDFs, or text files to add to the input"`
	Calls       []DecideCall     `json:"calls,omitempty" jsonschema:"tool calls to run and hand the decision model, in place of you reading their output (up to 50)"`
	Scope       string           `json:"scope,omitempty" jsonschema:"all (default): ask the questions once over every call's result; each: ask them of each call's result on its own"`
	Items       []string         `json:"items,omitempty" jsonschema:"separate texts to classify: every question is asked of each item on its own (up to 100)"`
	Questions   []DecideQuestion `json:"questions" jsonschema:"1 to 1000 questions, each answered independently"`
	Description string           `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// DecideCall is one tool call decide runs on the model's behalf.
type DecideCall struct {
	Tool        string         `json:"tool" jsonschema:"name of a tool of this session, wire or registry (read, grep, bash, an mcp__ tool, …)"`
	Args        map[string]any `json:"args,omitempty" jsonschema:"the tool's arguments, as you would pass them calling it directly"`
	Description string         `json:"description,omitempty" jsonschema:"what this call gathers, shown to the user (defaults to the decide's description)"`
}

// DecideQuestion is one question of a decide call.
type DecideQuestion struct {
	Key      string         `json:"key" jsonschema:"label the answer comes back under: letters, digits, _ or -, up to 64 characters"`
	Type     string         `json:"type" jsonschema:"yes_no, choice, or score"`
	Question string         `json:"question" jsonschema:"the complete question — the model sees this, never the key"`
	Yes      string         `json:"yes,omitempty" jsonschema:"yes_no only: what counts as yes"`
	No       string         `json:"no,omitempty" jsonschema:"yes_no only: what counts as no"`
	Options  []DecideOption `json:"options,omitempty" jsonschema:"choice only: 2 to 255 options; add an 'other' option when an input may fit none"`
	Levels   []string       `json:"levels,omitempty" jsonschema:"score only: 2 to 255 evenly spaced levels, lowest first"`
}

// DecideOption is one option of a choice question.
type DecideOption struct {
	Key  string `json:"key" jsonschema:"the value returned when this option wins"`
	When string `json:"when,omitempty" jsonschema:"when this option applies; may be empty when the key explains itself"`
}

// DecideResult is the decide tool's response.
type DecideResult struct {
	Answers string `json:"answers,omitempty" jsonschema:"one line per question (yes_no as yes/no with p(yes), choice as options by probability, score as the expected level and the most likely one), then the address of each call's raw result"`
	ID      string `json:"id,omitempty" jsonschema:"this decision's id: read or grep decision:<id> for its raw results"`
	Model   string `json:"model,omitempty"`
	Notice  string `json:"notice,omitempty" jsonschema:"set when no decision model is configured"`
}

// decideRefusedTools are the tools decide will not run: they write, steer
// the session or the workflow, start agents, or would recurse.
var decideRefusedTools = map[string]bool{
	"decide": true, "task": true, "invoke_tool": true, "todos": true,
	"write": true, "edit": true, "job_kill": true,
	"end_turn": true, "finish_workflow": true, "exit_loop": true, "save_artifact": true,
	"preload_memory": true, "inject_tool_images": true,
}

// decideRefusedVerb matches the native tools whose name says they change
// something (workflow_create, watcher_run, plugin_install, memory_index, …).
var decideRefusedVerb = regexp.MustCompile(`(^|_)(create|edit|delete|copy|install|uninstall|publish|pull|add|remove|run|index|forget|reinforce|demote|update|set|enable|disable)(_|$)`)

func decideRefused(name string) bool {
	if decideRefusedTools[name] {
		return true
	}
	return !strings.HasPrefix(name, "mcp__") && decideRefusedVerb.MatchString(name)
}

// decideCallResult is one inner call: the record kept in the session, and
// what the decision model is handed — the call as JSON, then any images
// its tool produced.
type decideCallResult struct {
	call  engine.DecisionCall
	parts []*genai.Part
}

// decideUnit is one request of a decide that asks per call or per item.
type decideUnit struct {
	label string
	parts []*genai.Part
}

// DecideTool returns the decide tool: closed-set questions answered by the
// configured decision model, over input the model gives and tool calls it
// asks decide to run.
func DecideTool(env *ToolEnv) Tool {
	return NewTypedTool("decide", DecideToolDescription, func(ctx agent.Context, p DecideParams) (DecideResult, error) {
		questions, order, err := decideQuestions(p.Questions)
		if err != nil {
			return DecideResult{}, err
		}
		if len(p.Items) > DecideMaxItems {
			return DecideResult{}, fmt.Errorf("at most %d items per call, got %d", DecideMaxItems, len(p.Items))
		}
		if len(p.Calls) > DecideMaxCalls {
			return DecideResult{}, fmt.Errorf("at most %d calls per decide, got %d", DecideMaxCalls, len(p.Calls))
		}
		each, err := decideScopeEach(p.Scope, len(p.Calls))
		if err != nil {
			return DecideResult{}, err
		}
		shared, err := decideSharedInput(env, p)
		if err != nil {
			return DecideResult{}, err
		}
		if len(shared) == 0 && len(p.Items) == 0 && len(p.Calls) == 0 {
			return DecideResult{}, errors.New("input, files, calls, or items is required")
		}
		cfg, _ := config.Load()
		d, target, err := engine.DeciderBuilder(ctx, cfg)
		if errors.Is(err, engine.ErrNoDecisionModel) {
			return DecideResult{Notice: DecideNoModelNotice}, nil
		}
		if err != nil {
			return DecideResult{}, err
		}

		calls := runDecideCalls(ctx, env, p.Calls, p.Description)
		id := env.nextDecisionID(ctx)
		var units []decideUnit
		for i, c := range calls {
			if each {
				units = append(units, decideUnit{label: decideCallLine(id, i, c.call), parts: c.parts})
			} else {
				shared = append(shared, c.parts...)
			}
		}
		for _, item := range p.Items {
			units = append(units, decideUnit{label: decideItemLabel(item), parts: []*genai.Part{genai.NewPartFromText(item)}})
		}
		ask := func(input []*genai.Part) (providers.DecisionResult, error) {
			res, err := d.Decide(ctx, providers.DecisionRequest{Input: input, Questions: questions})
			env.RecordSpend(SpendDecision, res.Usage)
			return res, err
		}

		var answers string
		if len(units) == 0 {
			res, err := ask(shared)
			if err != nil {
				return DecideResult{}, err
			}
			answers = formatDecisionAnswers(order, questions, res.Answers, "")
		} else if answers, err = decideUnits(ctx, units, shared, order, questions, ask); err != nil {
			return DecideResult{}, err
		}

		rec := engine.DecisionRecord{ID: id, CallID: ctx.FunctionCallID(), Model: target.Label(), Answers: answers, CreatedAt: time.Now()}
		for _, c := range calls {
			rec.Calls = append(rec.Calls, c.call)
		}
		out := DecideResult{Answers: answers, ID: id, Model: target.Label()}
		if err := engine.SaveDecision(ctx, rec); err != nil {
			out.ID = ""
			if len(calls) > 0 {
				out.Answers += "\n\n(the raw results were not kept: " + err.Error() + ")"
			}
			return out, nil
		}
		if len(calls) > 0 {
			out.Answers += "\n\n" + decideCallIndex(id, rec.Calls)
		}
		return out, nil
	})
}

func decideScopeEach(scope string, calls int) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "", "all":
		return false, nil
	case "each":
		if calls == 0 {
			return false, errors.New(`scope "each" needs calls`)
		}
		return true, nil
	}
	return false, fmt.Errorf(`scope must be "all" or "each", got %q`, scope)
}

// runDecideCalls runs the inner calls in parallel, each through the
// session's own tool — the same one the model would call, with its
// approval prompt, filters, and the rules and memory it brings in — and
// shows each in the chat under the decide.
func runDecideCalls(ctx agent.Context, env *ToolEnv, calls []DecideCall, phrase string) []decideCallResult {
	out := make([]decideCallResult, len(calls))
	if len(calls) == 0 {
		return out
	}
	scope := engine.NewInnerCall()
	parent := ctx.FunctionCallID()
	if parent == "" {
		parent = "decide"
	}
	sem := make(chan struct{}, decideConcurrency)
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[i] = decideCallFailed(i, c, ctx.Err().Error())
				return
			}
			defer func() { <-sem }()
			out[i] = runDecideCall(ctx, env, scope.ForCall(), parent, i, c, phrase)
		}()
	}
	wg.Wait()
	return out
}

func runDecideCall(ctx agent.Context, env *ToolEnv, scope *engine.InnerCall, parent string, i int, c DecideCall, phrase string) decideCallResult {
	name := strings.TrimSpace(c.Tool)
	switch {
	case name == "":
		return decideCallFailed(i, c, "every call needs a tool")
	case decideRefused(name):
		return decideCallFailed(i, c, name+" cannot run inside decide: it writes, changes the session, or starts agents — call it directly")
	case env.Tools == nil:
		return decideCallFailed(i, c, "this session has no tools for decide to run")
	}
	t, ok := env.Tools.LookupTool(name)
	if !ok {
		return decideCallFailed(i, c, "unknown tool "+name)
	}
	args := make(map[string]any, len(c.Args)+1)
	for k, v := range c.Args {
		args[k] = v
	}
	if d := strings.TrimSpace(c.Description); d != "" {
		phrase = d
	}
	args, err := prepareCall(t, args, phrase)
	if err != nil {
		return decideCallFailed(i, c, err.Error())
	}
	useID := fmt.Sprintf("%s/%d", parent, i+1)
	if env.Emit != nil {
		env.Emit(engine.ToolCallEvent{BaseEvent: engine.BaseEvent{TabID: env.TabID}, ToolUseID: useID, ToolName: name, Input: args, Parent: parent})
	}
	call := engine.DecisionCall{Tool: name, Args: args}
	res, err := engine.RunADKTool(scope.Context(ctx), t, args)
	if err != nil {
		call.Error = err.Error()
	} else {
		call.Result = res
	}
	text, isErr := call.Text()
	if env.Emit != nil {
		env.Emit(engine.ToolResultEvent{BaseEvent: engine.BaseEvent{TabID: env.TabID}, ToolUseID: useID, ToolName: name, Output: text, IsError: isErr, Parent: parent})
	}
	return decideCallResult{call: call, parts: decideCallParts(i, call, scope.Images())}
}

func decideCallFailed(i int, c DecideCall, reason string) decideCallResult {
	call := engine.DecisionCall{Tool: strings.TrimSpace(c.Tool), Args: c.Args, Error: reason}
	return decideCallResult{call: call, parts: decideCallParts(i, call, nil)}
}

// decideCallParts is how the decision model sees one call: a JSON object
// with the call's number, tool, arguments, and result or error, then the
// images its tool produced.
func decideCallParts(i int, call engine.DecisionCall, images []*genai.Part) []*genai.Part {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Call   int            `json:"call"`
		Tool   string         `json:"tool"`
		Args   map[string]any `json:"args,omitempty"`
		Result map[string]any `json:"result,omitempty"`
		Error  string         `json:"error,omitempty"`
	}{i + 1, call.Tool, call.Args, call.Result, call.Error})
	return append([]*genai.Part{genai.NewPartFromText(strings.TrimSpace(b.String()))}, images...)
}

func decideCallLine(id string, i int, call engine.DecisionCall) string {
	return engine.DecisionAddress{ID: id, Call: i + 1}.String() + "  " + clipLabel(engine.DecisionCallLabel(call))
}

// decideCallIndex tells the model where each call's raw result is kept.
func decideCallIndex(id string, calls []engine.DecisionCall) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Raw results (read or grep %s, or one address below, for the output behind an answer):", engine.DecisionAddress{ID: id})
	for i, c := range calls {
		text, isErr := c.Text()
		size := fmt.Sprintf("%d lines", strings.Count(strings.TrimRight(text, "\n"), "\n")+1)
		if isErr {
			size = "error: " + clipLabel(firstLine(text))
		}
		fmt.Fprintf(&b, "\n  %s · %s", decideCallLine(id, i, c), size)
	}
	return b.String()
}

func clipLabel(s string) string {
	if r := []rune(s); len(r) > decideLabelMax {
		return string(r[:decideLabelMax]) + "…"
	}
	return s
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// decideUnits asks the questions of each unit on its own, after the shared
// input, and lists the answers under each unit's label.
func decideUnits(ctx context.Context, units []decideUnit, shared []*genai.Part, order []string, questions map[string]providers.Question,
	ask func([]*genai.Part) (providers.DecisionResult, error)) (string, error) {
	answers := make([]map[string]providers.Answer, len(units))
	errs := make([]error, len(units))
	sem := make(chan struct{}, decideConcurrency)
	var wg sync.WaitGroup
	for i, u := range units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			input := append(append([]*genai.Part(nil), shared...), u.parts...)
			res, err := ask(input)
			answers[i], errs[i] = res.Answers, err
		}()
	}
	wg.Wait()

	var b strings.Builder
	failed := 0
	for i, u := range units {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[%d] %s\n", i+1, u.label)
		if errs[i] != nil {
			failed++
			fmt.Fprintf(&b, "  error: %v\n", errs[i])
			continue
		}
		b.WriteString(formatDecisionAnswers(order, questions, answers[i], "  "))
		b.WriteByte('\n')
	}
	if failed == len(units) {
		return "", fmt.Errorf("every request failed: %w", errs[0])
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func decideItemLabel(item string) string {
	line := strings.Join(strings.Fields(item), " ")
	if r := []rune(line); len(r) > decideItemPreview {
		return string(r[:decideItemPreview]) + "…"
	}
	return line
}

// decideQuestions converts the tool's questions into a decision request's,
// keeping their order for the reply.
func decideQuestions(in []DecideQuestion) (map[string]providers.Question, []string, error) {
	if len(in) == 0 {
		return nil, nil, errors.New("at least one question is required")
	}
	out := make(map[string]providers.Question, len(in))
	order := make([]string, 0, len(in))
	for _, q := range in {
		key := strings.TrimSpace(q.Key)
		if key == "" {
			return nil, nil, errors.New("every question needs a key")
		}
		if _, dup := out[key]; dup {
			return nil, nil, fmt.Errorf("question key %q is used twice", key)
		}
		pq := providers.Question{Instructions: strings.TrimSpace(q.Question)}
		switch strings.ToLower(strings.TrimSpace(q.Type)) {
		case "yes_no", "yesno", "yes/no", "bool", "boolean":
			pq.Kind, pq.Yes, pq.No = providers.QuestionYesNo, strings.TrimSpace(q.Yes), strings.TrimSpace(q.No)
		case "choice":
			pq.Kind = providers.QuestionChoice
			pq.Options = make(map[string]string, len(q.Options))
			for _, o := range q.Options {
				k := strings.TrimSpace(o.Key)
				if _, dup := pq.Options[k]; dup {
					return nil, nil, fmt.Errorf("question %q lists option %q twice", key, k)
				}
				pq.Options[k] = strings.TrimSpace(o.When)
			}
		case "score":
			pq.Kind, pq.Levels = providers.QuestionScore, q.Levels
		default:
			return nil, nil, fmt.Errorf("question %q: type must be yes_no, choice, or score, got %q", key, q.Type)
		}
		out[key] = pq
		order = append(order, key)
	}
	return out, order, nil
}

// decideSharedInput is the input every request carries: the text, then
// each file.
func decideSharedInput(env *ToolEnv, p DecideParams) ([]*genai.Part, error) {
	var parts []*genai.Part
	if text := strings.TrimSpace(p.Input); text != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}
	for _, f := range p.Files {
		part, err := decideFilePart(env.AbsPath(f))
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func decideFilePart(path string) (*genai.Part, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > DecideMaxFileBytes {
		return nil, fmt.Errorf("%s is %d bytes, larger than the %d-byte limit", path, info.Size(), DecideMaxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	mimeType, _, _ = strings.Cut(mimeType, ";")
	mimeType = strings.TrimSpace(mimeType)
	if strings.HasPrefix(mimeType, "text/") || mimeType == "application/json" || mimeType == "application/xml" {
		return genai.NewPartFromText("File " + path + ":\n" + string(data)), nil
	}
	return genai.NewPartFromBytes(data, mimeType), nil
}

// formatDecisionAnswers renders one line per question, in the order asked.
func formatDecisionAnswers(order []string, questions map[string]providers.Question, answers map[string]providers.Answer, indent string) string {
	lines := make([]string, 0, len(order))
	for _, key := range order {
		a, ok := answers[key]
		if !ok {
			lines = append(lines, indent+key+": (no answer)")
			continue
		}
		lines = append(lines, indent+key+": "+formatDecisionAnswer(questions[key], a))
	}
	return strings.Join(lines, "\n")
}

func formatDecisionAnswer(q providers.Question, a providers.Answer) string {
	switch a.Kind {
	case providers.QuestionYesNo:
		verdict := "no"
		if a.Yes >= 0.5 {
			verdict = "yes"
		}
		return fmt.Sprintf("%s — p(yes) %.2f", verdict, a.Yes)
	case providers.QuestionChoice:
		ranked := a.RankedOptions()
		shown := ranked
		if len(shown) > decideMaxShownProbs {
			shown = shown[:decideMaxShownProbs]
		}
		parts := make([]string, 0, len(shown)+1)
		for _, opt := range shown {
			parts = append(parts, fmt.Sprintf("%s %.2f", opt, a.Probabilities[opt]))
		}
		if len(ranked) > len(shown) {
			parts = append(parts, fmt.Sprintf("+%d more", len(ranked)-len(shown)))
		}
		return strings.Join(parts, " · ")
	case providers.QuestionScore:
		level := a.Level()
		label := ""
		if level < len(q.Levels) {
			label = fmt.Sprintf(" %q", q.Levels[level])
		}
		p := 0.0
		if level < len(a.Levels) {
			p = a.Levels[level]
		}
		return fmt.Sprintf("%.2f on 0-%d · most likely %d%s (%.2f)", a.Score, len(q.Levels)-1, level, label, p)
	}
	return "(unknown answer)"
}
