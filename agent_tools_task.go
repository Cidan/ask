package main

import (
	"context"
	"strings"

	"charm.land/fantasy"
)

const agentTaskToolDescription = `Launch a sub-agent with its own context window and collect its final report.

Without 'agent', a read-only research sub-agent runs on the current model with read/glob/grep/ls — use it for broad fan-out searches whose intermediate results would clutter your context. With 'agent', the named definition from <available_agents> runs instead: its own instructions, its own tool grants, and possibly a different model or provider entirely.

Set run_in_background:true to keep working while it runs — the call returns a job id immediately; poll the report with job_output and stop it with job_kill. The sub-agent's final message is returned verbatim as data.`

const agentTaskSystemPrompt = `You are a read-only research sub-agent inside a coding tool. You have the read, glob, grep, and ls tools — no shell, no editing, no network.

Investigate the task you are given thoroughly: search broadly, read the relevant files, and chase cross-references until you can answer with confidence. Your final message is returned verbatim to the calling agent as data, so make it a complete, self-contained report: state the answer first, then the supporting evidence as file_path:line_number references. Report honestly when something cannot be found.`

// agentSubagentPromptTail is appended to every named subagent's system
// prompt so the report contract holds regardless of how the definition
// was written.
const agentSubagentPromptTail = `

Your final message is returned verbatim to the calling agent as data — make it a complete, self-contained report.`

// agentTaskMaxSteps is a hard backstop on sub-agent looping; loop
// detection is the primary guard, this just bounds the worst case.
const agentTaskMaxSteps = 50

type agentTaskParams struct {
	Prompt          string `json:"prompt" description:"the self-contained task for the sub-agent, including everything it needs to know"`
	Agent           string `json:"agent,omitempty" description:"named agent definition to run (see <available_agents>); empty runs the default read-only researcher on the current model"`
	RunInBackground bool   `json:"run_in_background,omitempty" description:"run the sub-agent as a background job and return its job id immediately; poll with job_output"`
}

// agentTaskTool spawns a child fantasy agent. The default is the
// read-only researcher on the parent's model; a named definition can
// pin different instructions, tools, and — because every in-process
// provider is an agentProviderSpec — a different model or provider
// (cross-provider delegation). Background runs ride the existing job
// manager, so job_output/job_kill and the bgTask UI signals work
// unchanged.
func agentTaskTool(env *agentToolEnv, model func() fantasy.LanguageModel, maxTokens func() int64) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"task",
		agentTaskToolDescription,
		func(ctx context.Context, p agentTaskParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			prompt := strings.TrimSpace(p.Prompt)
			if prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			system := agentTaskSystemPrompt
			tools := []fantasy.AgentTool{
				agentReadTool(env),
				agentGlobTool(env),
				agentGrepTool(env),
				agentLsTool(env),
			}
			lm := model()
			var budget int64
			if maxTokens != nil {
				budget = maxTokens()
			}
			parentProviderID := ""
			if lm != nil {
				parentProviderID = lm.Provider()
			}

			if name := strings.TrimSpace(p.Agent); name != "" {
				var def *subagentDef
				for _, d := range discoverSubagents(env.cwd) {
					if d.Name == name {
						dd := d
						def = &dd
						break
					}
				}
				if def == nil {
					return fantasy.NewTextErrorResponse("unknown agent " + name + " — see <available_agents> for what is defined"), nil
				}
				resolved, pinnedBudget, err := resolveSubagentModel(*def, parentProviderID, lm)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				lm = resolved
				if pinnedBudget > 0 {
					budget = pinnedBudget
				}
				if def.Prompt != "" {
					system = def.Prompt + agentSubagentPromptTail
				}
				tools = subagentTools(*def, env)
			}
			if lm == nil {
				return fantasy.NewTextErrorResponse("sub-agent model unavailable"), nil
			}

			run := func(runCtx context.Context) (string, error) {
				sub := fantasy.NewAgent(lm,
					fantasy.WithSystemPrompt(system),
					fantasy.WithTools(tools...),
					fantasy.WithStopConditions(
						agentLoopDetectionCondition(),
						fantasy.StepCountIs(agentTaskMaxSteps),
					),
				)
				// Stream, not Generate: sub-agent turns can run long and
				// the anthropic SDK refuses non-streaming requests whose
				// max_tokens budget implies more than ~10 minutes.
				result, err := sub.Stream(runCtx, fantasy.AgentStreamCall{
					Prompt:          prompt,
					MaxOutputTokens: maxOutputTokensPtr(budget),
				})
				if err != nil {
					return "", err
				}
				return strings.TrimSpace(result.Response.Content.Text()), nil
			}

			if p.RunInBackground {
				label := "agent"
				if p.Agent != "" {
					label = "agent " + p.Agent
				}
				jobCtx, cancel := context.WithCancel(context.Background())
				job := env.jobs.add(label+": "+short(prompt), cancel)
				go func() {
					report, err := run(jobCtx)
					switch {
					case err != nil:
						job.appendOutput("sub-agent failed: " + err.Error())
						job.finish(shellResult{exitCode: 1})
					case report == "":
						job.appendOutput("sub-agent returned no report")
						job.finish(shellResult{exitCode: 1})
					default:
						job.appendOutput(report)
						job.finish(shellResult{exitCode: 0})
					}
					if env.emit != nil {
						env.emit(bgTaskEndedMsg{taskID: job.id})
					}
				}()
				if env.emit != nil {
					env.emit(bgTaskStartedMsg{taskID: job.id})
				}
				return fantasy.NewTextResponse(
					"started background " + label + " as " + job.id +
						"; poll the report with job_output and stop it with job_kill"), nil
			}

			report, err := run(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse("sub-agent failed: " + err.Error()), nil
			}
			if report == "" {
				return fantasy.NewTextErrorResponse("sub-agent returned no report"), nil
			}
			return fantasy.NewTextResponse(report), nil
		},
	)
}
