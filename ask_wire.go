package main

// ask_wire.go holds the modal/approval wire shapes shared by the
// native agent tools (agent_tools_ask.go, agent_tools_bridge.go, the
// MCP elicitation handler) and the UI. These predate the removal of
// the loopback MCP bridge — the types are the contract between tool
// goroutines and the tea loop.

import (
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
)

type mcpOption struct {
	Label   string `json:"label" jsonschema:"short label for the option"`
	Diagram string `json:"diagram,omitempty" jsonschema:"required only for pick_diagram kind: monospace box-drawing art, max 40 cols x 12 rows"`
}

type mcpQuestion struct {
	Kind        string      `json:"kind" jsonschema:"one of pick_one, pick_many, pick_diagram"`
	Prompt      string      `json:"prompt" jsonschema:"the question shown to the user"`
	Options     []mcpOption `json:"options" jsonschema:"list of options for the user to choose from"`
	AllowCustom bool        `json:"allow_custom,omitempty" jsonschema:"append an Enter-your-own free-text option (pick_one and pick_many only)"`
}

type mcpAnswer struct {
	Picks  []string `json:"picks" jsonschema:"labels of options the user selected; empty if user only entered a custom answer"`
	Custom string   `json:"custom,omitempty" jsonschema:"free-form text if the user used Enter your own"`
	Note   string   `json:"note,omitempty" jsonschema:"additional note the user attached via n"`
}

type askOutput struct {
	Answers   []mcpAnswer `json:"answers" jsonschema:"answers in the same order as input questions"`
	Cancelled bool        `json:"cancelled,omitempty" jsonschema:"true if the user dismissed the dialog without submitting"`
}

type askReply struct {
	answers   []qAnswer
	cancelled bool
	// headless is set when the request targets a workflow tab, which
	// runs with no user present to answer. buildAskResult turns it into
	// a clear "you are headless, proceed on your own" notice
	// (workflowHeadlessAskNotice) rather than the misleading
	// user-cancellation error a real Esc would produce.
	headless bool
}

type askToolRequestMsg struct {
	tabID     int
	questions []question
	reply     chan askReply
}

// permissionRule mirrors the Claude Code permission-rule wire shape:
// toolName identifies the tool (e.g. "Edit", "Bash"); ruleContent narrows the
// rule to a specific target (file_path for file tools, command for Bash).
// An empty ruleContent means "every invocation of this tool".
type permissionRule struct {
	toolName    string
	ruleContent string
}

type approvalReply struct {
	allow    bool
	remember *permissionRule
}

type approvalRequestMsg struct {
	tabID     int
	toolName  string
	input     map[string]any
	toolUseID string
	reply     chan approvalReply
}

const askToolDescription = `Ask the user one or more questions through a tabbed modal in the ask terminal UI.

Each question is one of three kinds:
  - "pick_one": user picks exactly one option
  - "pick_many": user picks zero or more options
  - "pick_diagram": user picks exactly one option; each option has an ASCII-art
    preview that is rendered in a side box as the user navigates the list

All submitted questions are displayed together as tabs; the user answers each
before submitting. Answers are returned in input order.

Diagram format (pick_diagram only; strict):
  - Monospace box-drawing characters only: ╭╮╰╯─│├┤┬┴┼
  - Fill blocks: ░ for content areas, ▓ for interactive or accent areas
  - No emoji, no tabs, no trailing whitespace
  - At most 40 columns wide and 12 rows tall; all diagrams in one question are
    padded to the same bounding box before rendering, so smaller is fine

Set allow_custom=true on pick_one or pick_many to append an Enter-your-own
option that accepts free-form multi-line text from the user.`

// workflowHeadlessAskNotice is returned to the agent when it calls
// ask_user_question from a workflow tab. Workflow steps run headless —
// there is no user at the terminal to answer — so instead of stranding
// the chain on a modal nobody can dismiss (or returning a misleading
// "user cancelled" error), we tell the agent it is headless and to
// decide on its own.
const workflowHeadlessAskNotice = "This step is running headless as part of an automated workflow. " +
	"There is no user available to answer questions, so ask_user_question cannot be used here. " +
	"Do not ask questions — proceed using your best judgment with the information you already have, " +
	"making and clearly stating any reasonable assumptions where a choice is required."

// teaProgramPtr is shared by every tab's mcpBridge. main.go stores the
// *tea.Program into it after tea.NewProgram so bridges can route tool
// requests (ask / approval) back to the owning tab through the app.
var teaProgramPtr atomic.Pointer[tea.Program]

func setTeaProgram(p *tea.Program) { teaProgramPtr.Store(p) }

func convertMCPQuestions(qs []mcpQuestion) []question {
	out := make([]question, len(qs))
	for i, q := range qs {
		var kind qKind
		switch q.Kind {
		case "pick_many":
			kind = qPickMany
		case "pick_diagram":
			kind = qPickDiagram
		default:
			kind = qPickOne
		}
		labels := make([]string, 0, len(q.Options)+1)
		diagrams := make([]string, 0, len(q.Options)+1)
		for _, o := range q.Options {
			labels = append(labels, o.Label)
			diagrams = append(diagrams, o.Diagram)
		}
		if q.AllowCustom && kind != qPickDiagram {
			labels = append(labels, "Enter your own")
			diagrams = append(diagrams, "")
		}
		out[i] = question{
			kind:     kind,
			prompt:   q.Prompt,
			options:  labels,
			diagrams: diagrams,
		}
	}
	return out
}

func convertMCPAnswers(qs []mcpQuestion, answers []qAnswer) []mcpAnswer {
	out := make([]mcpAnswer, len(qs))
	for i, q := range qs {
		ans := answers[i]
		customIdx := -1
		if q.AllowCustom && q.Kind != "pick_diagram" {
			customIdx = len(q.Options)
		}
		var picks []string
		for idx := 0; idx < len(q.Options); idx++ {
			if ans.picks[idx] {
				picks = append(picks, q.Options[idx].Label)
			}
		}
		if picks == nil {
			picks = []string{}
		}
		custom := ""
		if customIdx >= 0 && ans.picks[customIdx] {
			custom = ans.custom
		}
		out[i] = mcpAnswer{
			Picks:  picks,
			Custom: custom,
			Note:   ans.note,
		}
	}
	return out
}

func permissionRuleFor(toolName string, input map[string]any) permissionRule {
	r := permissionRule{toolName: toolName}
	switch toolName {
	case "Edit", "Write", "MultiEdit", "NotebookEdit", "Read":
		if p, _ := input["file_path"].(string); p != "" {
			r.ruleContent = p
		}
	case "Bash":
		if c, _ := input["command"].(string); c != "" {
			r.ruleContent = c
		}
	}
	return r
}
