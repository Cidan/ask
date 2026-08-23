package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Cidan/ask/pkg/engine"
	adkagent "google.golang.org/adk/v2/agent"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/tools"
	"github.com/Cidan/ask/pkg/workflow"
	adkmodel "google.golang.org/adk/v2/model"
)

type mockADKModel struct {
	name         string
	generateFunc func(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error]
}

func (m *mockADKModel) Name() string { return m.name }
func (m *mockADKModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if m.generateFunc != nil {
		return m.generateFunc(ctx, req, stream)
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {}
}

// fakeProvider is an instrumentable Provider for tests.
type fakeProvider struct {
	mu sync.Mutex

	id            string
	displayName   string
	caps          ProviderCapabilities
	modelPicker   ProviderPicker
	effortOptions []string
	baseSlash     []slashCmd

	probeInitFn     func(ProviderSessionArgs) tea.Cmd
	preMintFn       func(ProviderSessionArgs) string
	nativeSessionFn func(*providerProc) string
	startSessionFn  func(ProviderSessionArgs) (*providerProc, chan tea.Msg, error)
	sendFn          func(*providerProc, string, []pendingAttachment) error
	interruptFn     func(*providerProc) (bool, error)
	listSessionsFn  func(string) ([]sessionEntry, error)
	loadHistoryFn   func(string, HistoryOpts) ([]historyEntry, error)
	loadSettingsFn  func() ProviderSettings
	saveSettingsFn  func(ProviderSettings) error
	materializeFn   func(string, []NeutralTurn) (string, string, error)

	settings ProviderSettings

	sentTexts    []string
	sentAtts     [][]pendingAttachment
	startArgs    []ProviderSessionArgs
	savedState   []ProviderSettings
	historyCalls []string
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		id:            "fake",
		displayName:   "Fake",
		effortOptions: []string{"low", "medium", "high"},
		baseSlash:     []slashCmd{{"/new", "start a new fake session"}},
		caps: ProviderCapabilities{
			Resume:       true,
			ModelPicker:  true,
			EffortPicker: true,
		},
		modelPicker: ProviderPicker{
			Prompt:  "pick model",
			Options: []string{"default", "m-one", "m-two"},
		},
	}
}

func (f *fakeProvider) ID() string                         { return f.id }
func (f *fakeProvider) DisplayName() string                { return f.displayName }
func (f *fakeProvider) Capabilities() ProviderCapabilities { return f.caps }
func (f *fakeProvider) ModelPicker() ProviderPicker        { return f.modelPicker }
func (f *fakeProvider) EffortOptions() []string            { return f.effortOptions }
func (f *fakeProvider) BaseSlashCommands() []slashCmd      { return f.baseSlash }

func (f *fakeProvider) ProbeInit(args ProviderSessionArgs) tea.Cmd {
	if f.probeInitFn != nil {
		return f.probeInitFn(args)
	}
	return nil
}

func (f *fakeProvider) PreMintSessionID(args ProviderSessionArgs) string {
	if f.preMintFn != nil {
		return f.preMintFn(args)
	}
	return ""
}

func (f *fakeProvider) NativeSessionID(p *providerProc) string {
	if f.nativeSessionFn != nil {
		return f.nativeSessionFn(p)
	}
	return ""
}

func (f *fakeProvider) StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
	f.mu.Lock()
	f.startArgs = append(f.startArgs, args)
	f.mu.Unlock()
	if f.startSessionFn != nil {
		return f.startSessionFn(args)
	}
	ch := make(chan tea.Msg, 32)
	proc := &providerProc{stdin: &bufferCloser{Buffer: &bytes.Buffer{}}}
	return proc, ch, nil
}

func (f *fakeProvider) Interrupt(p *providerProc) (bool, error) {
	if f.interruptFn != nil {
		return f.interruptFn(p)
	}
	return false, nil
}

func (f *fakeProvider) Send(p *providerProc, text string, att []pendingAttachment) error {
	f.mu.Lock()
	f.sentTexts = append(f.sentTexts, text)
	cp := append([]pendingAttachment(nil), att...)
	f.sentAtts = append(f.sentAtts, cp)
	f.mu.Unlock()
	if f.sendFn != nil {
		return f.sendFn(p, text, att)
	}
	return nil
}

func (f *fakeProvider) ListSessions(cwd string) ([]sessionEntry, error) {
	if f.listSessionsFn != nil {
		return f.listSessionsFn(cwd)
	}
	return nil, nil
}

func (f *fakeProvider) LoadHistory(id string, opts HistoryOpts) ([]historyEntry, error) {
	f.mu.Lock()
	f.historyCalls = append(f.historyCalls, id)
	f.mu.Unlock()
	if f.loadHistoryFn != nil {
		return f.loadHistoryFn(id, opts)
	}
	return nil, nil
}

func (f *fakeProvider) LoadSettings() ProviderSettings {
	if f.loadSettingsFn != nil {
		return f.loadSettingsFn()
	}
	return f.settings
}

func (f *fakeProvider) SaveSettings(s ProviderSettings) error {
	f.mu.Lock()
	f.savedState = append(f.savedState, s)
	f.settings = s
	f.mu.Unlock()
	if f.saveSettingsFn != nil {
		return f.saveSettingsFn(s)
	}
	return nil
}

