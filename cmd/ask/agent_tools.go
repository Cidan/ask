package main

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
	"github.com/Cidan/ask/pkg/workflow"
)

type agentToolEnv = tools.ToolEnv
type agentFileTracker = tools.FileTracker
type agentJobManager = tools.JobManager
type shellResult = tools.ShellResult
type shellHandle = tools.ShellHandle

const (
	agentMaxToolOutput  = tools.MaxToolOutput
	agentMaxLineLength  = tools.MaxLineLength
	agentMaxReadLines   = tools.MaxReadLines
	agentMaxReadBytes   = tools.MaxReadBytes
	agentMaxSearchHits  = tools.MaxSearchHits
	agentMaxListEntries = tools.MaxListEntries
)

var agentImageExts = tools.ImageExts

func newAgentToolEnv(cwd string, tabID int, skipPermissions bool, gateTodosBeforeMutate bool, emit func(tea.Msg)) *agentToolEnv {
	var listener engine.EventListener
	if emit != nil {
		listener = func(ev engine.EngineEvent) {
			if msg := EngineEventToTeaMsg(ev); msg != nil {
				emit(msg)
			}
		}
	}
	env := tools.NewToolEnv(cwd, tabID, skipPermissions, gateTodosBeforeMutate, listener, globalTUIInteractionHandler)
	env.WorkflowRunner = func(ctx context.Context, tabID int, def workflow.Def, src any) (string, error) {
		var wfSrc workflowSource
		if s, ok := src.(workflowSource); ok {
			wfSrc = s
		} else if s, ok := src.(workflow.Source); ok {
			wfSrc = s
		}
		defer func() {
			agentSendToProgram(ClearWorkflowStateMsg{TabID: tabID})
		}()
		reply, err := globalCoordinator.RunWorkflow(ctx, tabID, fromPkgWorkflowDef(def), wfSrc)
		if err != nil {
			return "", err
		}
		out := fmt.Sprintf("Workflow %q completed successfully.", reply.workflowName)
		if reply.outcome != "" {
			out += "\nOutcome: " + reply.outcome
		}
		if len(reply.artifacts) > 0 {
			out += "\nArtifacts: " + strings.Join(reply.artifacts, ", ")
		}
		return out, nil
	}
	return env
}

func newAgentJobManager() *agentJobManager { return tools.NewJobManager() }

var agentSendToProgram = func(msg tea.Msg) bool {
	p := teaProgramPtr.Load()
	if p == nil {
		return false
	}
	p.Send(msg)
	return true
}

func truncateMiddle(s string) string { return tools.TruncateMiddle(s) }
func truncateLine(s string) string   { return tools.TruncateLine(s) }
func looksBinary(head []byte) bool   { return tools.LooksBinary(head) }
