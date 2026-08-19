package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
	"google.golang.org/genai"
)

// Loop detection bounds
const (
	agentLoopWindow     = 10
	agentLoopMaxRepeats = 5
)

// agentTurn is one queued user submission.
type agentTurn struct {
	text  string
	files []engine.FilePart
}

// agentSession owns a GenAI agent session and the conversation history.
type agentSession struct {
	args          ProviderSessionArgs
	spec          *agentProviderSpec
	client        *genai.Client
	env           *agentToolEnv
	sysMu         sync.RWMutex
	system        string
	callOpts      *genai.GenerateContentConfig
	temperature   *float64
	contextWindow int64
	maxOutputTokens int64
	modelID       string

	coreTools    []tools.Tool
	deferredBase []tools.Tool
	mcp          *mcpManager
	toolsMu      sync.Mutex
	tools        []tools.Tool
	deferred     []tools.Tool

	proc   *providerProc
	ch     chan tea.Msg
	sendCh chan agentTurn

	closed    chan struct{}
	closeOnce sync.Once

	turnMu     sync.Mutex
	turnCancel context.CancelFunc

	sessionID string
	store     *agentSessionStore
	messages  []engine.Message

	retryMaxRetries    int
	retryInitialDelay  time.Duration
	retryBackoffFactor float64
}

func (s *agentSession) refreshToolset() {
	s.toolsMu.Lock()
	toolList := append([]tools.Tool(nil), s.coreTools...)
	deferred := append([]tools.Tool(nil), s.deferredBase...)
	s.toolsMu.Unlock()
	if s.mcp != nil {
		deferred = append(deferred, s.mcp.Tools()...)
	}
	s.toolsMu.Lock()
	s.tools = toolList
	s.deferred = deferred
	s.toolsMu.Unlock()
}

func (s *agentSession) currentTools() []tools.Tool {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	return append([]tools.Tool(nil), s.tools...)
}

func (s *agentSession) deferredTools() []tools.Tool {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	return append([]tools.Tool(nil), s.deferred...)
}

func (s *agentSession) isCoreToolName(name string) bool {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	for _, t := range s.coreTools {
		if t.Info().Name == name {
			return true
		}
	}
	return false
}

type agentStdin struct{ s *agentSession }

func (w agentStdin) Write(p []byte) (int, error) { return len(p), nil }
func (w agentStdin) Close() error {
	w.s.shutdown()
	return nil
}

func (s *agentSession) shutdown() {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.interruptTurn()
	})
}

func (s *agentSession) setTurnCancel(fn context.CancelFunc) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	s.turnCancel = fn
}

func (s *agentSession) stepCost(u TokenUsage) (float64, bool) {
	if s.spec == nil {
		return 0, false
	}
	return stepCostUSD(s.spec.ID, s.modelID, u)
}

func (s *agentSession) interruptTurn() bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.turnCancel == nil {
		return false
	}
	s.turnCancel()
	return true
}

func (s *agentSession) isBusy() bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return s.turnCancel != nil
}

func (s *agentSession) emit(msg tea.Msg) {
	switch m := msg.(type) {
	case streamStatusMsg:
		m.proc = s.proc
		msg = m
	case assistantTextMsg:
		m.proc = s.proc
		msg = m
	case toolCallMsg:
		m.proc = s.proc
		msg = m
	case toolResultMsg:
		m.proc = s.proc
		msg = m
	case toolDiffMsg:
		m.proc = s.proc
		msg = m
	case usageMsg:
		m.proc = s.proc
		msg = m
	case costMsg:
		m.proc = s.proc
		msg = m
	case providerModelMsg:
		m.proc = s.proc
		msg = m
	case todoUpdatedMsg:
		m.proc = s.proc
		msg = m
	case bgTaskStartedMsg:
		m.proc = s.proc
		msg = m
	case bgTaskEndedMsg:
		m.proc = s.proc
		msg = m
	case providerDoneMsg:
		m.proc = s.proc
		msg = m
	case providerExitedMsg:
		m.proc = s.proc
		msg = m
	case turnCompleteMsg:
		m.proc = s.proc
		msg = m
	}
	msg = injectTabID(msg, s.args.TabID)
	select {
	case s.ch <- msg:
	default:
	}
	agentSendToProgram(msg)
}

func (s *agentSession) queueTurn(text string, files ...[]engine.FilePart) error {
	turn := agentTurn{text: text}
	for _, f := range files {
		turn.files = append(turn.files, f...)
	}
	select {
	case <-s.closed:
		return errors.New("agent session is closed")
	default:
	}
	select {
	case s.sendCh <- turn:
		return nil
	case <-s.closed:
		return errors.New("agent session is closed")
	}
}

