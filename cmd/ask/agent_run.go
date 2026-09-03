package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/tools"
	"github.com/Cidan/ask/pkg/workflow"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	adktool "google.golang.org/adk/v2/tool"
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
	args            ProviderSessionArgs
	provider        providers.Provider
	model           adkmodel.LLM
	env             *agentToolEnv
	sysMu           sync.RWMutex
	system          string
	callOpts        *genai.GenerateContentConfig
	temperature     *float64
	contextWindow   int64
	maxOutputTokens int64
	modelID         string

	coreTools    []tools.Tool
	deferredBase []tools.Tool
	mcp          *mcpManager
	toolsMu      sync.Mutex
	tools        []tools.Tool
	deferred     []tools.Tool

	proc   *providerProc
	ch     chan tea.Msg
	sendCh chan agentTurn

	// chMu serializes sends to ch against its close. ch is closed by run()
	// on shutdown, but emit is called from background goroutines (post-turn
	// memory extraction, finished background jobs, subagent tasks) that can
	// outlive the turn. A plain `select { case ch <- msg: default: }` does
	// NOT guard against a closed channel — the default only covers a full
	// one — so an emit racing the close would panic with "send on closed
	// channel". The lock plus chClosed flag makes the two mutually exclusive.
	chMu     sync.Mutex
	chClosed bool

	midTurnQueue *engine.MidTurnQueue

	closed    chan struct{}
	closeOnce sync.Once

	turnMu     sync.Mutex
	turnCancel context.CancelFunc

	sessionID string
	store     *agentSessionStore
	sessSvc   session.Service

	retryMaxRetries    int
	retryInitialDelay  time.Duration
	retryBackoffFactor float64

	// topic is the conversation's memory topic: the recall hook reads
	// it every turn and the hook's inference and the post-turn
	// extraction both update it (tabTopicMsg).
	topicMu sync.Mutex
	topic   string

	// workflowAgent, when set, replaces the ask_coder agent for this
	// session's turns: the session runs a compiled workflow graph
	// instead of a single coder agent. workflowProgress consumes the
	// same ADK event stream to drive the workflow tab's step log.
	workflowAgent    adkagent.Agent
	workflowProgress *workflow.Progress
}

func (s *agentSession) currentTopic() string {
	s.topicMu.Lock()
	defer s.topicMu.Unlock()
	return s.topic
}

// setTopic records a new topic and tells the tab; unchanged or empty
// topics are ignored.
func (s *agentSession) setTopic(topic string) {
	topic = memory.NormalizeTopic(topic)
	if topic == "" {
		return
	}
	s.topicMu.Lock()
	changed := topic != s.topic
	s.topic = topic
	s.topicMu.Unlock()
	if changed {
		s.emit(tabTopicMsg{topic: topic})
	}
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

// reconcileMCP brings this session's live MCP manager in line with the
// current config (a browser toggle or a completed authorization), then
// refreshes the deferred toolset.
func (s *agentSession) reconcileMCP(cfg askConfig) {
	if s.mcp == nil {
		return
	}
	s.mcp.Reconcile(context.Background(), agentSessionMCPServers(s.args, cfg))
	s.refreshToolset()
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
		if t.Name() == name {
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
		if s.model != nil {
			_ = engine.CloseModel(s.model)
		}
	})
}

func (s *agentSession) setTurnCancel(fn context.CancelFunc) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	s.turnCancel = fn
}

func (s *agentSession) stepCost(u TokenUsage) (float64, bool) {
	if s.provider == nil {
		return 0, false
	}
	return stepCostUSD(s.provider.ID(), s.modelID, u)
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
	case providerCwdMsg:
		m.proc = s.proc
		msg = m
	case todoUpdatedMsg:
		m.proc = s.proc
		msg = m
	case subagentStartedMsg:
		m.proc = s.proc
		msg = m
	case subagentEndedMsg:
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
	case queuedMessageDrainedMsg:
		m.proc = s.proc
		msg = m
	case tabTopicMsg:
		m.proc = s.proc
		msg = m
	}
	msg = injectTabID(msg, s.args.TabID)
	s.chMu.Lock()
	if !s.chClosed {
		select {
		case s.ch <- msg:
		default:
		}
	}
	s.chMu.Unlock()
	agentSendToProgram(msg)
}

