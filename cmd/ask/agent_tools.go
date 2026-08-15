package main

import (
	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
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
