package engine

import (
	"context"
	"errors"
	"sync"

	"github.com/Cidan/ask/pkg/workflow"
)

// Coordinator manages the background execution of all in-process agent sessions
// without coupling to Bubble Tea or UI models.
type Coordinator struct {
	mu              sync.RWMutex
	sessions        map[int]*Session
	workflowCancels map[int]context.CancelFunc
	interaction     InteractionHandler
	listener        EventListener
}

func NewCoordinator(interaction InteractionHandler, listener EventListener) *Coordinator {
	if interaction == nil {
		interaction = HeadlessInteractionHandler{AutoApproveTools: true}
	}
	return &Coordinator{
		sessions:        make(map[int]*Session),
		workflowCancels: make(map[int]context.CancelFunc),
		interaction:     interaction,
		listener:        listener,
	}
}

func (c *Coordinator) GetSession(tabID int) *Session {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessions[tabID]
}

func (c *Coordinator) HasSession(tabID int) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessions[tabID] != nil
}

func (c *Coordinator) IsBusy(tabID int) bool {
	c.mu.RLock()
	s := c.sessions[tabID]
	c.mu.RUnlock()
	if s == nil {
		return false
	}
	return s.IsBusy()
}

func (c *Coordinator) SetSession(tabID int, s *Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[tabID] = s
}

func (c *Coordinator) RemoveSession(tabID int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, tabID)
}

func (c *Coordinator) CancelWorkflow(tabID int) {
	c.mu.Lock()
	cancel, ok := c.workflowCancels[tabID]
	c.mu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

func (c *Coordinator) InterruptSession(tabID int) bool {
	c.mu.RLock()
	s := c.sessions[tabID]
	c.mu.RUnlock()
	if s == nil {
		return false
	}
	return s.InterruptTurn()
}

func (c *Coordinator) Dispatch(tabID int, text string) error {
	c.mu.RLock()
	s := c.sessions[tabID]
	c.mu.RUnlock()
	if s == nil {
		return errors.New("no active session for tab")
	}
	return s.QueueTurn(text)
}

func (c *Coordinator) ExecuteStep(ctx context.Context, cwd string, tabID int, step workflow.Step, prompt string, isFinal bool) (workflow.StepResult, error) {
	c.mu.RLock()
	s := c.sessions[tabID]
	c.mu.RUnlock()

	if s != nil {
		err := s.QueueTurnSync(ctx, prompt)
		if err != nil {
			return workflow.StepResult{Error: err}, err
		}
		msgs := s.Messages()
		lastResp := s.LastResponse()
		return extractStepResultFromMessages(lastResp, msgs), nil
	}

	runResult, err := Run(ctx, RunOptions{
		Prompt:             prompt,
		Cwd:                cwd,
		Provider:           step.Provider,
		Model:              step.Model,
		EventListener:      c.listener,
		InteractionHandler: c.interaction,
		SkipAllPermissions: true,
	})
	if err != nil {
		return workflow.StepResult{Error: err}, err
	}
	return extractStepResultFromMessages(runResult.Response, runResult.Messages), nil
}

func extractStepResultFromMessages(output string, messages []Message) workflow.StepResult {
	var summary, decision string
	var finishData *workflow.FinishData
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		for _, tc := range msg.ToolCalls {
			if tc.Name == "end_turn" {
				if s, ok := tc.Args["summary"].(string); ok && summary == "" {
					summary = s
				}
				if d, ok := tc.Args["decision"].(string); ok && decision == "" {
					decision = d
				}
			}
			if tc.Name == "exit_loop" {
				if decision == "" {
					decision = workflow.LoopBreak
				}
			}
			if tc.Name == "finish_workflow" && finishData == nil {
				desc, _ := tc.Args["description"].(string)
				var arts []string
				if rawArts, ok := tc.Args["artifacts"].([]any); ok {
					for _, a := range rawArts {
						if s, ok := a.(string); ok {
							arts = append(arts, s)
						}
					}
				} else if strArts, ok := tc.Args["artifacts"].([]string); ok {
					arts = strArts
				}
				finishData = &workflow.FinishData{
					Description: desc,
					Artifacts:   arts,
				}
			}
		}
	}
	return workflow.StepResult{
		Output:     output,
		Summary:    summary,
		Decision:   decision,
		FinishData: finishData,
	}
}
