package engine

import (
	"context"

	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// ConfirmationFunctionCallName is the wire function call name used by ADK for HITL confirmations.
const ConfirmationFunctionCallName = toolconfirmation.FunctionCallName

// QuestionOption represents a single choice in a question modal.
type QuestionOption struct {
	Label   string `json:"label"`
	Diagram string `json:"diagram,omitempty"`
}

// Question represents a structured prompt for the user.
type Question struct {
	Kind        string           `json:"kind"` // "pick_one", "pick_many", "pick_diagram"
	Prompt      string           `json:"prompt"`
	Options     []QuestionOption `json:"options"`
	AllowCustom bool             `json:"allow_custom,omitempty"`
}

// QuestionAnswer contains the user's response to a single question.
type QuestionAnswer struct {
	Picks  []string `json:"picks"`
	Custom string   `json:"custom,omitempty"`
	Note   string   `json:"note,omitempty"`
}

// QuestionResponse is the full result from answering one or more questions.
type QuestionResponse struct {
	Answers   []QuestionAnswer `json:"answers"`
	Cancelled bool             `json:"cancelled,omitempty"`
	Headless  bool             `json:"headless,omitempty"`
}

// PermissionRule identifies a scoped permission grant.
type PermissionRule struct {
	ToolName    string `json:"tool_name"`
	RuleContent string `json:"rule_content"`
}

// ApprovalRequest asks the user to permit a mutating tool invocation.
type ApprovalRequest struct {
	ToolName  string         `json:"tool_name"`
	Input     map[string]any `json:"input"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
}

// ApprovalResponse contains the user's decision on tool approval.
type ApprovalResponse struct {
	Allow    bool            `json:"allow"`
	Remember *PermissionRule `json:"remember,omitempty"`
}

// PlanRequest asks the user to confirm a finalized plan.
type PlanRequest struct {
	Plan            string `json:"plan"`
	Explanation     string `json:"explanation"`
	DefaultWorkflow string `json:"default_workflow,omitempty"`
}

// PlanResponse contains the user's choice after reviewing a plan.
type PlanResponse struct {
	Headless      bool     `json:"headless,omitempty"`
	Cancelled     bool     `json:"cancelled,omitempty"`
	TalkMore      bool     `json:"talk_more,omitempty"`
	ExecuteInline bool     `json:"execute_inline,omitempty"`
	WorkflowName  string   `json:"workflow_name,omitempty"`
	WorkflowDone  bool     `json:"workflow_done,omitempty"`
	FailedReason  string   `json:"failed_reason,omitempty"`
	Outcome       string   `json:"outcome,omitempty"`
	Artifacts     []string `json:"artifacts,omitempty"`
	Source        any      `json:"-"`
}

// SudoPasswordResponse contains the user's input for a sudo prompt.
type SudoPasswordResponse struct {
	Password  string `json:"password"`
	Cancelled bool   `json:"cancelled"`
}

// InteractionHandler is implemented by user interfaces (TUI, Web UI, CLI, headless agents)
// to respond to interactive requests from the engine and tools.
type InteractionHandler interface {
	AskQuestion(ctx context.Context, tabID int, questions []Question) (QuestionResponse, error)
	RequestApproval(ctx context.Context, tabID int, req ApprovalRequest) (ApprovalResponse, error)
	ConfirmPlan(ctx context.Context, tabID int, req PlanRequest) (PlanResponse, error)
	RequestSudoPassword(ctx context.Context, tabID int, prompt string) (SudoPasswordResponse, error)
}

// HeadlessInteractionHandler is a default InteractionHandler for automated runs.
type HeadlessInteractionHandler struct {
	AutoApproveTools bool
}

func (h HeadlessInteractionHandler) AskQuestion(ctx context.Context, tabID int, questions []Question) (QuestionResponse, error) {
	return QuestionResponse{Headless: true}, nil
}

func (h HeadlessInteractionHandler) RequestApproval(ctx context.Context, tabID int, req ApprovalRequest) (ApprovalResponse, error) {
	return ApprovalResponse{Allow: h.AutoApproveTools}, nil
}

func (h HeadlessInteractionHandler) ConfirmPlan(ctx context.Context, tabID int, req PlanRequest) (PlanResponse, error) {
	return PlanResponse{Headless: true}, nil
}

func (h HeadlessInteractionHandler) RequestSudoPassword(ctx context.Context, tabID int, prompt string) (SudoPasswordResponse, error) {
	return SudoPasswordResponse{Cancelled: true}, nil
}

// Approval is NOT ADK's tool/toolconfirmation flow, deliberately.
//
// That flow emits an adk_request_confirmation function call, pauses the
// run, and resumes when a function response arrives. ask has no
// suspend/resume path for a chat turn, so adopting it means building
// one. Instead a mutating tool calls ToolEnv.ApprovalDenied, which blocks
// on the TUI modal and returns the denial message inline.
//
// The two unwrap helpers below stay because an MCP server can declare
// confirmation on its own tools; if ADK ever emits the call, the event
// loops in run.go and agent_run.go render the inner intent rather than
// the wrapper.

// IsConfirmationCall reports whether a function call is an ADK tool confirmation request.
func IsConfirmationCall(fc *genai.FunctionCall) bool {
	if fc == nil {
		return false
	}
	return fc.Name == toolconfirmation.FunctionCallName
}

// UnwrapConfirmationCall extracts the underlying function call from an ADK confirmation wrapper.
func UnwrapConfirmationCall(fc *genai.FunctionCall) (*genai.FunctionCall, error) {
	return toolconfirmation.OriginalCallFrom(fc)
}
