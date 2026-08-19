package main

import (
	"context"
	"strings"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
	"github.com/google/uuid"
)

const agentTaskToolDescription = `Launch a sub-agent with its own context window and collect its final report.

Without 'agent', a research sub-agent runs on the current model with full access to the codebase (read, search, bash, fetch, web search, MCPs) — use it for broad fan-out searches, deep file reading, and multi-component investigations whose intermediate results would clutter your context. Multiple sub-agents can be launched in parallel in a single turn. With 'agent', the named definition from <available_agents> runs instead: its own instructions, its own tool grants, and possibly a different model or provider entirely.

Set run_in_background:true to keep working while it runs — the call returns a job id immediately; poll the report with job_output and stop it with job_kill. The sub-agent's final message is returned verbatim as data.`

const agentTaskSystemPrompt = `You are a research sub-agent inside a coding tool. You have access to the full toolset to investigate the codebase, read and edit files, search, and run commands as needed.

Investigate the task you are given thoroughly: search broadly, read the relevant files, and chase cross-references until you can answer with confidence. Your final message is returned verbatim to the calling agent as data, so make it a complete, self-contained report: state the answer first, then the supporting evidence as file_path:line_number references. Report honestly when something cannot be found.`

const agentSubagentPromptTail = `

Your final message is returned verbatim to the calling agent as data — make it a complete, self-contained report.`

type agentTaskParams struct {
	Prompt          string `json:"prompt" description:"the self-contained task for the sub-agent, including everything it needs to know"`
	Agent           string `json:"agent,omitempty" description:"named agent definition to run (see <available_agents>); empty runs the default read-only researcher on the current model"`
	RunInBackground bool   `json:"run_in_background,omitempty" description:"run the sub-agent as a background job and return its job id immediately; poll with job_output"`
	Description     string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this sub-agent is doing"`
}

func agentTaskTool(env *agentToolEnv, getSession func() *agentSession) tools.Tool {
	return tools.NewTool(
		"task",
		agentTaskToolDescription,
		func(ctx context.Context, p agentTaskParams) (tools.ToolResponse, error) {
			prompt := strings.TrimSpace(p.Prompt)
			if prompt == "" {
				return tools.NewTextErrorResponse("prompt is required"), nil
			}

			subagentID := uuid.New().String()[:8]
			agentType := "researcher"
			if p.Agent != "" {
				agentType = p.Agent
			}
			subDesc := strings.TrimSpace(p.Description)
			if subDesc == "" {
				subDesc = prompt
			}

			subEnv := tools.NewSubagentToolEnv(env, subagentID)
			sess := getSession()
			modelID := "gemini-3.1-pro-preview"
			if sess != nil && sess.modelID != "" {
				modelID = sess.modelID
			}

			deferredFn := func() []tools.Tool {
				if sess != nil {
					return sess.deferredTools()
				}
				return nil
			}

			var toolsList []tools.Tool
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
					return tools.NewTextErrorResponse("unknown agent " + name + " — see <available_agents> for what is defined"), nil
				}
				if def.Model != "" {
					modelID = def.Model
				}
				toolsList = subagentTools(*def, subEnv, deferredFn)
			} else {
				toolsList = tools.CoreTools(subEnv, deferredFn, true)
			}

			run := func(runCtx context.Context) (string, error) {
				toolsList = wrapContextAwareTools(toolsList, env.Cwd, discoverRules(env.Cwd))
				cfg, _ := loadConfig()
				res, err := engine.Run(runCtx, engine.RunOptions{
					Prompt:             prompt,
					Cwd:                env.Cwd,
					Config:             toPkgConfig(cfg),
					Provider:           "vertex",
					Model:              modelID,
					Tools:              toolsList,
					SkipAllPermissions: true,
				})
				if err != nil {
					return "", err
				}
				return strings.TrimSpace(res.Response), nil
			}

			if p.RunInBackground {
				label := "agent"
				if p.Agent != "" {
					label = "agent " + p.Agent
				}
				if env.Emit != nil {
					env.Emit(engine.SubagentStartedEvent{
						BaseEvent:   engine.BaseEvent{TabID: env.TabID},
						SubagentID:  subagentID,
						AgentType:   agentType,
						Description: subDesc,
						Background:  true,
					})
				}
				jobCtx, cancel := context.WithCancel(context.Background())
				job := env.Jobs.Add(label+": "+short(prompt), true, cancel)
				go func() {
					report, err := run(jobCtx)
					isErr := false
					switch {
					case err != nil:
						isErr = true
						job.AppendOutput("sub-agent failed: " + err.Error())
						job.Finish(tools.ShellResult{ExitCode: 1})
					case report == "":
						isErr = true
						job.AppendOutput("sub-agent returned no report")
						job.Finish(tools.ShellResult{ExitCode: 1})
					default:
						job.AppendOutput(report)
						job.Finish(tools.ShellResult{ExitCode: 0})
					}
					if env.Emit != nil {
						env.Emit(engine.SubagentEndedEvent{
							BaseEvent:  engine.BaseEvent{TabID: env.TabID},
							SubagentID: subagentID,
							IsError:    isErr,
						})
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
				return tools.NewTextResponse(
					"started background " + label + " as " + job.ID +
						"; poll the report with job_output and stop it with job_kill"), nil
			}

			if env.Emit != nil {
				env.Emit(engine.SubagentStartedEvent{
					BaseEvent:   engine.BaseEvent{TabID: env.TabID},
					SubagentID:  subagentID,
					AgentType:   agentType,
					Description: subDesc,
					Background:  false,
				})
			}
			report, err := run(ctx)
			isErr := err != nil || report == ""
			if env.Emit != nil {
				env.Emit(engine.SubagentEndedEvent{
					BaseEvent:  engine.BaseEvent{TabID: env.TabID},
					SubagentID: subagentID,
					IsError:    isErr,
				})
			}
			if err != nil {
				return tools.NewTextErrorResponse("sub-agent failed: " + err.Error()), nil
			}
			if report == "" {
				return tools.NewTextErrorResponse("sub-agent returned no report"), nil
			}
			return tools.NewTextResponse(report), nil
		},
	)
}
