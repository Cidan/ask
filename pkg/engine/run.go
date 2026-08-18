package engine

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

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

// ModelBuilder allows customizing or mocking ADK LLM instantiation in tests.
var ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
	if spec == nil {
		return nil, errors.New("provider spec is nil")
	}
	if spec.BuildModel == nil {
		return nil, fmt.Errorf("provider %s has no BuildModel implementation", spec.ID)
	}
	return spec.BuildModel(ctx, cfg, modelID)
}

// RunnerBuilder allows customizing or mocking the ADK runner in tests.
type RunnerBuilderFunc func(agentInstance agent.Agent, sessSvc session.Service) (*runner.Runner, error)

var RunnerBuilder RunnerBuilderFunc = func(agentInstance agent.Agent, sessSvc session.Service) (*runner.Runner, error) {
	return runner.New(runner.Config{
		AppName:        "ask",
		Agent:          agentInstance,
		SessionService: sessSvc,
	})
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
	if !ok || spec == nil {
		return nil, fmt.Errorf("unknown provider %q", providerID)
	}

	modelID := strings.TrimSpace(opts.Model)
	if modelID == "" {
		settings := spec.LoadSettings(opts.Config)
		modelID = strings.TrimSpace(settings.Model)
	}
	if modelID == "" {
		modelID = spec.DefaultModel
	}

	effort := strings.TrimSpace(opts.Effort)
	if effort == "" {
		settings := spec.LoadSettings(opts.Config)
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

	llm, err := ModelBuilder(ctx, spec, opts.Config, modelID)
	if err != nil {
		return nil, fmt.Errorf("failed to build model for provider %s: %w", providerID, err)
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

	adkTools, err := AsADKTools(agentTools)
	if err != nil {
		return nil, fmt.Errorf("failed to convert tools to ADK: %w", err)
	}

	systemPrompt := BuildSystemPrompt(PromptOptions{
		Cwd:        opts.Cwd,
		InWorkflow: false,
	})

	genConfig := &genai.GenerateContentConfig{}
	if spec.CallOptions != nil {
		if callOpts, _ := spec.CallOptions(modelID, effort); callOpts != nil {
			if callOpts.ThinkingConfig != nil {
				genConfig.ThinkingConfig = callOpts.ThinkingConfig
			}
		}
	}

	agentInstance, err := llmagent.New(llmagent.Config{
		Name:                  "ask_coder",
		Model:                 llm,
		Instruction:           systemPrompt,
		Tools:                 adkTools,
		GenerateContentConfig: genConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create ADK agent: %w", err)
	}

	sessSvc := session.InMemoryService()
	created, err := sessSvc.Create(ctx, &session.CreateRequest{
		AppName:   "ask",
		UserID:    "user",
		SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create ADK session: %w", err)
	}

	// Replay history events into the ADK session service if resuming
	for _, msg := range messages {
		event := &session.Event{
			LLMResponse: model.LLMResponse{
				Content: msg.ToGenAIContent(),
			},
			Timestamp: time.Now(),
		}
		_ = sessSvc.AppendEvent(ctx, created.Session, event)
	}

	r, err := RunnerBuilder(agentInstance, sessSvc)
	if err != nil {
		return nil, fmt.Errorf("failed to create ADK runner: %w", err)
	}

	userMsg := genai.NewContentFromText(opts.Prompt, genai.RoleUser)
	for _, f := range opts.Files {
		if len(f.Data) > 0 {
			userMsg.Parts = append(userMsg.Parts, genai.NewPartFromBytes(f.Data, f.MIMEType))
		}
	}

	userMessageObj := NewUserMessage(opts.Prompt, opts.Files...)
	messages = append(messages, userMessageObj)

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

	var finalResponseText strings.Builder
	var currentTurnThoughts []ThoughtPart
	var currentTurnToolCalls []ToolCallPart
	var currentTurnToolResults []ToolResultPart

	for event, err := range r.Run(ctx, "user", sessionID, userMsg, agent.RunConfig{}) {
		if err != nil {
			if opts.EventListener != nil {
				opts.EventListener(DoneEvent{
					BaseEvent: BaseEvent{TabID: 0},
					Result: ResultSummary{
						SessionID: sessionID,
						IsError:   true,
						Result:    err.Error(),
					},
					Error: err,
				})
				opts.EventListener(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: 0}})
			}
			return &RunResult{
				SessionID: sessionID,
				IsError:   true,
				Error:     err,
				Messages:  messages,
			}, err
		}
		if event == nil {
			continue
		}

		if event.UsageMetadata != nil && opts.EventListener != nil {
			opts.EventListener(UsageEvent{
				BaseEvent:    BaseEvent{TabID: 0},
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
						if opts.EventListener != nil {
							opts.EventListener(TextDeltaEvent{
								BaseEvent: BaseEvent{TabID: 0},
								Delta:     part.Text,
							})
						}
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
					if opts.EventListener != nil {
						opts.EventListener(ToolCallEvent{
							BaseEvent: BaseEvent{TabID: 0},
							ToolName:  tc.Name,
							Input:     tc.Args,
						})
					}
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
					if opts.EventListener != nil {
						opts.EventListener(ToolResultEvent{
							BaseEvent: BaseEvent{TabID: 0},
							ToolName:  tr.Name,
							Output:    tr.Content,
							IsError:   tr.IsError,
						})
					}
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
				if opts.EventListener != nil {
					opts.EventListener(AssistantTextEvent{
						BaseEvent: BaseEvent{TabID: 0},
						Text:      txt,
					})
				}
			}
			if len(currentTurnThoughts) > 0 || len(currentTurnToolCalls) > 0 || txt != "" {
				messages = append(messages, NewAssistantMessage(txt, currentTurnThoughts, currentTurnToolCalls))
				currentTurnThoughts = nil
				currentTurnToolCalls = nil
			}
			if len(currentTurnToolResults) > 0 {
				messages = append(messages, NewToolResultMessage(currentTurnToolResults...))
				currentTurnToolResults = nil
			}
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
