package engine

import (
	"testing"

	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

func TestInteraction_ConfirmationHelpers(t *testing.T) {
	// IsConfirmationCall
	if IsConfirmationCall(nil) {
		t.Error("expected false for nil FunctionCall")
	}

	nonConfirmCall := &genai.FunctionCall{
		Name: "read",
		Args: map[string]any{"file_path": "foo.go"},
	}
	if IsConfirmationCall(nonConfirmCall) {
		t.Error("expected false for non-confirmation FunctionCall")
	}

	confirmCall := &genai.FunctionCall{
		Name: toolconfirmation.FunctionCallName,
		Args: map[string]any{
			"originalFunctionCall": map[string]any{
				"name": "bash",
				"args": map[string]any{"command": "rm -rf tmp"},
			},
			"toolConfirmation": map[string]any{
				"hint": "destructive command",
			},
		},
	}
	if !IsConfirmationCall(confirmCall) {
		t.Error("expected true for confirmation FunctionCall")
	}

	// UnwrapConfirmationCall
	origCall, err := UnwrapConfirmationCall(confirmCall)
	if err != nil {
		t.Fatalf("unexpected error unwrapping confirmation call: %v", err)
	}
	if origCall == nil {
		t.Fatal("expected non-nil unwrapped function call")
	}
	if origCall.Name != "bash" {
		t.Errorf("expected unwrapped tool name bash, got %q", origCall.Name)
	}
	if cmd, ok := origCall.Args["command"].(string); !ok || cmd != "rm -rf tmp" {
		t.Errorf("expected unwrapped command 'rm -rf tmp', got %v", origCall.Args["command"])
	}

	// FormatConfirmationResponse
	respTrue := FormatConfirmationResponse(true)
	if confirmed, ok := respTrue["confirmed"].(bool); !ok || !confirmed {
		t.Errorf("expected confirmed: true in response, got %v", respTrue)
	}

	respFalse := FormatConfirmationResponse(false)
	if confirmed, ok := respFalse["confirmed"].(bool); !ok || confirmed {
		t.Errorf("expected confirmed: false in response, got %v", respFalse)
	}
}
