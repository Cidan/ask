package engine

import (
	"context"
	"errors"
	"strings"
	"sync"

	"charm.land/fantasy"
)

type SessionArgs struct {
	TabID              int
	Cwd                string
	Provider           string
	Model              string
	Effort             string
	InWorkflow         bool
	SkipAllPermissions bool
	SessionID          string
}

type Turn struct {
	Text  string
	Files []fantasy.FilePart
}

type Session struct {
	args          SessionArgs
	model         fantasy.LanguageModel
	system        string
	contextWindow int64
	modelID       string
	tools         []fantasy.AgentTool
	messages      []fantasy.Message
	sendCh        chan Turn
	closed        chan struct{}
	closeOnce     sync.Once
	turnMu        sync.Mutex
	turnCancel    context.CancelFunc
	listener      EventListener
	interaction   InteractionHandler
	sessionID     string
}

func NewSession(args SessionArgs, model fantasy.LanguageModel, system string, tools []fantasy.AgentTool, listener EventListener, interaction InteractionHandler) *Session {
	if interaction == nil {
		interaction = HeadlessInteractionHandler{AutoApproveTools: true}
	}
	s := &Session{
		args:          args,
		model:         model,
		system:        system,
		contextWindow: 200_000,
		modelID:       args.Model,
		tools:         tools,
		sendCh:        make(chan Turn, 8),
		closed:        make(chan struct{}),
		listener:      listener,
		interaction:   interaction,
		sessionID:     args.SessionID,
	}
	if s.sessionID == "" {
		s.sessionID = "ses-" + args.Model
	}
	go s.run()
	return s
}

func (s *Session) Emit(event EngineEvent) {
	if s.listener != nil {
		s.listener(event)
	}
}

func (s *Session) IsBusy() bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return s.turnCancel != nil
}

func (s *Session) InterruptTurn() bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.turnCancel == nil {
		return false
	}
	s.turnCancel()
	return true
}

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.InterruptTurn()
	})
}

func (s *Session) QueueTurn(text string, files ...[]fantasy.FilePart) error {
	turn := Turn{Text: text}
	for _, f := range files {
		turn.Files = append(turn.Files, f...)
	}
	select {
	case <-s.closed:
		return errors.New("session is closed")
	default:
	}
	select {
	case s.sendCh <- turn:
		return nil
	case <-s.closed:
		return errors.New("session is closed")
	}
}

func (s *Session) run() {
	first := true
	for {
		select {
		case turn := <-s.sendCh:
			if first {
				s.Emit(ModelInfoEvent{
					BaseEvent: BaseEvent{TabID: s.args.TabID},
					Model:     s.modelID,
				})
				first = false
			}
			s.runTurn(turn)
		case <-s.closed:
			s.Emit(ExitedEvent{
				BaseEvent: BaseEvent{TabID: s.args.TabID},
			})
			return
		}
	}
}

func (s *Session) runTurn(turn Turn) {
	ctx, cancel := context.WithCancel(context.Background())
	s.turnMu.Lock()
	s.turnCancel = cancel
	s.turnMu.Unlock()

	defer func() {
		s.turnMu.Lock()
		s.turnCancel = nil
		s.turnMu.Unlock()
		cancel()
	}()

	s.Emit(StatusEvent{
		BaseEvent: BaseEvent{TabID: s.args.TabID},
		Status:    "thinking…",
	})

	var textBuf strings.Builder

	agentOpts := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(s.system),
		fantasy.WithTools(s.tools...),
	}
	agent := fantasy.NewAgent(s.model, agentOpts...)

	history := append([]fantasy.Message(nil), s.messages...)
	result, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:   turn.Text,
		Files:    turn.Files,
		Messages: history,
		OnTextDelta: func(_, text string) error {
			textBuf.WriteString(text)
			s.Emit(TextDeltaEvent{
				BaseEvent: BaseEvent{TabID: s.args.TabID},
				Delta:     text,
			})
			return nil
		},
		OnTextEnd: func(string) error {
			if t := strings.TrimSpace(textBuf.String()); t != "" {
				s.Emit(AssistantTextEvent{
					BaseEvent: BaseEvent{TabID: s.args.TabID},
					Text:      t,
				})
			}
			textBuf.Reset()
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			s.Emit(ToolCallEvent{
				BaseEvent: BaseEvent{TabID: s.args.TabID},
				ToolUseID: tc.ToolCallID,
				ToolName:  tc.ToolName,
			})
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			s.Emit(ToolResultEvent{
				BaseEvent: BaseEvent{TabID: s.args.TabID},
				ToolUseID: tr.ToolCallID,
				Output:    ToolResultText(tr.Result),
				IsError:   ToolResultIsError(tr.Result),
			})
			return nil
		},
	})

	if err != nil {
		s.Emit(DoneEvent{
			BaseEvent: BaseEvent{TabID: s.args.TabID},
			Result: ResultSummary{
				SessionID: s.sessionID,
				IsError:   true,
				Result:    err.Error(),
			},
			Error: err,
		})
		s.Emit(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: s.args.TabID}})
		return
	}

	newMessages := []fantasy.Message{fantasy.NewUserMessage(turn.Text, turn.Files...)}
	for _, step := range result.Steps {
		newMessages = append(newMessages, step.Messages...)
	}
	s.messages = append(s.messages, newMessages...)

	resultText := result.Response.Content.Text()

	s.Emit(DoneEvent{
		BaseEvent: BaseEvent{TabID: s.args.TabID},
		Result: ResultSummary{
			SessionID: s.sessionID,
			Result:    resultText,
		},
	})
	s.Emit(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: s.args.TabID}})
}

func ToolResultText(out fantasy.ToolResultOutputContent) string {
	switch v := out.(type) {
	case fantasy.ToolResultOutputContentText:
		return v.Text
	case *fantasy.ToolResultOutputContentText:
		return v.Text
	case fantasy.ToolResultOutputContentError:
		if v.Error != nil {
			return v.Error.Error()
		}
	case *fantasy.ToolResultOutputContentError:
		if v.Error != nil {
			return v.Error.Error()
		}
	case fantasy.ToolResultOutputContentMedia:
		return "(media result: " + v.MediaType + ")"
	case *fantasy.ToolResultOutputContentMedia:
		return "(media result: " + v.MediaType + ")"
	}
	return ""
}

func ToolResultIsError(out fantasy.ToolResultOutputContent) bool {
	switch out.(type) {
	case fantasy.ToolResultOutputContentError:
		return true
	case *fantasy.ToolResultOutputContentError:
		return true
	}
	return false
}
