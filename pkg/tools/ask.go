package tools

import (
	"errors"
	"fmt"
	"google.golang.org/adk/v2/agent"
	"strings"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/workflow"
)

const AskToolDescription = `Ask the user one or more questions through a tabbed modal in the ask terminal UI.

Use this tool when a decision is best made by the user and you cannot reasonably infer the answer from context, prior turns, or project conventions. Do not use it for trivia you can answer yourself, and do not use it as a substitute for a plan or a todo list.

# Crafting in-depth, fully formed questions

The user is reading the prompt cold, with no access to your chain of thought. Assume they cannot see any of your intermediate reasoning, the files you have read, or the tradeoffs you have been weighing. The prompt must therefore be a self-contained brief, not a fragment.

Each question's prompt should:

- Lead with the actual question, phrased precisely. The user should be able to answer it without re-reading the surrounding options.
- Span a full paragraph when the decision warrants it. Short one-liners are fine for simple choices, but for anything with real consequences, write as much as you need to make the choice clear — multiple paragraphs, code snippets, or concrete examples are all welcome. Do not artificially compress.
- State the rationale for asking. Explain WHY you are asking, what you have already considered or ruled out, and what the user knows that you do not (e.g. requirements, deadlines, audience, deployment constraints, prior art, taste, policy).
- Lay out the tradeoffs between the options. For each option, briefly note what it gains, what it costs, and the failure mode it is most exposed to. Tradeoffs often matter more than the options themselves — surface them so the user can weigh in rather than guessing.
- Make the recommendation explicit when you have one. If one option is clearly best in most scenarios and the choice only matters on the margins, say so. The user is free to override; a stated recommendation speeds up the common case.
- Anticipate the "I would have asked you about X" follow-up. If there is a related decision the user is likely to want to make in the same pass, group it as another question in the same call rather than bouncing back and forth.
- Name what happens on the default path. If the user were to dismiss or skip the question, what would you do? Saying so removes the cost of saying nothing.

When NOT to ask:

- You can answer it from the codebase, the conversation, or reasonable engineering judgment. Pick a default and state it; the user can correct you.
- The question is rhetorical or for your own bookkeeping. Use a todo or a note, not the user.
- The choice is reversible and cheap. Just make it and surface it in the result.

# Modal shape

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

const WorkflowHeadlessAskNotice = `ask_user_question is not available during automated workflow execution (headless mode). The user has already authorized this workflow run; make decisions autonomously based on the instructions, project conventions, and context provided. Do not retry ask_user_question.`

const EndTurnToolDescription = `Report the end of your turn for the current workflow step. REQUIRED on every step.

Call this once, as the final action of your turn, with:
  - summary: 1-3 sentences describing what you did this step and the outcome (plus anything left to do). This becomes this step's entry in the workflow log — write it for a human following along, not as a note to yourself.
  - decision: ONLY when your step prompt says you are the final step of a workflow loop iteration. Pass "continue" to run another iteration or "break" to end the loop. Breaking should be exceptional — only when the loop's exit goal is met. Omit decision when you are not inside a loop, or not its final step (unless you are deliberately breaking the loop early).

