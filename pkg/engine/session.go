package engine

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
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
	Done  chan struct{}
}

type Session struct {
	args          SessionArgs
	llm           model.LLM
	system        string
	contextWindow int64
	modelID       string
	tools         []Tool
	lastMu        sync.Mutex
	lastResponse  string
	gate          TurnGate
	sendCh        chan Turn
	closed        chan struct{}
	closeOnce     sync.Once
	listener      EventListener
	interaction   InteractionHandler
	sessionID     string
	sessSvc       session.Service
	runner        *runner.Runner
	compactor     *Compactor
}

func NewSession(args SessionArgs, llm model.LLM, system string, tools []Tool, listener EventListener, interaction InteractionHandler) *Session {
	if interaction == nil {
		interaction = HeadlessInteractionHandler{AutoApproveTools: true}
	}
	modelID := args.Model
	contextWindow := int64(1_048_576)
	prov, hasProvider := providers.Get(args.Provider)
	if hasProvider {
		modelID = prov.CanonicalModelID(args.Model, prov.DefaultModel())
		contextWindow = prov.ContextWindow(modelID)
	}
	s := &Session{
		args:          args,
		llm:           llm,
		system:        system,
		contextWindow: contextWindow,
		modelID:       modelID,
		tools:         tools,
		sendCh:        make(chan Turn, 8),
		closed:        make(chan struct{}),
		listener:      listener,
		interaction:   interaction,
		sessionID:     args.SessionID,
	}
	if s.sessionID == "" {
		s.sessionID = "ses-" + modelID
	}

	sessSvc := NewFileSessionService(args.Provider, args.Cwd)
	// The user-facing toggle is read per call in compactBeforeModel so
	// flipping it takes effect without restarting the session.
	compact := CompactOptions{
		ContextWindow: contextWindow,
		Model:         llm,
		Notify: func(r CompactionResult) {
			s.Emit(NewContextCompactedEvent(args.TabID, r))
		},
		OnSummarize: func(_ int, running bool) {
			s.Emit(StatusEvent{BaseEvent: BaseEvent{TabID: args.TabID}, Status: CompactionStatus(running)})
		},
	}
	if path, err := HistoryArchivePath(sessSvc, args.Cwd, s.sessionID); err == nil {
		compact.History = NewHistoryArchive(path)
	}
	if hasProvider {
		compact.Summarizer = ModelChunkSummarizer{
			Provider: prov,
			ModelID:  modelID,
			Effort:   args.Effort,
			OnUsage: func(u providers.Usage) {
				if u.CostKnown() {
					s.Emit(CostEvent{BaseEvent: BaseEvent{TabID: args.TabID}, CostUSD: u.CostUSD})
				}
			},
		}
	}
	s.compactor = NewCompactor(compact)

	genConfig := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(providers.MaxOutputTokensGemini),
	}
	if hasProvider {
		if callOpts, _ := prov.CallOptions(modelID, args.Effort); callOpts != nil {
			if callOpts.ThinkingConfig != nil {
				genConfig.ThinkingConfig = callOpts.ThinkingConfig
			}
			if callOpts.MaxOutputTokens > 0 {
				genConfig.MaxOutputTokens = callOpts.MaxOutputTokens
			}
		}
	}

	adkTools, _ := AsADKTools(tools)
	instructionProvider := BuildInstructionProvider(PromptOptions{
		Cwd:          args.Cwd,
		InWorkflow:   args.InWorkflow,
		SystemPrompt: system,
	})

	agentInstance, err := llmagent.New(llmagent.Config{
		Name:                  "ask_coder",
		Model:                 llm,
		InstructionProvider:   instructionProvider,
		Tools:                 adkTools,
		GenerateContentConfig: genConfig,
		// Order matters only in that the two touch opposite ends: the drain
		// appends steering to the tail, the compactor trims the head.
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			s.beforeModelCallback,
			s.compactBeforeModel,
		},
		AfterModelCallbacks: []llmagent.AfterModelCallback{s.compactor.AfterModel},
	})
	if err == nil {
		s.sessSvc = sessSvc
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

// IsBusy reports whether a turn is running and taking input. A turn that was
// interrupted is not, though it may still be unwinding.
func (s *Session) IsBusy() bool {
	return s.gate.Busy()
}

func (s *Session) InterruptTurn() bool {
	return s.gate.Interrupt()
}

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.InterruptTurn()
		if s.llm != nil {
			_ = CloseModel(s.llm)
		}
		_ = s.compactor.Close()
	})
}

// Submit hands the session a message from the user: it steers the turn that
// is running, taken by that turn's next model call, or — when no turn is
// taking input — runs as a turn of its own. Either way it is never lost
// between the two.
func (s *Session) Submit(text string, files ...FilePart) error {
	if s.gate.Steer(TurnInput{Text: text, Files: files}) {
		return nil
	}
	return s.QueueTurn(text, files)
}

func (s *Session) beforeModelCallback(ctx agent.Context, llmRequest *model.LLMRequest) (*model.LLMResponse, error) {
	taken := s.gate.Take(ctx)
	if len(taken) == 0 {
		return nil, nil
	}
	in := JoinTurnInputs(taken)
	llmRequest.Contents = append(llmRequest.Contents, in.Content())

	if s.sessSvc != nil && s.sessionID != "" {
		if getResp, err := s.sessSvc.Get(ctx, &session.GetRequest{
			AppName:   "ask",
			UserID:    "user",
			SessionID: s.sessionID,
		}); err == nil && getResp.Session != nil {
			s.sessSvc.AppendEvent(ctx, getResp.Session, &session.Event{
				LLMResponse: model.LLMResponse{
					Content: in.Content(),
				},
				Author: "user",
			})
		}
	}

	s.Emit(MidTurnDrainedEvent{
		BaseEvent: BaseEvent{TabID: s.args.TabID},
		Text:      in.Text,
	})

	return nil, nil
}

