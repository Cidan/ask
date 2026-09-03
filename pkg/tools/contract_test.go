package tools

import (
	"encoding/json"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"
)

func allCoreTools(t *testing.T) []Tool {
	t.Helper()
	env := NewToolEnv(t.TempDir(), 1, true, nil, nil)
	return CoreTools(env, func() []Tool { return nil }, true)
}

// Every tool speaks ADK's executable-tool contract exactly. RunADKTool
// used to probe seven different Run shapes because invoke_tool was the
// odd one out; a single assertion only works while this holds.
func TestEveryToolSatisfiesTheADKRunContract(t *testing.T) {
	for _, tl := range allCoreTools(t) {
		if tl.Name() == "preload_memory" {
			// A request processor, never called by the model.
			continue
		}
		if _, ok := tl.(interface {
			Run(agent.Context, any) (map[string]any, error)
		}); !ok {
			t.Errorf("%s does not implement Run(agent.Context, any) (map[string]any, error)", tl.Name())
		}
	}
}

// Tools are built with functiontool.New[TArgs, TResults] over real
// structs, so ADK derives an output schema. Passing `any` as TResults
// leaves it empty, which is what every tool used to do.
func TestEveryToolDeclaresAnOutputSchema(t *testing.T) {
	for _, tl := range allCoreTools(t) {
		if tl.Name() == "invoke_tool" || tl.Name() == "preload_memory" {
			// invoke_tool returns whatever the registry tool it dispatched
			// to returns, so it has no static response shape to declare;
			// preload_memory is a request processor with no declaration.
			continue
		}
		d, ok := tl.(interface {
			Declaration() *genai.FunctionDeclaration
		})
		if !ok {
			t.Errorf("%s has no Declaration", tl.Name())
			continue
		}
		decl := d.Declaration()
		if decl == nil {
			t.Errorf("%s has a nil Declaration", tl.Name())
			continue
		}
		// ADK stores this as either map[string]any or *jsonschema.Schema
		// depending on how the tool was built, so compare through JSON.
		raw, err := json.Marshal(decl.ResponseJsonSchema)
		if err != nil || string(raw) == "null" {
			t.Errorf("%s has no response schema — is it still passing `any` as TResults?", tl.Name())
			continue
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Errorf("%s response schema is not an object: %s", tl.Name(), raw)
			continue
		}
		if len(schema.Properties) == 0 {
			t.Errorf("%s response schema has no properties: %s", tl.Name(), raw)
		}
	}
}

// A tool reports a genuine failure with a Go error. That is what ADK's
// OnToolErrorCallback keys on, and therefore what lets the
// retryandreflect plugin hand the model corrective guidance instead of
// the failure being an ordinary result field it may ignore.
func TestToolFailuresAreGoErrors(t *testing.T) {
	env := NewToolEnv(t.TempDir(), 1, true, nil, nil)
	if _, err := runTypedTool[ReadResult](t, ReadTool(env), ReadParams{FilePath: "does-not-exist"}); err == nil {
		t.Error("a failing read must return a Go error, not a result field")
	}
	if _, err := runTypedTool[EditResult](t, EditTool(env), EditParams{FilePath: "x", OldString: "a", NewString: "a"}); err == nil {
		t.Error("a no-op edit must return a Go error")
	}
}

// The description phrase is a static field on every coding tool's params
// struct. That is what makes ADK's functioncallmodifier plugin
// unnecessary — it exists to inject synthetic arguments at request time,
// and ask has nothing left to inject.
func TestCodingToolsDeclareDescriptionStatically(t *testing.T) {
	coding := map[string]bool{
		"read": true, "write": true, "edit": true, "glob": true, "grep": true,
		"ls": true, "bash": true, "fetch": true, "todos": true,
	}
	for _, tl := range allCoreTools(t) {
		info := ExtractToolInfo(tl)
		if !coding[info.Name] {
			continue
		}
		if _, ok := info.Parameters["description"]; !ok {
			t.Errorf("%s must declare a description parameter, got %v", info.Name, info.Parameters)
		}
	}
}
