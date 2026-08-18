package engine

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/google/uuid"
	"google.golang.org/genai"
)

// RunOptions defines the input parameters for executing an ask agent turn.
type RunOptions struct {
	// Prompt is the user query, instruction, or task description.
	Prompt string `json:"prompt"`

	// SessionID is the unique session identifier. If empty, a new session is created.
	// If provided, prior conversation turns and tool calls are loaded from disk.
	SessionID string `json:"session_id,omitempty"`

	// Cwd is the target working directory. Defaults to os.Getwd().
	Cwd string `json:"cwd,omitempty"`

	// Config optionally overrides default configuration (~/.config/ask/ask.json).
	Config config.Config `json:"config,omitempty"`

	// Provider optionally overrides the LLM provider (e.g. "vertex").
	Provider string `json:"provider,omitempty"`

	// Model optionally overrides the default model for the selected provider.
	Model string `json:"model,omitempty"`

	// Effort optionally sets reasoning/thinking effort level.
	Effort string `json:"effort,omitempty"`

	// Files provides optional image/media attachments for models with vision.
	Files []FilePart `json:"files,omitempty"`

	// Tools optionally overrides or augments the default toolset.
	Tools []Tool `json:"-"`

	// EventListener receives real-time streaming deltas, tool calls, and lifecycle events.
	EventListener EventListener `json:"-"`

	// InteractionHandler manages tool approval prompts and user questions.
	// Defaults to HeadlessInteractionHandler{AutoApproveTools: true}.
	InteractionHandler InteractionHandler `json:"-"`

	// SkipAllPermissions bypasses confirmation prompts for all tools.
	SkipAllPermissions bool `json:"skip_all_permissions,omitempty"`
}

// RunResult contains the outcome of the agent turn.
type RunResult struct {
	// SessionID is the session identifier used for this turn (persisted on disk).
	SessionID string `json:"session_id"`

	// Response is the final assistant text output.
	Response string `json:"response"`

	// Messages contains the complete message history up to this point.
	Messages []Message `json:"messages"`

	// IsError indicates whether the turn failed.
	IsError bool `json:"is_error"`

	// Error contains the failure error, if any.
	Error error `json:"error,omitempty"`
}

// ToolFactoryArgs provides configuration parameters to construct the agent toolset.
type ToolFactoryArgs struct {
	Cwd                   string
	TabID                 int
	SkipPermissions       bool
	GateTodosBeforeMutate bool
	EventListener         EventListener
	InteractionHandler    InteractionHandler
	AttachWebSearch       bool
}

// ToolFactory builds a slice of Tools for an engine turn.
type ToolFactory func(args ToolFactoryArgs) []Tool

var (
	toolFactoryMu      sync.RWMutex
	defaultToolFactory ToolFactory
)

// RegisterToolFactory registers the global default tool factory.
func RegisterToolFactory(factory ToolFactory) {
	toolFactoryMu.Lock()
	defer toolFactoryMu.Unlock()
	defaultToolFactory = factory
}

// GetDefaultToolFactory retrieves the currently registered default tool factory.
func GetDefaultToolFactory() ToolFactory {
	toolFactoryMu.RLock()
	defer toolFactoryMu.RUnlock()
	return defaultToolFactory
}

// ClientBuilder allows customizing or mocking GenAI client instantiation in tests.
var ClientBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config) (*genai.Client, error) {
	if spec == nil {
		return nil, errors.New("provider spec is nil")
	}
	if spec.BuildClient == nil {
		return nil, fmt.Errorf("provider %s has no BuildClient implementation", spec.ID)
	}
	return spec.BuildClient(cfg)
}

// GenerateStreamFunc defines the signature for streaming content generation from GenAI.
type GenerateStreamFunc func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error]

// GenerateStream is the streaming generation hook, swappable in tests.
var GenerateStream GenerateStreamFunc = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	if client == nil {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(nil, errors.New("genai client is nil"))
		}
	}
	return client.Models.GenerateContentStream(ctx, model, contents, config)
}