Calling this RECORDS your report; it does NOT end your turn early or exit a loop immediately. Finish your turn normally — the workflow acts on what you registered when your turn completes. If your turn ends without calling end_turn (or, as a loop's final step, without a decision), you will be re-prompted to provide it.`

type AskOption struct {
	Label   string `json:"label" jsonschema:"short label for the option"`
	Diagram string `json:"diagram,omitempty" jsonschema:"required only for pick_diagram kind: monospace box-drawing art, max 40 cols x 12 rows"`
}

type AskQuestion struct {
	Kind        string      `json:"kind" jsonschema:"one of pick_one, pick_many, pick_diagram"`
	Prompt      string      `json:"prompt" jsonschema:"the question shown to the user"`
	Options     []AskOption `json:"options" jsonschema:"list of options for the user to choose from"`
	AllowCustom bool        `json:"allow_custom,omitempty" jsonschema:"append an Enter-your-own free-text option (pick_one and pick_many only)"`
}

type AskParams struct {
	Questions   []AskQuestion `json:"questions" jsonschema:"one or more questions to ask the user together in a tabbed modal"`
	Description string        `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what is being asked"`
}

type AskOutput struct {
	Answers []engine.QuestionAnswer `json:"answers"`
}

// AskUserQuestionTool returns the interactive ask_user_question tool.
// AskResult is the ask_user_question tool's response.
type AskResult struct {
	Answers   []engine.QuestionAnswer `json:"answers,omitempty" jsonschema:"one entry per question asked"`
	Notice    string                  `json:"notice,omitempty" jsonschema:"why no answers were collected"`
	Cancelled bool                    `json:"cancelled,omitempty"`
	Headless  bool                    `json:"headless,omitempty" jsonschema:"true when no human was available to answer"`
}

func AskUserQuestionTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"ask_user_question",
		AskToolDescription,
		func(ctx agent.Context, p AskParams) (AskResult, error) {
			if len(p.Questions) == 0 {
				return AskResult{}, errors.New("at least one question is required")
			}
			engineQs := make([]engine.Question, 0, len(p.Questions))
			for _, q := range p.Questions {
				opts := make([]engine.QuestionOption, 0, len(q.Options))
				for _, o := range q.Options {
					opts = append(opts, engine.QuestionOption{Label: o.Label, Diagram: o.Diagram})
				}
				engineQs = append(engineQs, engine.Question{
					Kind:        q.Kind,
					Prompt:      q.Prompt,
					Options:     opts,
					AllowCustom: q.AllowCustom,
				})
			}

			if env.Interaction != nil {
				resp, err := env.Interaction.AskQuestion(ctx, env.TabID, engineQs)
				if err != nil {
					return AskResult{}, errors.New(err.Error())
				}
				if resp.Headless {
					return AskResult{Headless: true, Notice: WorkflowHeadlessAskNotice}, nil
				}
				if resp.Cancelled {
					return AskResult{Cancelled: true, Notice: "user cancelled the dialog"}, nil
				}

				return AskResult{Answers: resp.Answers}, nil
			}

			return AskResult{}, errors.New("ask UI not ready")
		},
	)
}

type FinishWorkflowParams struct {
	Description string   `json:"description" jsonschema:"required: summary of the workflow outcome"`
	Artifacts   []string `json:"artifacts,omitempty" jsonschema:"list of created/modified artifacts (e.g. PR link). If a PR was created, it MUST be a part of the artifacts"`
}

// FinishWorkflowTool records final workflow artifacts and outcome description.
// FinishWorkflowResult is the finish_workflow tool's response.
type FinishWorkflowResult struct {
	Recorded bool   `json:"recorded,omitempty"`
	Next     string `json:"next,omitempty" jsonschema:"what the caller must do next"`
}

func FinishWorkflowTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"finish_workflow",
		"Report the final outcome and artifacts of the workflow. REQUIRED on the final step.",
		func(ctx agent.Context, p FinishWorkflowParams) (FinishWorkflowResult, error) {
			desc := strings.TrimSpace(p.Description)
			if desc == "" {
				return FinishWorkflowResult{}, errors.New("description is required: provide a summary of the workflow outcome")
			}

			env.PendingFinishData = &FinishWorkflowData{
				Description: desc,
				Artifacts:   p.Artifacts,
			}

			return FinishWorkflowResult{Recorded: true, Next: "Now call end_turn to complete the step."}, nil
		},
	)
}

type EndTurnParams struct {
	Summary  string `json:"summary" jsonschema:"required: 1-3 sentence summary of what you did this step (and what remains), recorded as this step's line in the workflow log"`
	Decision string `json:"decision,omitempty" jsonschema:"loop control, required only on the final step of a loop iteration: 'continue' to run another iteration or 'break' to end the loop; omit when not the final step of a loop"`
}

// EndTurnTool registers the workflow step summary and loop decision.
// EndTurnResult is the end_turn tool's response.
type EndTurnResult struct {
	Recorded bool   `json:"recorded,omitempty"`
	Note     string `json:"note,omitempty" jsonschema:"guidance about what happens next"`
}

func EndTurnTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"end_turn",
		EndTurnToolDescription,
		func(ctx agent.Context, p EndTurnParams) (EndTurnResult, error) {
			summary := strings.TrimSpace(p.Summary)
			if summary == "" {
				return EndTurnResult{}, errors.New("summary is required: describe in 1-3 sentences what you did this step")
			}
			decision := strings.TrimSpace(p.Decision)
			if decision != "" && decision != "break" && decision != "continue" {
				return EndTurnResult{}, fmt.Errorf(
					"decision, when provided, must be %q or %q", "continue", "break")
			}
			env.PendingEndTurn = &EndTurnSignal{Decision: decision, Summary: summary}
			note := "end_turn recorded"
			if decision != "" {
				note += " (decision: " + decision + ")"
			}
			note += ". Finish your turn normally; the workflow acts on it when your turn ends."
			return EndTurnResult{Recorded: true, Note: note}, nil
		},
	)
}