func (f *fakeProvider) Materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	if f.materializeFn != nil {
		return f.materializeFn(workspace, turns)
	}
	return "fake-" + f.id + "-" + newVirtualSessionID(), workspace, nil
}

type bufferCloser struct {
	*bytes.Buffer
}

func (b *bufferCloser) Close() error { return nil }

func withRegisteredProviders(t *testing.T, provs ...Provider) {
	t.Helper()
	prev := providerRegistry
	providerRegistry = append([]Provider(nil), provs...)
	t.Cleanup(func() { providerRegistry = prev })
}

// testHomes makes isolateHome idempotent within one test: fixtures and
// newTestModel can both call it, and seeding config between those calls
// must keep landing in the same isolated home.
var testHomes = map[*testing.T]string{}

func isolateHome(t *testing.T) string {
	t.Helper()
	if home, ok := testHomes[t]; ok {
		return home
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	testHomes[t] = home
	t.Cleanup(func() { delete(testHomes, t) })
	return home
}

func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func jjAvailable() bool {
	_, err := exec.LookPath("jj")
	return err == nil
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	if !gitAvailable() {
		t.Skip("git not available in PATH")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "commit", "--allow-empty", "-m", "init", "-q")
	return dir
}

func initJJRepo(t *testing.T) string {
	t.Helper()
	if !jjAvailable() {
		t.Skip("jj not available in PATH")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "repo")
	runJJ(t, parent, "git", "init", dir)
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func runJJ(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func newTestModel(t *testing.T, prov Provider) model {
	t.Helper()
	isolateHome(t)
	globalCoordinator.RemoveSession(1)
	globalCoordinator.CancelWorkflow(1)
	t.Cleanup(func() {
		globalCoordinator.RemoveSession(1)
		globalCoordinator.CancelWorkflow(1)
	})

	ta := textarea.New()
	ta.SetHeight(3)
	ta.DynamicHeight = true
	ta.Focus()
	vp := newChatView()
	sp := spinner.New()
	sp.Spinner = spinner.Spinner{
		Frames: spinner.Dot.Frames,
		FPS:    time.Second / 60,
	}
	return model{
		id:              1,
		cwd:             t.TempDir(),
		provider:        prov,
		input:           ta,
		chat:            vp,
		spinner:         sp,
		renderer:        newRenderer(100),
		width:           100,
		height:          30,
		mode:            modeInput,
		historyIdx:      -1,
		shellOutIdx:     -1,
		shellHistoryIdx: -1,
		fc:              &frameCache{},
	}
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runTool(t *testing.T, tool tools.Tool, input any) tools.ToolResponse {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	resp, err := tools.RunToolWithJSON(testAgentCtx(), tool, string(b))
	if err != nil {
		t.Fatalf("tool.Run returned hard error: %v", err)
	}
	return resp
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func newTestToolEnv(t *testing.T) (*agentToolEnv, *[]tea.Msg) {
	t.Helper()
	var mu sync.Mutex
	msgs := &[]tea.Msg{}
	env := newAgentToolEnv(t.TempDir(), 1, true, true, func(m tea.Msg) {
		mu.Lock()
		defer mu.Unlock()
		*msgs = append(*msgs, m)
	})
	return env, msgs
}

func drainCh(ch <-chan tea.Msg) []tea.Msg {
	var out []tea.Msg
	for msg := range ch {
		out = append(out, msg)
	}
	return out
}

func walkForItemsAnyOfConflict(t *testing.T, toolName string, node any) {
	t.Helper()
	switch n := node.(type) {
	case map[string]any:
		if anyOf, ok := n["anyOf"].([]any); ok {
			for _, branch := range anyOf {
				if bm, ok := branch.(map[string]any); ok {
					if _, hasItems := bm["items"]; hasItems {
						if _, parentHasItems := n["items"]; parentHasItems {
							t.Errorf("tool %s has conflicting items in anyOf branch and parent", toolName)
						}
					}
				}
			}
		}
		for _, child := range n {
			walkForItemsAnyOfConflict(t, toolName, child)
		}
	case []any:
		for _, child := range n {
			walkForItemsAnyOfConflict(t, toolName, child)
		}
	}
}

func drainBatch(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	switch m := msg.(type) {
	case tea.BatchMsg:
		var out []tea.Msg
		for _, sub := range m {
			out = append(out, drainBatch(t, sub)...)
		}
		return out
	default:
		if msg == nil {
			return nil
		}
		return []tea.Msg{msg}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stubWorkflowStepModel swaps the workflow compiler's model resolution
// for an inert LLM so a test can compile and run a graph without a real
// provider registered in pkg/providers.
func stubWorkflowStepModel(t *testing.T) {
	t.Helper()
	prev := workflowStepModel
	workflowStepModel = func(ctx context.Context, sess *agentSession, step workflow.Step) (adkmodel.LLM, error) {
		return &stubStepLLM{}, nil
	}
	t.Cleanup(func() { workflowStepModel = prev })
}

type stubStepLLM struct{}

func (s *stubStepLLM) Name() string { return "stub-step-model" }
func (s *stubStepLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {}
}

// testAgentCtx builds the fake agent.Context tests run tools under.
func testAgentCtx() adkagent.Context {
	return engine.NewStandaloneAgentContext(context.Background())
}
