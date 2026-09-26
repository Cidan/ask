package engine_test

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/workflow"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// wfCompactProvider is a registry entry with a window small enough for one
// workflow step to overflow it.
type wfCompactProvider struct{ window int64 }

func (wfCompactProvider) ID() string                            { return "wfcompacttest" }
func (wfCompactProvider) DisplayName() string                   { return "Workflow Compact Test" }
func (wfCompactProvider) DefaultModel() string                  { return "wf-model" }
func (wfCompactProvider) ModelOptions() []string                { return []string{"wf-model"} }
func (wfCompactProvider) EffortOptions() []string               { return nil }
func (wfCompactProvider) Settings() []providers.SettingField    { return nil }
func (wfCompactProvider) Configured(config.ProviderConfig) bool { return true }
func (wfCompactProvider) SupportsImages(string) bool            { return false }
func (wfCompactProvider) MaxOutputTokens(string) int64          { return 4096 }
func (p wfCompactProvider) ContextWindow(string) int64          { return p.window }
func (wfCompactProvider) CanonicalModelID(modelID, _ string) string {
	if modelID == "" {
		return "wf-model"
	}
	return modelID
}
func (wfCompactProvider) CallOptions(string, string) (*genai.GenerateContentConfig, *float64) {
	return nil, nil
}
func (wfCompactProvider) BuildModel(context.Context, config.ProviderConfig, string) (adkmodel.LLM, error) {
	return nil, nil
}

// readLoopModel keeps a workflow step reading a large file, far past the
// window, and reports usage as its own count of each request: a third of the
// request's JSON size, denser than the compactor's estimate.
type readLoopModel struct {
	path  string
	reads int

	mu    sync.Mutex
	calls int
	sizes []int
}

func (m *readLoopModel) Name() string { return "wf-model" }

func (m *readLoopModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	body, _ := json.Marshal(req.Contents)
	size := len(body) / 3
	if req.Config != nil {
		sys, _ := json.Marshal(req.Config.SystemInstruction)
		tools, _ := json.Marshal(req.Config.Tools)
		size += (len(sys) + len(tools)) / 3
	}
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.sizes = append(m.sizes, size)
	m.mu.Unlock()

	resp := finalText("read it all")
	if n <= m.reads {
		resp = functionCall("read", map[string]any{"file_path": m.path, "description": "reading again"})
	}
	resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount: int32(size),
		TotalTokenCount:  int32(size + 20),
	}
	return one(resp)
}

// A workflow step compacts like a chat turn: its own compactor, sized to its
// own model's window, keeps every request inside it through a long run of
// tool calls, and the notice reaches the run's tab.
func TestWorkflowStep_CompactsItsOwnContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	path := filepath.Join(cwd, "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("line of source code here\n", 400)), 0o644); err != nil {
		t.Fatal(err)
	}

	const window = 40_000
	providers.Register(wfCompactProvider{window: window})
	scripted := &readLoopModel{path: path, reads: 40}
	origBuilder := engine.ModelBuilder
	engine.ModelBuilder = func(context.Context, providers.Provider, config.Config, string) (adkmodel.LLM, error) {
		return scripted, nil
	}
	defer func() { engine.ModelBuilder = origBuilder }()

	var mu sync.Mutex
	var compacted []engine.ContextCompactedEvent
	eng := engine.New(engine.Options{
		Config:             config.Config{Provider: "wfcompacttest"},
		InteractionHandler: engine.HeadlessInteractionHandler{AutoApproveTools: true},
		EventListener: func(ev engine.EngineEvent) {
			if e, ok := ev.(engine.ContextCompactedEvent); ok {
				mu.Lock()
				compacted = append(compacted, e)
				mu.Unlock()
			}
		},
	})
	def := workflow.Def{Name: "long-read", Steps: []workflow.Step{{Name: "reader", Prompt: "Read big.txt over and over."}}}
	if err := eng.RunWorkflow(context.Background(), cwd, 7, def, workflow.NewTextSource(7, "src")); err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	scripted.mu.Lock()
	defer scripted.mu.Unlock()
	if scripted.calls <= scripted.reads {
		t.Fatalf("the step made %d model calls, want more than %d", scripted.calls, scripted.reads)
	}
	for i, size := range scripted.sizes {
		if size > window {
			t.Fatalf("request %d was %d tokens, over the %d-token window; sizes %v", i, size, window, scripted.sizes)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(compacted) == 0 {
		t.Fatalf("the step never compacted; sizes %v", scripted.sizes)
	}
	if compacted[0].TabID != 7 || compacted[0].ContextWindow != window {
		t.Fatalf("compaction event = %+v, want tab 7 and the step's window", compacted[0])
	}
}
