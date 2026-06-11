package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
)

// agentCompactReserve is the remaining-window threshold that triggers
// auto-summarization: when a step leaves fewer than this many tokens
// of headroom, the turn is stopped, the transcript is compacted into a
// summary head message, and — if the model was mid-tool-loop — a
// continuation turn is queued automatically.
const agentCompactReserve = 20_000

// Loop detection bounds (crush's scheme): a step signature is the
// hash of every (tool, input, result) interaction in the step; if the
// same signature appears more than agentLoopMaxRepeats times within
// the last agentLoopWindow steps, the turn is stopped.
const (
	agentLoopWindow     = 10
	agentLoopMaxRepeats = 5
)

// agentTurn is one queued user submission.
type agentTurn struct {
	text string
}

// agentSession is the in-process replacement for a provider
// subprocess: a goroutine owning a fantasy agent, its tools, and the
// conversation history. It satisfies the providerProc contract with
// cmd=nil — kill() closes stdin, which tears the session down.
type agentSession struct {
	args   ProviderSessionArgs
	model  fantasy.LanguageModel
	env    *agentToolEnv
	tools  []fantasy.AgentTool
	system string

	providerOpts  fantasy.ProviderOptions
	temperature   *float64
	contextWindow int64
	modelID       string

	proc   *providerProc
	ch     chan tea.Msg
	sendCh chan agentTurn

	closed    chan struct{}
	closeOnce sync.Once

	turnMu     sync.Mutex
	turnCancel context.CancelFunc

	sessionID  string
	store      *agentSessionStore
	messages   []fantasy.Message
	mcpClosers []func()
}

// agentStdin adapts the session's shutdown to the providerProc stdin
// contract: killProc closes stdin, which must end the session.
type agentStdin struct{ s *agentSession }

func (w agentStdin) Write(p []byte) (int, error) { return len(p), nil }
func (w agentStdin) Close() error {
	w.s.shutdown()
	return nil
}

// shutdown signals the run goroutine to exit and cancels any in-flight
// turn. Idempotent.
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

// interruptTurn cancels the in-flight turn, if any. Returns whether a
// turn was actually cancelled.
func (s *agentSession) interruptTurn() bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.turnCancel == nil {
		return false
	}
	s.turnCancel()
	return true
}

// emit tags a provider-protocol message with the session's proc and
// pushes it onto the stream channel. Messages are dropped once the
// session is closed so tool goroutines can never wedge on a dead tab.
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
	// Buffer-first: when the channel has room the message always lands,
	// even mid-shutdown (s.closed closed) — a bare two-case select would
	// pick pseudo-randomly and could drop the final providerExitedMsg.
	// Only when the buffer is full do we block, bailing on shutdown so a
	// tool goroutine can never wedge on a dead tab (killProc's drain
	// resolves the block in every live path).
	select {
	case s.ch <- msg:
		return
	default:
	}
	select {
	case s.ch <- msg:
	case <-s.closed:
	}
}

// queueTurn enqueues a user turn for the run loop. Errors when the
// session is shut down.
func (s *agentSession) queueTurn(text string) error {
	select {
	case <-s.closed:
		return errors.New("agent session is closed")
	default:
	}
	select {
	case s.sendCh <- agentTurn{text: text}:
		return nil
	case <-s.closed:
		return errors.New("agent session is closed")
	}
}

// run is the session goroutine: process queued turns until shutdown,
// then clean up jobs/MCP sessions, emit providerExitedMsg, and close
// the stream channel (drainProviderStream relies on that close).
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
			s.env.jobs.killAll()
			for _, closeMCP := range s.mcpClosers {
				closeMCP()
			}
			s.emit(providerExitedMsg{})
			return
		}
	}
}