// compactBeforeModel keeps the outgoing request inside the model's context
// window. It runs after beforeModelCallback: the drain appends steering to
// the end of req.Contents and compaction trims the front, so the two never
// contend, and draining first means a message queued this instant is measured
// by the cut rather than smuggled past it.
func (s *Session) compactBeforeModel(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	cfg, _ := config.Load()
	if !AutoCompactEnabled(cfg) {
		s.compactor.Reset()
		return nil, nil
	}
	return s.compactor.BeforeModel(ctx, req)
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

// QueueTurnSync sends a turn to the session and blocks until turn execution is completed.
func (s *Session) QueueTurnSync(ctx context.Context, text string, files ...[]FilePart) error {
	done := make(chan struct{})
	turn := Turn{Text: text, Done: done}
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
	case <-s.closed:
		return errors.New("session is closed")
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.InterruptTurn()
		return ctx.Err()
	case <-s.closed:
		return errors.New("session is closed")
	}
}

func (s *Session) SessionID() string {
	return s.sessionID
}

func (s *Session) Messages() []Message {
	if s.sessSvc != nil && s.sessionID != "" {
		if getResp, err := s.sessSvc.Get(context.Background(), &session.GetRequest{
			AppName:   "ask",
			UserID:    "user",
			SessionID: s.sessionID,
		}); err == nil && getResp.Session != nil {
			var events []*session.Event
			for e := range getResp.Session.Events().All() {
				events = append(events, e)
			}
			return MessagesFromEvents(events)
		}
	}
	return nil
}

func (s *Session) LastResponse() string {
	s.lastMu.Lock()
	defer s.lastMu.Unlock()
	return s.lastResponse
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
			for {
				s.runTurn(turn)
				left, ok := s.gate.End()
				if turn.Done != nil {
					close(turn.Done)
				}
				if !ok {
					break
				}
				// What the user sent that the turn never took runs next,
				// ahead of anything queued behind it.
				s.Emit(MidTurnDrainedEvent{
					BaseEvent: BaseEvent{TabID: s.args.TabID},
					Text:      left.Text,
				})
				turn = Turn{Text: left.Text, Files: left.Files}
			}
		case <-s.closed:
			s.Emit(ExitedEvent{
				BaseEvent: BaseEvent{TabID: s.args.TabID},
			})
			return
		}
	}
}

// closeTurn leaves the model and the history as a turn that is over. A model
// that runs a turn of its own is told it ended; and when this one stopped
// before it finished — interrupted, or failed — every tool call it left
// without a result is answered as interrupted, so the next request is one
// every provider accepts.
func (s *Session) closeTurn(invocationID string, finished bool) {
	providers.CloseTurn(s.llm)
	if finished {
		return
	}
	responses, err := CloseInterruptedCalls(context.Background(), s.sessSvc, s.sessionID, invocationID)
	if err != nil {
		DebugLog("close interrupted calls %s: %v", s.sessionID, err)
	}
	for _, fr := range responses {
		s.Emit(ToolResultEvent{
			BaseEvent: BaseEvent{TabID: s.args.TabID},
			ToolUseID: fr.ID,
			ToolName:  fr.Name,
			Output:    ToolInterruptedError,
			IsError:   true,
		})
	}
}

func (s *Session) runTurn(turn Turn) {
	ctx, cancel := context.WithCancel(context.Background())
	s.gate.Begin(cancel)
	defer cancel()

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

	userMsg := TurnInput{Text: turn.Text, Files: turn.Files}.Content()

	var finalResponseText strings.Builder
	invocationID := ""

	for event, err := range s.runner.Run(ctx, "user", s.sessionID, userMsg, agent.RunConfig{}) {
		if err != nil {
			// A cancelled turn is not always reported as context.Canceled:
			// ADK can end it with a panic it recovered from its own node.
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			s.closeTurn(invocationID, false)
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
		if event.InvocationID != "" {
			invocationID = event.InvocationID
		}

		if u, ok := ResponseUsage(&event.LLMResponse); ok && !event.Partial {
			s.Emit(NewUsageEvent(s.args.TabID, u))
		}

		if event.LLMResponse.Content != nil {
			for _, part := range event.LLMResponse.Content.Parts {
				if part == nil {
					continue
				}
				if part.Thought {
					// Thought processed
				} else if part.Text != "" {
					if event.LLMResponse.Partial {
						s.Emit(TextDeltaEvent{
							BaseEvent: BaseEvent{TabID: s.args.TabID},
							Delta:     part.Text,
						})
					}
				}
				if part.FunctionCall != nil {
					s.Emit(ToolCallEvent{
						BaseEvent: BaseEvent{TabID: s.args.TabID},
						ToolName:  part.FunctionCall.Name,
						Input:     part.FunctionCall.Args,
					})
				}
				if part.FunctionResponse != nil {
					resStr, isErr := ToolResultText(part.FunctionResponse.Response)
					s.Emit(ToolResultEvent{
						BaseEvent: BaseEvent{TabID: s.args.TabID},
						ToolName:  part.FunctionResponse.Name,
						Output:    resStr,
						IsError:   isErr,
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
		}
	}

	respText := finalResponseText.String()
	s.lastMu.Lock()
	s.lastResponse = respText
	s.lastMu.Unlock()
	s.closeTurn(invocationID, ctx.Err() == nil)
	s.Emit(DoneEvent{
		BaseEvent: BaseEvent{TabID: s.args.TabID},
		Result: ResultSummary{
			SessionID: s.sessionID,
			Result:    respText,
		},
	})
	s.Emit(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: s.args.TabID}})
}
