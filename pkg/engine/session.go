package engine

import (
	"context"
	"errors"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
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
	Files []FilePart
}

type Session struct {
	args          SessionArgs
	llm           model.LLM
	system        string
	contextWindow int64
	modelID       string
	tools         []Tool
	messages      []Message
	sendCh        chan Turn
	closed        chan struct{}
	closeOnce     sync.Once
	turnMu        sync.Mutex
	turnCancel    context.CancelFunc
	listener      EventListener
	interaction   InteractionHandler
	sessionID     string
	sessSvc       session.Service
	runner        *runner.Runner
}

func NewSession(args SessionArgs, llm model.LLM, system string, tools []Tool, listener EventListener, interaction InteractionHandler) *Session {
	if interaction == nil {
		interaction = HeadlessInteractionHandler{AutoApproveTools: true}
	}
	s := &Session{
		args:          args,
		llm:           llm,
		system:        system,
		contextWindow: 1_048_576,
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

	adkTools, _ := AsADKTools(tools)
	agentInstance, err := llmagent.New(llmagent.Config{
		Name:        "ask_coder",
		Model:       llm,
		Instruction: system,
		Tools:       adkTools,
	})
	if err == nil {
		s.sessSvc = session.InMemoryService()
		_, _ = s.sessSvc.Create(context.Background(), &session.CreateRequest{
			AppName:   "ask",
			UserID:    "user",
			SessionID: s.sessionID,
		})
		s.runner, _ = RunnerBuilder(agentInstance, s.sessSvc)
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

func (s *Session) QueueTurn(text string, files ...[]FilePart) error {
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

	if s.runner == nil {
		s.Emit(DoneEvent{
			BaseEvent: BaseEvent{TabID: s.args.TabID},
			Result: ResultSummary{
				SessionID: s.sessionID,
				IsError:   true,
				Result:    "runner is nil",
			},
			Error: errors.New("runner is nil"),
		})
		s.Emit(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: s.args.TabID}})
		return
	}

	userMsg := genai.NewContentFromText(turn.Text, genai.RoleUser)
	for _, f := range turn.Files {
		if len(f.Data) > 0 {
			userMsg.Parts = append(userMsg.Parts, genai.NewPartFromBytes(f.Data, f.MIMEType))
		}
	}

	userMessageObj := NewUserMessage(turn.Text, turn.Files...)
	s.messages = append(s.messages, userMessageObj)

	var finalResponseText strings.Builder
	var currentTurnThoughts []ThoughtPart
	var currentTurnToolCalls []ToolCallPart
	var currentTurnToolResults []ToolResultPart

	for event, err := range s.runner.Run(ctx, "user", s.sessionID, userMsg, agent.RunConfig{}) {
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
		if event == nil {
			continue
		}

		if event.UsageMetadata != nil {
			s.Emit(UsageEvent{
				BaseEvent:    BaseEvent{TabID: s.args.TabID},
				InputTokens:  int(event.UsageMetadata.PromptTokenCount),
				OutputTokens: int(event.UsageMetadata.CandidatesTokenCount),
				TotalTokens:  int(event.UsageMetadata.TotalTokenCount),
			})
		}

		if event.LLMResponse.Content != nil {
			for _, part := range event.LLMResponse.Content.Parts {
				if part == nil {
					continue
				}
				if part.Thought {
					currentTurnThoughts = append(currentTurnThoughts, ThoughtPart{
						Text:      part.Text,
						Signature: part.ThoughtSignature,
					})
				} else if part.Text != "" {
					if event.LLMResponse.Partial {
						s.Emit(TextDeltaEvent{
							BaseEvent: BaseEvent{TabID: s.args.TabID},
							Delta:     part.Text,
						})
					}
				}
				if part.FunctionCall != nil {
					tc := ToolCallPart{
						ID:               part.FunctionCall.ID,
						Name:             part.FunctionCall.Name,
						Args:             part.FunctionCall.Args,
						ThoughtSignature: part.ThoughtSignature,
					}
					currentTurnToolCalls = append(currentTurnToolCalls, tc)
					s.Emit(ToolCallEvent{
						BaseEvent: BaseEvent{TabID: s.args.TabID},
						ToolName:  tc.Name,
						Input:     tc.Args,
					})
				}
				if part.FunctionResponse != nil {
					resStr := ""
					isErr := false
					if res, ok := part.FunctionResponse.Response["result"].(string); ok {
						resStr = res
					}
					if errFlag, ok := part.FunctionResponse.Response["is_error"].(bool); ok {
						isErr = errFlag
					}
					tr := ToolResultPart{
						ID:      part.FunctionResponse.ID,
						Name:    part.FunctionResponse.Name,
						Content: resStr,
						IsError: isErr,
					}
					currentTurnToolResults = append(currentTurnToolResults, tr)
					s.Emit(ToolResultEvent{
						BaseEvent: BaseEvent{TabID: s.args.TabID},
						ToolName:  tr.Name,
						Output:    tr.Content,
						IsError:   tr.IsError,
					})
				}
			}
		}

		if !event.LLMResponse.Partial && event.LLMResponse.Content != nil {
			var nonPartialText strings.Builder
			for _, part := range event.LLMResponse.Content.Parts {
				if part != nil && !part.Thought && part.Text != "" {
					nonPartialText.WriteString(part.Text)
				}
			}
			txt := nonPartialText.String()
			if txt != "" {
				if finalResponseText.Len() > 0 {
					finalResponseText.WriteString("\n")
				}
				finalResponseText.WriteString(txt)
				s.Emit(AssistantTextEvent{
					BaseEvent: BaseEvent{TabID: s.args.TabID},
					Text:      txt,
				})
			}
			if len(currentTurnThoughts) > 0 || len(currentTurnToolCalls) > 0 || txt != "" {
				s.messages = append(s.messages, NewAssistantMessage(txt, currentTurnThoughts, currentTurnToolCalls))
				currentTurnThoughts = nil
				currentTurnToolCalls = nil
			}
			if len(currentTurnToolResults) > 0 {
				s.messages = append(s.messages, NewToolResultMessage(currentTurnToolResults...))
				currentTurnToolResults = nil
			}
		}
	}

	respText := finalResponseText.String()
	s.Emit(DoneEvent{
		BaseEvent: BaseEvent{TabID: s.args.TabID},
		Result: ResultSummary{
			SessionID: s.sessionID,
			Result:    respText,
		},
	})
	s.Emit(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: s.args.TabID}})
}