// runTurn executes one user turn through the fantasy agent loop and
// emits the provider message protocol around it. Always ends with
// providerDoneMsg then turnCompleteMsg (same order as claude's
// stream) so workflow advancement and busy-state work identically.
func (s *agentSession) runTurn(turn agentTurn) {
	ctx, cancel := context.WithCancel(context.Background())
	s.setTurnCancel(cancel)
	defer func() {
		s.setTurnCancel(nil)
		cancel()
	}()

	s.emit(streamStatusMsg{status: "thinking…"})

	shouldCompact := false
	var textBuf strings.Builder
	backgroundCalls := map[string]bool{}

	agent := fantasy.NewAgent(s.model,
		fantasy.WithSystemPrompt(s.system),
		fantasy.WithTools(s.tools...),
		fantasy.WithStopConditions(
			agentLoopDetectionCondition(),
			s.contextPressureCondition(&shouldCompact),
		),
	)

	history := append([]fantasy.Message(nil), s.messages...)
	result, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:          turn.text,
		Messages:        history,
		Temperature:     s.temperature,
		ProviderOptions: s.providerOpts,
		OnReasoningStart: func(string, fantasy.ReasoningContent) error {
			s.emit(streamStatusMsg{status: "thinking…"})
			return nil
		},
		OnTextDelta: func(_, text string) error {
			textBuf.WriteString(text)
			return nil
		},
		OnTextEnd: func(string) error {
			if t := strings.TrimSpace(textBuf.String()); t != "" {
				s.emit(assistantTextMsg{text: t})
			}
			textBuf.Reset()
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			input := map[string]any{}
			_ = json.Unmarshal([]byte(tc.Input), &input)
			background, _ := input["run_in_background"].(bool)
			if background {
				backgroundCalls[tc.ToolCallID] = true
			}
			s.emit(toolCallMsg{
				id:         tc.ToolCallID,
				name:       tc.ToolName,
				input:      input,
				background: background,
			})
			s.emit(streamStatusMsg{status: "running " + tc.ToolName + "…"})
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			s.emit(toolResultMsg{
				toolUseID:  tr.ToolCallID,
				name:       tr.ToolName,
				output:     toolResultText(tr.Result),
				isError:    toolResultIsError(tr.Result),
				background: backgroundCalls[tr.ToolCallID],
			})
			return nil
		},
		OnStepFinish: func(step fantasy.StepResult) error {
			s.emit(usageMsg{tokens: contextTokensFromUsage(step.Usage)})
			s.emit(streamStatusMsg{status: "thinking…"})
			return nil
		},
	})

	switch {
	case err != nil && isAgentCancel(err):
		// User interrupt: no error display, just a clean turn end. The
		// partial turn is not persisted — resuming replays the last
		// complete turn boundary, like a killed claude proc.
		s.emit(providerDoneMsg{res: providerResult{SessionID: s.sessionID}})
		s.emit(turnCompleteMsg{})
		return
	case err != nil:
		s.emit(providerDoneMsg{
			res: providerResult{SessionID: s.sessionID, IsError: true, Result: err.Error()},
			err: err,
		})
		s.emit(turnCompleteMsg{})
		return
	}

	// Fold the turn into history: the user message plus every step's
	// assistant/tool messages, with dangling tool calls repaired so the
	// persisted transcript always satisfies DeepSeek's strict
	// call/result pairing (a loop-detection or compaction stop can land
	// mid-tool-use).
	newMessages := []fantasy.Message{fantasy.NewUserMessage(turn.text)}
	for _, step := range result.Steps {
		newMessages = append(newMessages, step.Messages...)
	}
	newMessages = repairDanglingToolCalls(newMessages)
	s.messages = append(s.messages, newMessages...)
	s.persist()

	resultText := strings.TrimSpace(result.Response.Content.Text())

	if shouldCompact {
		s.compact(ctx, turn, result)
	}

	s.emit(providerDoneMsg{res: providerResult{
		SessionID: s.sessionID,
		Result:    resultText,
	}})
	s.emit(turnCompleteMsg{})
}

// persist writes the current message history through the session
// store. Nil store (tests, ephemeral sessions) is a no-op.
func (s *agentSession) persist() {
	if s.store == nil || s.sessionID == "" {
		return
	}
	if err := s.store.save(s.sessionID, s.args.Cwd, s.messages); err != nil {
		debugLog("agent session persist: %v", err)
	}
}

const agentSummaryPrompt = `Summarize this coding conversation for a fresh context window. Your summary will be the ONLY context available to continue the work, so be thorough and specific.

Required sections:
1. Goal — what the user is trying to accomplish, in their words where possible.
2. Current state — what has been done, which files were created/modified (full paths), what was verified (builds, tests).
3. Key technical context — APIs, types, invariants, decisions, and constraints discovered along the way.
4. Next steps — exactly what remains, with file:line specificity where known.

Write it as a briefing to your successor. Do not omit failing tests, open errors, or unverified assumptions.`