func (s *agentSession) queueMidTurn(text string) {
	if s.midTurnQueue == nil {
		return
	}
	s.midTurnQueue.Push(text)
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
	defer func() {
		s.chMu.Lock()
		s.chClosed = true
		close(s.ch)
		s.chMu.Unlock()
	}()
	first := true
	for {
		select {
		case turn := <-s.sendCh:
			if first {
				s.emit(providerModelMsg{model: s.modelID})
				first = false
			}
			s.runTurn(turn)
			if s.midTurnQueue != nil {
				msgs := s.midTurnQueue.Drain()
				if len(msgs) > 0 {
					combined := strings.Join(msgs, "\n\n")
					s.emit(queuedMessageDrainedMsg{text: combined})
					go s.queueTurn(combined)
				}
			}
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

type streamToADKModel struct {
	modelID string
	client  *genai.Client
}

func (m *streamToADKModel) Name() string { return m.modelID }

func (m *streamToADKModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for chunk, err := range engine.GenerateStream(ctx, m.client, m.modelID, req.Contents, req.Config) {
			if err != nil {
				yield(nil, err)
				return
			}
			if chunk == nil {
				continue
			}
			var parts []*genai.Part
			for _, c := range chunk.Candidates {
				if c.Content != nil {
					parts = append(parts, c.Content.Parts...)
				}
			}
			resp := &adkmodel.LLMResponse{
				Content: &genai.Content{
					Role:  genai.RoleModel,
					Parts: parts,
				},
				UsageMetadata: chunk.UsageMetadata,
				Partial:       stream,
			}
			if !yield(resp, nil) {
				return
			}
		}
	}
}

func (s *agentSession) beforeModelCallback(ctx adkagent.Context, req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
	if s.midTurnQueue == nil {
		return nil, nil
	}
	msgs := s.midTurnQueue.Drain()
	if len(msgs) == 0 {
		return nil, nil
	}
	combined := strings.Join(msgs, "\n\n")
	req.Contents = append(req.Contents, genai.NewContentFromText(combined, genai.RoleUser))

	if s.sessSvc != nil && s.sessionID != "" {
		if getResp, err := s.sessSvc.Get(ctx, &session.GetRequest{
			AppName:   "ask",
			UserID:    "user",
			SessionID: s.sessionID,
		}); err == nil && getResp.Session != nil {
			s.sessSvc.AppendEvent(ctx, getResp.Session, &session.Event{
				LLMResponse: adkmodel.LLMResponse{
					Content: genai.NewContentFromText(combined, genai.RoleUser),
				},
				Author: "user",
			})
		}
	}

	s.emit(queuedMessageDrainedMsg{text: combined})
	return nil, nil
}

func (s *agentSession) runTurn(turn agentTurn) {
	ctx, cancel := context.WithCancel(context.Background())
	s.setTurnCancel(cancel)
	defer func() {
		s.setTurnCancel(nil)
		cancel()
	}()

	s.emit(streamStatusMsg{status: "thinking…"})

	if expanded, ok := expandSkillInvocation(s.args.Cwd, turn.text); ok {
		turn.text = expanded
	}

	activeTools := s.currentTools()
	adkTools, err := engine.AsADKTools(activeTools)
	if err != nil {
		s.emit(providerDoneMsg{
			res: providerResult{SessionID: s.sessionID, IsError: true, Result: err.Error()},
			err: err,
		})
		s.emit(turnCompleteMsg{})
		return
	}

	genaiConfig := &genai.GenerateContentConfig{}
	if s.maxOutputTokens > 0 {
		genaiConfig.MaxOutputTokens = int32(s.maxOutputTokens)
	}
	if s.callOpts != nil && s.callOpts.ThinkingConfig != nil {
		genaiConfig.ThinkingConfig = s.callOpts.ThinkingConfig
	}

	instructionProvider := engine.BuildInstructionProvider(engine.PromptOptions{
		Cwd:          s.args.Cwd,
		InWorkflow:   s.args.InWorkflow,
		SystemPrompt: s.system,
	})

	llm := s.model
	if llm == nil {
		cfg, _ := loadConfig()
		if s.provider != nil {
			built, err := engine.ModelBuilder(ctx, s.provider, toPkgConfig(cfg), s.modelID)
			if err != nil {
				s.emit(providerDoneMsg{
					res: providerResult{SessionID: s.sessionID, IsError: true, Result: err.Error()},
					err: err,
				})
				s.emit(turnCompleteMsg{})
				return
			}
			llm = built
			s.model = built
		}
	}
	if llm == nil {
		// No provider (tests): the engine.GenerateStream seam stands in
		// for the model. A real session always carries a provider.
		llm = &streamToADKModel{modelID: s.modelID}
	}

	var toolsets []adktool.Toolset
	if s.mcp != nil {
		toolsets = append(toolsets, s.mcp.Toolsets()...)
	}
	if skillTS, err := engine.NewSkillToolset(ctx, s.args.Cwd); err == nil && skillTS != nil {
		toolsets = append(toolsets, skillTS)
	}

	var agentInstance adkagent.Agent
	if s.workflowAgent != nil {
		agentInstance = s.workflowAgent
	} else {
		agentInstance, err = llmagent.New(llmagent.Config{
			Name:                  "ask_coder",
			Model:                 llm,
			InstructionProvider:   instructionProvider,
			Tools:                 adkTools,
			Toolsets:              toolsets,
			GenerateContentConfig: genaiConfig,
			BeforeModelCallbacks: []llmagent.BeforeModelCallback{
				s.beforeModelCallback,
			},
		})
	}
	if err != nil {
		s.emit(providerDoneMsg{
			res: providerResult{SessionID: s.sessionID, IsError: true, Result: err.Error()},
			err: err,
		})
		s.emit(turnCompleteMsg{})
		return
	}

	if s.sessSvc == nil {
		providerID := providers.DefaultProviderID()
		if s.provider != nil {
			providerID = s.provider.ID()
		}
		s.sessSvc = engine.NewFileSessionService(providerID, s.args.Cwd)
	}

	r, err := engine.RunnerBuilder(agentInstance, s.sessSvc)
	if err != nil {
		s.emit(providerDoneMsg{
			res: providerResult{SessionID: s.sessionID, IsError: true, Result: err.Error()},
			err: err,
		})
		s.emit(turnCompleteMsg{})
		return
	}

	adkUserMsg := genai.NewContentFromText(turn.text, genai.RoleUser)
	for _, f := range turn.files {
		if len(f.Data) > 0 {
			adkUserMsg.Parts = append(adkUserMsg.Parts, genai.NewPartFromBytes(f.Data, f.MIMEType))
		}
	}

	var finalResponseText strings.Builder
	var latestThoughtSig []byte
	var touchedFiles []string

	displayNames := make(map[string]string)
	backgroundCalls := make(map[string]bool)

	for event, err := range r.Run(ctx, "user", s.sessionID, adkUserMsg, adkagent.RunConfig{}) {
		if err != nil {
			if isAgentCancel(err) {
				s.emit(providerDoneMsg{res: providerResult{SessionID: s.sessionID}})
				s.emit(turnCompleteMsg{})
				return
			}
			s.emit(providerDoneMsg{
				res: providerResult{SessionID: s.sessionID, IsError: true, Result: err.Error()},
				err: err,
			})
			s.emit(turnCompleteMsg{})
			return
		}
		if event == nil {
			continue
		}
		s.workflowProgress.Observe(event)

		if event.UsageMetadata != nil {
			usage := TokenUsage{
				InputTokens:  int(event.UsageMetadata.PromptTokenCount),
				OutputTokens: int(event.UsageMetadata.CandidatesTokenCount),
			}
			cost, known := s.stepCost(usage)
			s.emit(usageMsg{
				tokens:    usage.InputTokens + usage.OutputTokens,
				costUSD:   cost,
				costKnown: known,
			})
		}

		if event.LLMResponse.Content != nil {
			for _, part := range event.LLMResponse.Content.Parts {
				if part == nil {
					continue
				}
				if part.Thought {
					if len(part.ThoughtSignature) > 0 {
						latestThoughtSig = part.ThoughtSignature
					}
					s.emit(streamStatusMsg{status: "thinking…"})
				} else if part.Text != "" {
					s.emit(assistantTextMsg{text: part.Text})
				}
				if part.FunctionCall != nil {
					inputMap := part.FunctionCall.Args
					name := part.FunctionCall.Name
					if engine.IsConfirmationCall(part.FunctionCall) {
						if origCall, err := engine.UnwrapConfirmationCall(part.FunctionCall); err == nil && origCall != nil {
							name = origCall.Name
							inputMap = origCall.Args
						}
					} else if name == "invoke_tool" {
						name, inputMap = unwrapInvokeToolCall(inputMap)
					}
					displayNames[part.FunctionCall.Name] = name
					touchedFiles = engine.AppendTouchedFile(touchedFiles, name, inputMap)
					bg, _ := inputMap["run_in_background"].(bool)
					if bg {
						backgroundCalls[part.FunctionCall.Name] = true
					}

					s.emit(toolCallMsg{
						name:       name,
						input:      inputMap,
						background: bg,
					})
					status := "Running…"
					if phrase := toolCallPhrase(inputMap); phrase != "" {
						status = capitalizeFirst(phrase)
					}
					s.emit(streamStatusMsg{status: status})
				}
				if part.FunctionResponse != nil {
					resStr, isErr := engine.ToolResultText(part.FunctionResponse.Response)
					dispName := part.FunctionResponse.Name
					if dn, ok := displayNames[part.FunctionResponse.Name]; ok {
						dispName = dn
					}
					exitCode, hasExit := extractExitCode(part.FunctionResponse.Response)
					s.emit(toolResultMsg{
						name:        dispName,
						output:      resStr,
						isError:     isErr,
						exitCode:    exitCode,
						hasExitCode: hasExit,
						background:  backgroundCalls[part.FunctionResponse.Name],
					})
				}
			}

			// Accumulate the turn's final result text one block per
			// non-partial event, joined with a newline — matching the
			// engine's aggregation (session.go) so a preamble and the
			// answer don't scrunch together in providerResult.Result
			// (workflow step capture, the done message).
			if !event.LLMResponse.Partial {
				var blockText strings.Builder
				for _, part := range event.LLMResponse.Content.Parts {
					if part != nil && !part.Thought && part.Text != "" {
						blockText.WriteString(part.Text)
					}
				}
				if txt := blockText.String(); txt != "" {
					if finalResponseText.Len() > 0 {
						finalResponseText.WriteString("\n")
					}
					finalResponseText.WriteString(txt)
				}
			}
		}
	}

	_ = latestThoughtSig

	respText := strings.TrimSpace(finalResponseText.String())
	s.emit(providerDoneMsg{
		res: providerResult{
			SessionID: s.sessionID,
			Result:    respText,
		},
	})
	s.emit(turnCompleteMsg{})
	if !s.args.InWorkflow && s.workflowAgent == nil {
		s.enqueueMemoryTurn(turn.text, respText, touchedFiles)
	}
}

// enqueueMemoryTurn hands the finished turn to the background concept
// extractor. The extraction call's spend lands on the tab's cost meter
// and the topic it settles on becomes the tab's.
func (s *agentSession) enqueueMemoryTurn(prompt, response string, files []string) {
	providerID := providers.DefaultProviderID()
	if s.provider != nil {
		providerID = s.provider.ID()
	}
	engine.EnqueueMemoryTurn(engine.MemoryTurn{
		Cwd:      s.args.Cwd,
		Prompt:   prompt,
		Response: response,
		Topic:    s.currentTopic(),
		Files:    files,
		Provider: providerID,
		OnUsage: func(pid, mid string, in, out int) {
			if cost, known := stepCostUSD(pid, mid, TokenUsage{InputTokens: in, OutputTokens: out}); known {
				s.emit(costMsg{costUSD: cost})
			}
		},
		OnTopic: s.setTopic,
	})
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

func capitalizeFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}
