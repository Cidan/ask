package engine

import (
	"context"
	"errors"
	"sync"
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