// compact replaces the conversation history with a summary head
// message. When the stopped turn was still mid-tool-loop, a
// continuation turn is queued so the work resumes automatically.
func (s *agentSession) compact(ctx context.Context, turn agentTurn, result *fantasy.AgentResult) {
	s.emit(streamStatusMsg{status: "compacting context…"})
	summarizer := fantasy.NewAgent(s.model, fantasy.WithSystemPrompt(agentSummaryPrompt))
	sum, err := summarizer.Generate(ctx, fantasy.AgentCall{
		Messages: s.messages,
		Prompt:   "Produce the continuation summary now.",
	})
	if err != nil {
		debugLog("agent compact failed: %v", err)
		return
	}
	summary := strings.TrimSpace(sum.Response.Content.Text())
	if summary == "" {
		debugLog("agent compact produced empty summary; keeping full history")
		return
	}
	s.messages = []fantasy.Message{fantasy.NewUserMessage(
		"Context was compacted. Summary of the conversation so far:\n\n" + summary)}
	s.persist()

	interrupted := len(result.Steps) > 0 &&
		result.Steps[len(result.Steps)-1].FinishReason == fantasy.FinishReasonToolCalls
	if interrupted {
		go func() {
			_ = s.queueTurn("The previous turn was interrupted because the context window filled up. " +
				"A summary of progress so far is in your context. Continue working on the original request: " + turn.text)
		}()
	}
}

// agentLoopDetectionCondition stops a turn when the model keeps making
// identical tool calls with identical results.
func agentLoopDetectionCondition() fantasy.StopCondition {
	return func(steps []fantasy.StepResult) bool {
		if len(steps) < agentLoopMaxRepeats {
			return false
		}
		window := steps
		if len(window) > agentLoopWindow {
			window = window[len(window)-agentLoopWindow:]
		}
		counts := map[[32]byte]int{}
		for _, step := range window {
			sig, ok := stepSignature(step)
			if !ok {
				continue
			}
			counts[sig]++
			if counts[sig] > agentLoopMaxRepeats {
				return true
			}
		}
		return false
	}
}

// stepSignature hashes every tool interaction in a step. Steps without
// tool calls never count toward loop detection.
func stepSignature(step fantasy.StepResult) ([32]byte, bool) {
	calls := step.Content.ToolCalls()
	if len(calls) == 0 {
		return [32]byte{}, false
	}
	h := sha256.New()
	for _, c := range calls {
		h.Write([]byte(c.ToolName))
		h.Write([]byte{0})
		h.Write([]byte(c.Input))
		h.Write([]byte{0})
	}
	for _, r := range step.Content.ToolResults() {
		h.Write([]byte(toolResultText(r.Result)))
		h.Write([]byte{0})
	}
	var sig [32]byte
	copy(sig[:], h.Sum(nil))
	return sig, true
}

// contextPressureCondition stops the turn (setting *flag) when the
// last step's usage leaves less than agentCompactReserve headroom.
func (s *agentSession) contextPressureCondition(flag *bool) fantasy.StopCondition {
	return func(steps []fantasy.StepResult) bool {
		if s.contextWindow <= 0 || len(steps) == 0 {
			return false
		}
		if contextTokensFromUsage(steps[len(steps)-1].Usage)+int(steps[len(steps)-1].Usage.OutputTokens) >=
			int(s.contextWindow)-agentCompactReserve {
			*flag = true
			return true
		}
		return false
	}
}

// contextTokensFromUsage derives the prompt-side context footprint:
// fresh input plus cached input, mirroring codexContextTokens'
// definition of "tokens occupying the window".
func contextTokensFromUsage(u fantasy.Usage) int {
	return int(u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens)
}

func toolResultText(out fantasy.ToolResultOutputContent) string {
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

func toolResultIsError(out fantasy.ToolResultOutputContent) bool {
	switch out.(type) {
	case fantasy.ToolResultOutputContentError, *fantasy.ToolResultOutputContentError:
		return true
	}
	return false
}

func isAgentCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// repairDanglingToolCalls appends synthesized error results for any
// assistant tool call that has no matching tool result, so a persisted
// transcript never violates the strict call/result pairing OpenAI-
// compatible APIs (DeepSeek included) enforce on replay.
func repairDanglingToolCalls(msgs []fantasy.Message) []fantasy.Message {
	answered := map[string]bool{}
	for _, m := range msgs {
		for _, part := range m.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				answered[tr.ToolCallID] = true
			}
		}
	}
	var dangling []fantasy.ToolCallPart
	for _, m := range msgs {
		if m.Role != fantasy.MessageRoleAssistant {
			continue
		}
		for _, part := range m.Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && !answered[tc.ToolCallID] {
				dangling = append(dangling, tc)
			}
		}
	}
	if len(dangling) == 0 {
		return msgs
	}
	parts := make([]fantasy.MessagePart, 0, len(dangling))
	for _, tc := range dangling {
		parts = append(parts, fantasy.ToolResultPart{
			ToolCallID: tc.ToolCallID,
			Output: fantasy.ToolResultOutputContentError{
				Error: errors.New("tool call was interrupted before it could run"),
			},
		})
	}
	return append(msgs, fantasy.Message{Role: fantasy.MessageRoleTool, Content: parts})
}
