package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

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
	client        *genai.Client
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
}

func NewSession(args SessionArgs, client *genai.Client, system string, tools []Tool, listener EventListener, interaction InteractionHandler) *Session {
	if interaction == nil {
		interaction = HeadlessInteractionHandler{AutoApproveTools: true}
	}
	s := &Session{
		args:          args,
		client:        client,
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

	var functionDecls []*genai.FunctionDeclaration
	toolMap := make(map[string]Tool, len(s.tools))
	for _, t := range s.tools {
		toolMap[t.Info().Name] = t
		if decl := t.Declaration(); decl != nil {
			functionDecls = append(functionDecls, decl)
		}
	}

	genaiConfig := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(s.system, genai.RoleUser),
	}
	if len(functionDecls) > 0 {
		genaiConfig.Tools = []*genai.Tool{
			{FunctionDeclarations: functionDecls},
		}
	}

	var contents []*genai.Content
	for _, msg := range s.messages {
		contents = append(contents, msg.ToGenAIContent())
	}

	userMsg := NewUserMessage(turn.Text, turn.Files...)
	s.messages = append(s.messages, userMsg)
	contents = append(contents, userMsg.ToGenAIContent())

	const maxTurns = 50
	var finalResponseText strings.Builder

	for turnIdx := 0; turnIdx < maxTurns; turnIdx++ {
		stream := GenerateStream(ctx, s.client, s.modelID, contents, genaiConfig)

		var turnTextBuf strings.Builder
		var turnThoughts []ThoughtPart
		var turnToolCalls []ToolCallPart
		var latestThoughtSig []byte
		var streamErr error

		for chunk, err := range stream {
			if err != nil {
				streamErr = err
				break
			}
			if chunk == nil {
				continue
			}

			if chunk.UsageMetadata != nil {
				s.Emit(UsageEvent{
					BaseEvent:    BaseEvent{TabID: s.args.TabID},
					InputTokens:  int(chunk.UsageMetadata.PromptTokenCount),
					OutputTokens: int(chunk.UsageMetadata.CandidatesTokenCount),
					TotalTokens:  int(chunk.UsageMetadata.TotalTokenCount),
				})
			}

			for _, candidate := range chunk.Candidates {
				if candidate.Content == nil {
					continue
				}
				for _, part := range candidate.Content.Parts {
					if part.Thought {
						if len(part.ThoughtSignature) > 0 {
							latestThoughtSig = part.ThoughtSignature
						}
						turnThoughts = append(turnThoughts, ThoughtPart{
							Text:      part.Text,
							Signature: part.ThoughtSignature,
						})
					} else if part.Text != "" {
						turnTextBuf.WriteString(part.Text)
						s.Emit(TextDeltaEvent{
							BaseEvent: BaseEvent{TabID: s.args.TabID},
							Delta:     part.Text,
						})
					}
					if part.FunctionCall != nil {
						sig := part.ThoughtSignature
						if len(sig) == 0 {
							sig = latestThoughtSig
						}
						id := ""
						if part.FunctionCall != nil {
							id = part.FunctionCall.ID
						}
						turnToolCalls = append(turnToolCalls, ToolCallPart{
							ID:               id,
							Name:             part.FunctionCall.Name,
							Args:             part.FunctionCall.Args,
							ThoughtSignature: sig,
						})
						s.Emit(ToolCallEvent{
							BaseEvent: BaseEvent{TabID: s.args.TabID},
							ToolName:  part.FunctionCall.Name,
							Input:     part.FunctionCall.Args,
						})
					}
				}
			}
		}

		if streamErr != nil {
			s.Emit(DoneEvent{
				BaseEvent: BaseEvent{TabID: s.args.TabID},
				Result: ResultSummary{
					SessionID: s.sessionID,
					IsError:   true,
					Result:    streamErr.Error(),
				},
				Error: streamErr,
			})
			s.Emit(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: s.args.TabID}})
			return
		}

		rawText := turnTextBuf.String()
		if rawText != "" {
			if finalResponseText.Len() > 0 {
				finalResponseText.WriteString("\n")
			}
			finalResponseText.WriteString(rawText)
			s.Emit(AssistantTextEvent{
				BaseEvent: BaseEvent{TabID: s.args.TabID},
				Text:      rawText,
			})
		}

		assistantMsg := NewAssistantMessage(rawText, turnThoughts, turnToolCalls)
		s.messages = append(s.messages, assistantMsg)
		contents = append(contents, assistantMsg.ToGenAIContent())

		if len(turnToolCalls) == 0 {
			break
		}

		var toolResultParts []ToolResultPart
		var stopTurnRequested bool

		for _, tc := range turnToolCalls {
			tool, ok := toolMap[tc.Name]
			var resp ToolResponse
			var runErr error
			if !ok {
				resp = NewTextErrorResponse(fmt.Sprintf("unknown tool %s", tc.Name))
			} else {
				resp, runErr = tool.Run(ctx, tc.Args)
				if runErr != nil {
					resp = NewTextErrorResponse(fmt.Sprintf("tool %s error: %s", tc.Name, runErr.Error()))
				}
			}

			s.Emit(ToolResultEvent{
				BaseEvent: BaseEvent{TabID: s.args.TabID},
				ToolName:  tc.Name,
				Output:    resp.Content,
				IsError:   resp.IsError,
			})

			toolResultParts = append(toolResultParts, ToolResultPart{
				Name:    tc.Name,
				Content: resp.Content,
				IsError: resp.IsError,
			})

			if resp.StopTurn {
				stopTurnRequested = true
			}
		}

		toolMsg := NewToolResultMessage(toolResultParts...)
		s.messages = append(s.messages, toolMsg)
		contents = append(contents, toolMsg.ToGenAIContent())

		if stopTurnRequested {
			break
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
