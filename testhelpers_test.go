package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// fakeProvider is an instrumentable Provider for tests. Every method has an
// overridable *Fn hook; the zero value picks safe defaults so most tests can
// use newFakeProvider() verbatim.
type fakeProvider struct {
	mu sync.Mutex

	id            string
	displayName   string
	caps          ProviderCapabilities
	modelPicker   ProviderPicker
	effortOptions []string
	baseSlash     []slashCmd

	probeInitFn    func(ProviderSessionArgs) tea.Cmd
	startSessionFn func(ProviderSessionArgs) (*providerProc, chan tea.Msg, error)
	sendFn         func(*providerProc, string, []pendingAttachment) error
	interruptFn    func(*providerProc) (bool, error)
	listSessionsFn func(string) ([]sessionEntry, error)
	loadHistoryFn  func(string, HistoryOpts) ([]historyEntry, error)
	loadSettingsFn func() ProviderSettings
	saveSettingsFn func(ProviderSettings) error
	materializeFn  func(string, []NeutralTurn) (string, string, error)

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
		effortOptions: []string{"default", "low", "high"},
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

// bufferCloser makes a bytes.Buffer satisfy io.WriteCloser; providerProc.stdin
// is typed that way and is normally a pipe/file.
type bufferCloser struct {
	*bytes.Buffer
}

func (b *bufferCloser) Close() error { return nil }

// withRegisteredProviders swaps the global providerRegistry for the duration
// of the test, restoring it on cleanup.
func withRegisteredProviders(t *testing.T, provs ...Provider) {
	t.Helper()
	prev := providerRegistry
	providerRegistry = append([]Provider(nil), provs...)
	t.Cleanup(func() { providerRegistry = prev })
}

// isolateHome pins $HOME, $XDG_CONFIG_HOME and $XDG_CACHE_HOME at a
// tmp dir so tests never read or write the caller's real ~/.config,
// ~/.claude, or ~/.cache state (e.g. the ask-usage plugin cache).
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	return home
}

func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// initGitRepo stands up a throwaway git checkout with an empty initial commit
// so branches/worktrees can be cut off HEAD. Skips the test when git is not
// on PATH.
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

// newTestModel returns a model wired up just enough that Update/layout can
// run without spawning a real provider process. Tests replace m.provider when
// they want specific fake behavior.
func newTestModel(t *testing.T, prov Provider) model {
	t.Helper()
	ta := textarea.New()
	ta.SetHeight(3)
	ta.DynamicHeight = true
	vp := viewport.New()
	sp := spinner.New()
	return model{
		id:              1,
		cwd:             t.TempDir(),
		provider:        prov,
		input:           ta,
		viewport:        vp,
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

func drainCh(ch <-chan tea.Msg) []tea.Msg {
	var out []tea.Msg
	for msg := range ch {
		out = append(out, msg)
	}
	return out
}

// drainBatch executes cmd (expected to be a tea.BatchMsg-producing
// batch or a plain cmd) and returns the flattened tea.Msg list that
// the batched commands produced. Used by tests that fire compound
// commands (e.g. probe + loadHistory + translate) and need to pick
// specific messages out.
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