func (s *agentSession) run() {
	defer close(s.ch)
	first := true
	for {
		select {
		case turn := <-s.sendCh:
			if first {
				s.emit(providerModelMsg{model: s.modelID})
				first = false
			}
			s.runTurn(turn)
		case <-s.closed:
			if s.env != nil && s.env.Jobs != nil {
				s.env.Jobs.KillAll()
			}
			if s.mcp != nil {
				s.mcp.Close()
			}
			s.emit(providerExitedMsg{})
			return
		}
	}
}

func (s *agentSession) runTurn(turn agentTurn) {
	ctx, cancel := context.WithCancel(context.Background())
	s.setTurnCancel(cancel)
	defer func() {
		s.setTurnCancel(nil)
		cancel()
	}()

	s.emit(streamStatusMsg{status: "thinking…"})

	s.sysMu.RLock()
	systemPrompt := s.system
	s.sysMu.RUnlock()

	if expanded, ok := expandSkillInvocation(s.args.Cwd, turn.text); ok {
		turn.text = expanded
	}
	if mem := agentMemoryPromptContext(s.args.Cwd, turn.text); mem != "" {
		turn.text = turn.text + "\n\n" + mem
	}

	activeTools := s.currentTools()
	var functionDecls []*genai.FunctionDeclaration
	toolMap := make(map[string]tools.Tool, len(activeTools))
	for _, t := range activeTools {
		toolMap[t.Info().Name] = t
		if decl := t.Declaration(); decl != nil {
			functionDecls = append(functionDecls, decl)
		}
	}

	genaiConfig := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
	}
	if s.maxOutputTokens > 0 {
		genaiConfig.MaxOutputTokens = int32(s.maxOutputTokens)
	}
	if s.callOpts != nil && s.callOpts.ThinkingConfig != nil {
		genaiConfig.ThinkingConfig = s.callOpts.ThinkingConfig
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

	userMsg := engine.NewUserMessage(turn.text, turn.files...)
	s.messages = append(s.messages, userMsg)
	contents = append(contents, userMsg.ToGenAIContent())

	const maxSteps = 50
	var finalResponseText strings.Builder
	stepSignatures := make([][32]byte, 0)
	displayNames := make(map[string]string)
	backgroundCalls := make(map[string]bool)

	for stepIdx := 0; stepIdx < maxSteps; stepIdx++ {
		stream := engine.GenerateStream(ctx, s.client, s.modelID, contents, genaiConfig)

		var turnTextBuf strings.Builder
		var turnThoughts []engine.ThoughtPart
		var turnToolCalls []engine.ToolCallPart
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
				usage := TokenUsage{
					InputTokens:  int(chunk.UsageMetadata.PromptTokenCount),
					OutputTokens: int(chunk.UsageMetadata.CandidatesTokenCount),
				}
				cost, known := s.stepCost(usage)
				s.emit(usageMsg{
					tokens:    usage.InputTokens + usage.OutputTokens,
					costUSD:   cost,
					costKnown: known,
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
						turnThoughts = append(turnThoughts, engine.ThoughtPart{
							Text:      part.Text,
							Signature: part.ThoughtSignature,
						})
						s.emit(streamStatusMsg{status: "thinking…"})
					} else if part.Text != "" {
						turnTextBuf.WriteString(part.Text)
					}
					if part.FunctionCall != nil {
						sig := part.ThoughtSignature
						if len(sig) == 0 {
							sig = latestThoughtSig
						}
						inputMap := part.FunctionCall.Args
						name := part.FunctionCall.Name
						if name == "invoke_tool" {
							name, inputMap = unwrapInvokeToolCall(inputMap)
						}
						displayNames[part.FunctionCall.Name] = name
						bg, _ := inputMap["run_in_background"].(bool)
						if bg {
							backgroundCalls[part.FunctionCall.Name] = true
						}

						id := ""
						if part.FunctionCall != nil {
							id = part.FunctionCall.ID
						}
						turnToolCalls = append(turnToolCalls, engine.ToolCallPart{
							ID:               id,
							Name:             part.FunctionCall.Name,
							Args:             part.FunctionCall.Args,
							ThoughtSignature: sig,
						})
						s.emit(toolCallMsg{
							name:       name,
							input:      inputMap,
							background: bg,
						})
						status := "running " + name + "…"
						if phrase := toolCallPhrase(inputMap); phrase != "" {
							status = name + ": " + phrase
						}
						s.emit(streamStatusMsg{status: status})
					}
				}
			}
		}

		if streamErr != nil {
			if isAgentCancel(streamErr) {
				s.emit(providerDoneMsg{res: providerResult{SessionID: s.sessionID}})
				s.emit(turnCompleteMsg{})
				return
			}
			s.emit(providerDoneMsg{
				res: providerResult{SessionID: s.sessionID, IsError: true, Result: streamErr.Error()},
				err: streamErr,
			})
			s.emit(turnCompleteMsg{})
			return
		}

		rawText := turnTextBuf.String()
		if rawText != "" {
			if finalResponseText.Len() > 0 {
				finalResponseText.WriteString("\n")
			}
			finalResponseText.WriteString(rawText)
			s.emit(assistantTextMsg{text: rawText})
		}

		if len(turnThoughts) > 0 || len(turnToolCalls) > 0 || rawText != "" {
			assistantMsg := engine.NewAssistantMessage(rawText, turnThoughts, turnToolCalls)
			s.messages = append(s.messages, assistantMsg)
			contents = append(contents, assistantMsg.ToGenAIContent())
		}

		if len(turnToolCalls) == 0 {
			break
		}

		// Loop detection
		sig := stepSignatureFromToolCalls(turnToolCalls)
		stepSignatures = append(stepSignatures, sig)
		if checkLoopDetection(stepSignatures) {
			s.emit(providerDoneMsg{res: providerResult{
				SessionID: s.sessionID,
				IsError:   true,
				Result:    "stopped: repeated identical tool call loop detected",
			}})
			s.emit(turnCompleteMsg{})
			return
		}

		var toolResultParts []engine.ToolResultPart
		var stopTurnRequested bool

		for _, tc := range turnToolCalls {
			tool, ok := toolMap[tc.Name]
			var resp tools.ToolResponse
			var runErr error
			if !ok {
				resp = tools.NewTextErrorResponse(fmt.Sprintf("unknown tool %s", tc.Name))
			} else {
				resp, runErr = tool.Run(ctx, tc.Args)
				if runErr != nil {
					resp = tools.NewTextErrorResponse(fmt.Sprintf("tool %s error: %s", tc.Name, runErr.Error()))
				}
			}

			dispName := tc.Name
			if dn, ok := displayNames[tc.Name]; ok {
				dispName = dn
			}

			s.emit(toolResultMsg{
				name:       dispName,
				output:     resp.Content,
				isError:    resp.IsError,
				background: backgroundCalls[tc.Name],
			})

			toolResultParts = append(toolResultParts, engine.ToolResultPart{
				Name:    tc.Name,
				Content: resp.Content,
				IsError: resp.IsError,
			})

			if resp.StopTurn {
				stopTurnRequested = true
			}
		}

		toolMsg := engine.NewToolResultMessage(toolResultParts...)
		s.messages = append(s.messages, toolMsg)
		contents = append(contents, toolMsg.ToGenAIContent())

		if stopTurnRequested {
			break
		}
	}

	s.persist()

	respText := strings.TrimSpace(finalResponseText.String())
	s.emit(providerDoneMsg{
		res: providerResult{
			SessionID: s.sessionID,
			Result:    respText,
		},
	})
	s.emit(turnCompleteMsg{})
}

func (s *agentSession) persist() {
	if s.store == nil || s.sessionID == "" {
		return
	}
	if err := s.store.save(s.sessionID, s.args.Cwd, s.messages); err != nil {
		debugLog("agent session persist: %v", err)
	}
}

func isAgentCancel(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(err.Error()), "cancel")
}

func stepSignatureFromToolCalls(calls []engine.ToolCallPart) [32]byte {
	h := sha256.New()
	for _, c := range calls {
		h.Write([]byte(c.Name))
		if b, err := json.Marshal(c.Args); err == nil {
			h.Write(b)
		}
	}
	var sig [32]byte
	copy(sig[:], h.Sum(nil))
	return sig
}

func checkLoopDetection(sigs [][32]byte) bool {
	if len(sigs) < agentLoopMaxRepeats {
		return false
	}
	window := sigs
	if len(window) > agentLoopWindow {
		window = window[len(window)-agentLoopWindow:]
	}
	counts := make(map[[32]byte]int)
	for _, sig := range window {
		counts[sig]++
		if counts[sig] >= agentLoopMaxRepeats {
			return true
		}
	}
	return false
}
