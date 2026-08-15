package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/google/uuid"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
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

	// Provider optionally overrides the LLM provider (e.g. "anthropic", "vertex", "deepseek").
	Provider string `json:"provider,omitempty"`

	// Model optionally overrides the default model for the selected provider.
	Model string `json:"model,omitempty"`

	// Effort optionally sets reasoning/thinking effort level.
	Effort string `json:"effort,omitempty"`

	// Files provides optional image/media attachments for models with vision.
	Files []fantasy.FilePart `json:"files,omitempty"`

	// Tools optionally overrides or augments the default toolset.
	Tools []fantasy.AgentTool `json:"-"`

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
	Messages []fantasy.Message `json:"messages"`

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

// ToolFactory builds a slice of fantasy AgentTools for an engine turn.
type ToolFactory func(args ToolFactoryArgs) []fantasy.AgentTool

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

// ModelBuilder allows customizing or mocking language model instantiation in tests.
var ModelBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
	if spec == nil {
		return nil, errors.New("provider spec is nil")
	}
	if spec.BuildModel == nil {
		return nil, fmt.Errorf("provider %s has no BuildModel implementation", spec.ID)
	}
	return spec.BuildModel(cfg, modelID)
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
	// Normalize working directory
	if opts.Cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.Cwd = wd
		} else {
			opts.Cwd = "."
		}
	}

	// Normalize configuration
	if opts.Config.Provider == "" {
		if loadedCfg, err := config.Load(); err == nil && loadedCfg.Provider != "" {
			opts.Config = loadedCfg
		}
	}

	// Normalize interaction handler
	if opts.InteractionHandler == nil {
		if e.opts.InteractionHandler != nil {
			opts.InteractionHandler = e.opts.InteractionHandler
		} else {
			opts.InteractionHandler = HeadlessInteractionHandler{
				AutoApproveTools: opts.SkipAllPermissions,
			}
		}
	}

	// Normalize event listener
	if opts.EventListener == nil {
		opts.EventListener = e.opts.EventListener
	}

	// Resolve provider ID
	providerID := strings.TrimSpace(opts.Provider)
	if providerID == "" {
		providerID = strings.TrimSpace(opts.Config.Provider)
	}
	if providerID == "" {
		providerID = "anthropic"
	}

	// Resolve provider spec
	spec, ok := providers.GetAgentProviderSpec(providerID)
	if !ok {
		return nil, fmt.Errorf("unsupported provider %q", providerID)
	}

	// Resolve model and effort
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

	// Resolve session ID and history
	sessionID := strings.TrimSpace(opts.SessionID)
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	store := NewSessionStore(providerID)
	var messages []fantasy.Message
	if opts.SessionID != "" {
		if file, err := store.Load(sessionID); err == nil {
			messages = file.Messages
		}
	}

	// Build language model
	lm, err := ModelBuilder(spec, opts.Config, modelID)
	if err != nil {
		return nil, fmt.Errorf("failed to build model %s for provider %s: %w", modelID, providerID, err)
	}

	// Setup tools
	var agentTools []fantasy.AgentTool
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
			AttachWebSearch:       spec.NativeWebSearch == nil,
		})
	}

	if spec.DecorateTools != nil && len(agentTools) > 0 {
		spec.DecorateTools(agentTools)
	}

	// Build system prompt
	systemPrompt := BuildSystemPrompt(PromptOptions{
		Cwd:        opts.Cwd,
		InWorkflow: false,
	})

	// Construct agent
	agentOpts := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(systemPrompt),
	}
	if len(agentTools) > 0 {
		agentOpts = append(agentOpts, fantasy.WithTools(agentTools...))
	}
	if spec.NativeWebSearch != nil {
		agentOpts = append(agentOpts, fantasy.WithProviderDefinedTools(spec.NativeWebSearch(modelID)))
	}
	if spec.PrepareStep != nil {
		agentOpts = append(agentOpts, fantasy.WithPrepareStep(spec.PrepareStep))
	}
	if spec.CallOptions != nil {
		callOpts, maxTokens := spec.CallOptions(modelID, effort)
		if callOpts != nil {
			agentOpts = append(agentOpts, fantasy.WithProviderOptions(callOpts))
		}
		if maxTokens != nil {
			agentOpts = append(agentOpts, fantasy.WithMaxOutputTokens(int64(*maxTokens)))
		}
	}

	agent := fantasy.NewAgent(lm, agentOpts...)

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

	var textBuf strings.Builder
	streamCall := fantasy.AgentStreamCall{
		Prompt:   opts.Prompt,
		Files:    opts.Files,
		Messages: append([]fantasy.Message(nil), messages...),
		OnTextDelta: func(_, delta string) error {
			textBuf.WriteString(delta)
			if opts.EventListener != nil {
				opts.EventListener(TextDeltaEvent{
					BaseEvent: BaseEvent{TabID: 0},
					Delta:     delta,
				})
			}
			return nil
		},
		OnTextEnd: func(string) error {
			if t := strings.TrimSpace(textBuf.String()); t != "" && opts.EventListener != nil {
				opts.EventListener(AssistantTextEvent{
					BaseEvent: BaseEvent{TabID: 0},
					Text:      t,
				})
			}
			textBuf.Reset()
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			if opts.EventListener != nil {
				var inMap map[string]any
				_ = json.Unmarshal([]byte(tc.Input), &inMap)
				opts.EventListener(ToolCallEvent{
					BaseEvent: BaseEvent{TabID: 0},
					ToolUseID: tc.ToolCallID,
					ToolName:  tc.ToolName,
					Input:     inMap,
				})
			}
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			if opts.EventListener != nil {
				opts.EventListener(ToolResultEvent{
					BaseEvent: BaseEvent{TabID: 0},
					ToolUseID: tr.ToolCallID,
					ToolName:  "",
					Output:    ToolResultText(tr.Result),
					IsError:   ToolResultIsError(tr.Result),
				})
			}
			return nil
		},
	}

	result, err := agent.Stream(ctx, streamCall)
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
			Messages:  messages,
			IsError:   true,
			Error:     err,
		}, err
	}

	// Append turn messages to history
	newMessages := []fantasy.Message{fantasy.NewUserMessage(opts.Prompt, opts.Files...)}
	for _, step := range result.Steps {
		newMessages = append(newMessages, step.Messages...)
	}
	messages = append(messages, newMessages...)

	response := result.Response.Content.Text()

	// Persist session transcript
	_ = store.Save(sessionID, opts.Cwd, messages)

	if opts.EventListener != nil {
		opts.EventListener(DoneEvent{
			BaseEvent: BaseEvent{TabID: 0},
			Result: ResultSummary{
				SessionID: sessionID,
				Result:    response,
				IsError:   false,
			},
		})
		opts.EventListener(TurnCompleteEvent{BaseEvent: BaseEvent{TabID: 0}})
	}

	return &RunResult{
		SessionID: sessionID,
		Response:  response,
		Messages:  messages,
		IsError:   false,
	}, nil
}