// Run executes an ask agent turn with the provided options using default engine settings.
func Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	eng := New(Options{
		Config:             opts.Config,
		InteractionHandler: opts.InteractionHandler,
		EventListener:      opts.EventListener,
	})
	return eng.Run(ctx, opts)
}

// Run executes an ask agent turn on the Engine instance.
func (e *Engine) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	if opts.Cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.Cwd = wd
		} else {
			opts.Cwd = "."
		}
	}

	if opts.Config.Provider == "" {
		if loadedCfg, err := config.Load(); err == nil && loadedCfg.Provider != "" {
			opts.Config = loadedCfg
		}
	}

	if opts.InteractionHandler == nil {
		if e.opts.InteractionHandler != nil {
			opts.InteractionHandler = e.opts.InteractionHandler
		} else {
			opts.InteractionHandler = HeadlessInteractionHandler{
				AutoApproveTools: opts.SkipAllPermissions,
			}
		}
	}

	if opts.EventListener == nil {
		opts.EventListener = e.opts.EventListener
	}

	providerID := strings.TrimSpace(opts.Provider)
	if providerID == "" {
		providerID = strings.TrimSpace(opts.Config.Provider)
	}
	if providerID == "" {
		providerID = "vertex"
	}

	spec, ok := providers.GetAgentProviderSpec(providerID)
	if !ok {
		return nil, fmt.Errorf("unsupported provider %q", providerID)
	}

	settings := spec.LoadSettings(opts.Config)
	modelID := strings.TrimSpace(opts.Model)
	if modelID == "" {
		modelID = strings.TrimSpace(settings.Model)
	}
	if modelID == "" {
		modelID = spec.DefaultModel
	}

	effort := strings.TrimSpace(opts.Effort)
	if effort == "" {
		effort = strings.TrimSpace(settings.Effort)
	}
	if effort == "" {
		effort = "medium"
	}

	sessionID := strings.TrimSpace(opts.SessionID)
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	store := NewSessionStore(providerID)
	var messages []Message
	if opts.SessionID != "" {
		if file, err := store.Load(sessionID); err == nil {
			messages = file.Messages
		}
	}

	client, err := ClientBuilder(spec, opts.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to build client for provider %s: %w", providerID, err)
	}

	var agentTools []Tool
	if len(opts.Tools) > 0 {
		agentTools = append(agentTools, opts.Tools...)
	} else if tf := GetDefaultToolFactory(); tf != nil {
		agentTools = tf(ToolFactoryArgs{
			Cwd:                   opts.Cwd,
			TabID:                 0,
			SkipPermissions:       opts.SkipAllPermissions,
			GateTodosBeforeMutate: opts.Config.UI.GateTodosBeforeMutate != nil && *opts.Config.UI.GateTodosBeforeMutate,
			EventListener:         opts.EventListener,
			InteractionHandler:    opts.InteractionHandler,
			AttachWebSearch:       true,
		})
	}

	systemPrompt := BuildSystemPrompt(PromptOptions{
		Cwd:        opts.Cwd,
		InWorkflow: false,
	})

	genaiConfig := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
	}
	if spec.CallOptions != nil {
		if callOpts, _ := spec.CallOptions(modelID, effort); callOpts != nil {
			if callOpts.ThinkingConfig != nil {
				genaiConfig.ThinkingConfig = callOpts.ThinkingConfig
			}
		}
	}

	var functionDecls []*genai.FunctionDeclaration
	toolMap := make(map[string]Tool, len(agentTools))
	for _, t := range agentTools {
		toolMap[t.Info().Name] = t
		if decl := t.Declaration(); decl != nil {
			functionDecls = append(functionDecls, decl)
		}
	}
	if len(functionDecls) > 0 {
		genaiConfig.Tools = []*genai.Tool{
			{FunctionDeclarations: functionDecls},
		}
	}

	if opts.EventListener != nil {
		opts.EventListener(ModelInfoEvent{
			BaseEvent: BaseEvent{TabID: 0},
			Model:     modelID,
		})
		opts.EventListener(StatusEvent{
			BaseEvent: BaseEvent{TabID: 0},
			Status:    "thinking…",
		})
	}

	// Prepare history contents
	var contents []*genai.Content
	for _, msg := range messages {
		contents = append(contents, msg.ToGenAIContent())
	}

	userMsg := NewUserMessage(opts.Prompt, opts.Files...)
	contents = append(contents, userMsg.ToGenAIContent())
	messages = append(messages, userMsg)

	const maxTurns = 50
	var finalResponseText strings.Builder

	for turnIdx := 0; turnIdx < maxTurns; turnIdx++ {
		stream := GenerateStream(ctx, client, modelID, contents, genaiConfig)

		var turnTextBuf strings.Builder
		var turnThoughts []ThoughtPart
		var turnToolCalls []ToolCallPart
		var streamErr error

		for chunk, err := range stream {
			if err != nil {
				streamErr = err
				break
			}
			if chunk == nil {
				continue
			}

			if chunk.UsageMetadata != nil && opts.EventListener != nil {
				opts.EventListener(UsageEvent{
					BaseEvent:    BaseEvent{TabID: 0},
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
						turnThoughts = append(turnThoughts, ThoughtPart{
							Text:      part.Text,
							Signature: part.ThoughtSignature,
						})
					} else if part.Text != "" {
						turnTextBuf.WriteString(part.Text)
						if opts.EventListener != nil {
							opts.EventListener(TextDeltaEvent{
								BaseEvent: BaseEvent{TabID: 0},
								Delta:     part.Text,
							})
						}
					}
					if part.FunctionCall != nil {
						turnToolCalls = append(turnToolCalls, ToolCallPart{
							Name: part.FunctionCall.Name,
							Args: part.FunctionCall.Args,
						})
						if opts.EventListener != nil {
							opts.EventListener(ToolCallEvent{
								BaseEvent: BaseEvent{TabID: 0},
								ToolName:  part.FunctionCall.Name,
								Input:     part.FunctionCall.Args,
							})
						}
					}
				}
			}
		}

		if streamErr != nil {
			if opts.EventListener != nil {
				opts.EventListener(DoneEvent{
					BaseEvent: BaseEvent{TabID: 0},
					Result: ResultSummary{
						SessionID: sessionID,
						IsError:   true,
						Result:    streamErr.Error(),
					},
					Error: streamErr,
				})
				opts.EventListener(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: 0}})
			}
			return &RunResult{
				SessionID: sessionID,
				IsError:   true,
				Error:     streamErr,
				Messages:  messages,
			}, streamErr
		}

		rawText := turnTextBuf.String()
		if rawText != "" {
			if finalResponseText.Len() > 0 {
				finalResponseText.WriteString("\n")
			}
			finalResponseText.WriteString(rawText)
			if opts.EventListener != nil {
				opts.EventListener(AssistantTextEvent{
					BaseEvent: BaseEvent{TabID: 0},
					Text:      rawText,
				})
			}
		}

		assistantMsg := NewAssistantMessage(rawText, turnThoughts, turnToolCalls)
		messages = append(messages, assistantMsg)
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

			if opts.EventListener != nil {
				opts.EventListener(ToolResultEvent{
					BaseEvent: BaseEvent{TabID: 0},
					ToolName:  tc.Name,
					Output:    resp.Content,
					IsError:   resp.IsError,
				})
			}

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
		messages = append(messages, toolMsg)
		contents = append(contents, toolMsg.ToGenAIContent())

		if stopTurnRequested {
			break
		}
	}

	_ = store.Save(sessionID, opts.Cwd, messages)

	respText := finalResponseText.String()
	if opts.EventListener != nil {
		opts.EventListener(DoneEvent{
			BaseEvent: BaseEvent{TabID: 0},
			Result: ResultSummary{
				SessionID: sessionID,
				Result:    respText,
			},
		})
		opts.EventListener(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: 0}})
	}

	return &RunResult{
		SessionID: sessionID,
		Response:  respText,
		Messages:  messages,
		IsError:   false,
	}, nil
}