type FinalizedPlanParams struct {
	Plan            string `json:"plan" jsonschema:"required: the full markdown plan covering the necessary file changes, tests, and verification steps. MUST be an exhaustive, code-complete specification with exact before/after code blocks, function signatures, wire/caller verification matrix, anti-stub rules, and specific test assertions. Note that workflow execution runs as a separate subagent context that CANNOT read the current chat history; your plan MUST be completely self-contained, including all necessary context, code locations, reasoning, and intent."`
	Explanation     string `json:"explanation" jsonschema:"required: one or two sentences explaining why this plan is optimal"`
	DefaultWorkflow string `json:"default_workflow,omitempty" jsonschema:"optional: the matched/suggested workflow name (e.g. 'ship') if any matches the plan"`
}

// FinalizedPlanTool presents a finalized plan for confirmation or workflow dispatch.
// FinalizedPlanResult is the finalized_plan tool's response.
type FinalizedPlanResult struct {
	Outcome  string `json:"outcome,omitempty" jsonschema:"what the user decided and what to do next"`
	Approved bool   `json:"approved,omitempty"`
	Workflow string `json:"workflow,omitempty" jsonschema:"name of the workflow that ran, when one did"`
}

func FinalizedPlanTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"finalized_plan",
		"Present a finalized implementation plan to the user for confirmation and execution choice. Invoking this tool MUST be your absolute final action in the turn. Once called, do not generate any further text or perform any more planning, as the user will be presented with a modal to launch a workflow or execute the plan directly. The workflow runs in a separate, isolated subagent context without access to this chat's history, so the plan must be completely self-contained.",
		func(ctx agent.Context, p FinalizedPlanParams) (FinalizedPlanResult, error) {
			plan := strings.TrimSpace(p.Plan)
			explanation := strings.TrimSpace(p.Explanation)
			if plan == "" {
				return FinalizedPlanResult{}, errors.New("plan is required")
			}
			if explanation == "" {
				return FinalizedPlanResult{}, errors.New("explanation is required")
			}

			if env.Interaction != nil {
				resp, err := env.Interaction.ConfirmPlan(ctx, env.TabID, engine.PlanRequest{
					Plan:            plan,
					Explanation:     explanation,
					DefaultWorkflow: strings.TrimSpace(p.DefaultWorkflow),
				})
				if err != nil {
					return FinalizedPlanResult{}, errors.New("plan confirmation failed: " + err.Error())
				}
				if resp.Headless {
					return FinalizedPlanResult{Approved: true, Outcome: "This step is running headless as part of an automated workflow. Continuing directly."}, nil
				}
				if resp.Cancelled {
					return FinalizedPlanResult{}, errors.New("user cancelled or closed the finalized plan dialog")
				}
				if resp.TalkMore {
					return FinalizedPlanResult{Outcome: "The user declined the plan and wants to continue discussing. Re-evaluate your approach based on the user's feedback."}, nil
				}
				if resp.ExecuteInline {
					env.MarkWorkflowsChecked()
					env.MarkWorkflowRunDispatched()
					return FinalizedPlanResult{Approved: true, Outcome: "Plan approved for inline execution. Planning mode has been turned OFF and todos guards have been disarmed. You can now execute your plan directly using write/edit/bash/etc."}, nil
				}
				if resp.WorkflowName != "" {
					env.MarkWorkflowsChecked()
					env.MarkWorkflowRunDispatched()

					if env.WorkflowRunner != nil {
						def, err := workflow.ResolveByName(env.Cwd, resp.WorkflowName, "")
						if err != nil {
							return FinalizedPlanResult{}, errors.New("could not resolve workflow: " + err.Error())
						}
						out, err := env.WorkflowRunner(ctx, env.TabID, def, resp.Source)
						if err != nil {
							return FinalizedPlanResult{}, errors.New("workflow execution failed: " + err.Error())
						}
						return FinalizedPlanResult{Approved: true, Workflow: resp.WorkflowName, Outcome: out}, nil
					}
					return FinalizedPlanResult{Approved: true, Workflow: resp.WorkflowName, Outcome: "Workflow dispatched."}, nil
				}
				return FinalizedPlanResult{Approved: true, Outcome: "Plan approved."}, nil
			}

			return FinalizedPlanResult{}, errors.New("plan confirmation UI not ready")
		},
	)
}
