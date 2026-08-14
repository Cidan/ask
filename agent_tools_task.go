package main

import (
	"context"
	"strings"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
)

const agentTaskToolDescription = `Launch a sub-agent with its own context window and collect its final report.

Without 'agent', a read-only research sub-agent runs on the current model with read/glob/grep/ls — use it for broad fan-out searches whose intermediate results would clutter your context. With 'agent', the named definition from <available_agents> runs instead: its own instructions, its own tool grants, and possibly a different model or provider entirely.

Set run_in_background:true to keep working while it runs — the call returns a job id immediately; poll the report with job_output and stop it with job_kill. The sub-agent's final message is returned verbatim as data.`

const agentTaskSystemPrompt = `You are a read-only research sub-agent inside a coding tool. You have the read, glob, grep, and ls tools — no shell, no editing, no network. NEVER use python, sed, awk, or other scripts to hack together edits. Only use the provided file modification tools (read/edit/write) to apply changes if they were provided (which they are not in your case).

Investigate the task you are given thoroughly: search broadly, read the relevant files, and chase cross-references until you can answer with confidence. Your final message is returned verbatim to the calling agent as data, so make it a complete, self-contained report: state the answer first, then the supporting evidence as file_path:line_number references. Report honestly when something cannot be found.`

const agentSubagentPromptTail = `

Your final message is returned verbatim to the calling agent as data — make it a complete, self-contained report.`

const agentTaskMaxSteps = 50

type agentTaskParams struct {
	Prompt          string `json:"prompt" description:"the self-contained task for the sub-agent, including everything it needs to know"`
	Agent           string `json:"agent,omitempty" description:"named agent definition to run (see <available_agents>); empty runs the default read-only researcher on the current model"`
	RunInBackground bool   `json:"run_in_background,omitempty" description:"run the sub-agent as a background job and return its job id immediately; poll with job_output"`
	Description     string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this sub-agent is doing"`
}

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
			toolsList := []fantasy.AgentTool{
				tools.ReadTool(env),
				tools.GlobTool(env),
				tools.GrepTool(env),
				tools.LsTool(env),
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
				for _, d := range discoverSubagents(env.Cwd) {
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
				toolsList = subagentTools(*def, env)
			}
			if lm == nil {
				return fantasy.NewTextErrorResponse("sub-agent model unavailable"), nil
			}

			run := func(runCtx context.Context) (string, error) {
				toolsList = wrapContextAwareTools(toolsList, env.Cwd, discoverRules(env.Cwd))
				taskCfg, _ := loadConfig()
				taskMaxRetries, _, _ := agentRetryOptions(taskCfg)
				sub := fantasy.NewAgent(lm,
					fantasy.WithSystemPrompt(system),
					fantasy.WithTools(toolsList...),
					fantasy.WithStopConditions(
						agentLoopDetectionCondition(),
						fantasy.StepCountIs(agentTaskMaxSteps),
					),
				)
				result, err := sub.Stream(runCtx, fantasy.AgentStreamCall{
					Prompt:          prompt,
					MaxOutputTokens: maxOutputTokensPtr(budget),
					MaxRetries:      retryMaxRetriesPtr(taskMaxRetries),
				})
				if err != nil {
					return "", err
				}
				if cost, known := stepCostUSD(lm.Provider(), lm.Model(), result.TotalUsage); known && env.Emit != nil {
					env.Emit(engine.CostEvent{
						BaseEvent: engine.BaseEvent{TabID: env.TabID},
						CostUSD:   cost,
					})
				}
				return strings.TrimSpace(result.Response.Content.Text()), nil
			}

			if p.RunInBackground {
				label := "agent"
				if p.Agent != "" {
					label = "agent " + p.Agent
				}
				jobCtx, cancel := context.WithCancel(context.Background())
				job := env.Jobs.Add(label+": "+short(prompt), true, cancel)
				go func() {
					report, err := run(jobCtx)
					switch {
					case err != nil:
						job.AppendOutput("sub-agent failed: " + err.Error())
						job.Finish(tools.ShellResult{ExitCode: 1})
					case report == "":
						job.AppendOutput("sub-agent returned no report")
						job.Finish(tools.ShellResult{ExitCode: 1})
					default:
						job.AppendOutput(report)
						job.Finish(tools.ShellResult{ExitCode: 0})
					}
					if env.Emit != nil {
						env.Emit(engine.BgTaskEndedEvent{
							BaseEvent: engine.BaseEvent{TabID: env.TabID},
							JobID:     job.ID,
						})
					}
				}()
				if env.Emit != nil {
					env.Emit(engine.BgTaskStartedEvent{
						BaseEvent:   engine.BaseEvent{TabID: env.TabID},
						JobID:       job.ID,
						Description: p.Description,
					})
				}
				return fantasy.NewTextResponse(
					"started background " + label + " as " + job.ID +
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
