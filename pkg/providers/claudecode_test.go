package providers

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
)

func TestClaudeCode_Identity(t *testing.T) {
	var p ClaudeCode
	if p.ID() != "claude-code" || p.DisplayName() != "Claude Code" {
		t.Fatalf("identity = %q / %q", p.ID(), p.DisplayName())
	}
	if p.DefaultModel() != "default" {
		t.Errorf("default model = %q", p.DefaultModel())
	}
	if got := p.EffortOptions(); !slices.Equal(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("effort options = %v", got)
	}
	if _, ok := SecretField(p); ok {
		t.Error("Claude Code has no secret field — auth lives in the binary")
	}
}

func TestClaudeCode_RegisteredAndDefaultUnchanged(t *testing.T) {
	if _, ok := Get(ClaudeCodeProviderID); !ok {
		t.Fatal("claude-code must be registered")
	}
	// Adding it must not move the default provider off Vertex.
	if DefaultProviderID() != VertexProviderID {
		t.Errorf("default provider = %q, want vertex", DefaultProviderID())
	}
}

func TestClaudeCode_Configured_ChecksBinaryOnPath(t *testing.T) {
	var p ClaudeCode
	// A binary that certainly does not exist.
	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "definitely-not-a-real-binary-xyz")
	if p.Configured(pc) {
		t.Error("Configured must be false when the binary is not on PATH")
	}
	// A binary that always exists on a POSIX test host.
	pc = config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "sh")
	if !p.Configured(pc) {
		t.Error("Configured must be true when the binary resolves")
	}
}

func TestClaudeCode_BuildModel_FailsFastWithoutBinary(t *testing.T) {
	var p ClaudeCode
	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "definitely-not-a-real-binary-xyz")
	if _, err := p.BuildModel(context.Background(), pc, "opus"); err == nil {
		t.Fatal("BuildModel must fail when the binary is missing")
	}
}

func TestClaudeCode_BuildModel_ReturnsCloser(t *testing.T) {
	isolateWindowProbes(t, func(context.Context, ClaudeCodeStartArgs) (ccConn, error) { return nil, errChildExited })
	var p ClaudeCode
	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "sh")
	m, err := p.BuildModel(context.Background(), pc, "opus")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	if m.Name() != "opus" {
		t.Errorf("model name = %q, want opus", m.Name())
	}
	// No child has been spawned, so Close is a no-op that must not error.
	if c, ok := m.(interface{ Close() error }); !ok {
		t.Fatal("the model must be an io.Closer")
	} else if err := c.Close(); err != nil {
		t.Errorf("Close on an unspawned model: %v", err)
	}
}

func TestClaudeCode_CanonicalModelID(t *testing.T) {
	var p ClaudeCode
	if got := p.CanonicalModelID("", ""); got != "default" {
		t.Errorf("empty -> %q, want default", got)
	}
	if got := p.CanonicalModelID("", "opus"); got != "opus" {
		t.Errorf("empty with fallback -> %q, want opus", got)
	}
	// An unrecognized id passes through — the CLI accepts full names and aliases.
	if got := p.CanonicalModelID("claude-opus-5", ""); got != "claude-opus-5" {
		t.Errorf("full name -> %q", got)
	}
}

func TestClaudeCode_CatalogAndLimits(t *testing.T) {
	var p ClaudeCode
	if !p.SupportsImages("opus") {
		t.Error("Claude models support images")
	}
	if got := p.ContextWindow("opus[1m]"); got != 1_000_000 {
		t.Errorf("opus[1m] context = %d, want 1M", got)
	}
	if got := p.ContextWindow("haiku"); got != 200_000 {
		t.Errorf("haiku context = %d, want 200k", got)
	}
	opts := p.ModelOptions()
	if len(opts) == 0 || opts[0] != "default" {
		t.Errorf("model options = %v, want default first", opts)
	}
}

func TestCCArgv_LocksDownClaudeContext(t *testing.T) {
	argv := ccArgv("opus", "high", "/tmp/ask-claude-system-abc.txt", "", false)
	joined := strings.Join(argv, " ")
	// The three flags that overwrite Claude's tools with ask's.
	for _, want := range []string{
		"--tools ", "--mcp-config", "--strict-mcp-config",
		"--allowedTools mcp__ask", "--setting-sources ",
		"--no-session-persistence",
		"--system-prompt-file /tmp/ask-claude-system-abc.txt",
		"--model opus", "--effort high",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q; got %v", want, argv)
		}
	}
	// The prompt must ride --system-prompt-file, never inline as --system-prompt,
	// or a large prompt overflows the OS argv limit.
	if i := indexOf(argv, "--system-prompt"); i >= 0 {
		t.Errorf("argv must not carry the inline --system-prompt flag; got %v", argv)
	}
	if i := indexOf(argv, "--system-prompt-file"); i < 0 || argv[i+1] != "/tmp/ask-claude-system-abc.txt" {
		t.Errorf("--system-prompt-file must be followed by the temp path; got %v", argv)
	}
	// autoMemory is turned off in --settings.
	if !strings.Contains(joined, `"autoMemoryEnabled":false`) {
		t.Errorf("argv must disable auto memory; got %v", argv)
	}
	// --tools is the empty string (disable all built-ins).
	if i := indexOf(argv, "--tools"); i < 0 || argv[i+1] != "" {
		t.Errorf("--tools must be followed by an empty string; got %v", argv)
	}
	// The sdk MCP server is declared.
	mcpIdx := indexOf(argv, "--mcp-config")
	if mcpIdx < 0 {
		t.Fatal("--mcp-config missing")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(argv[mcpIdx+1]), &cfg); err != nil {
		t.Fatalf("mcp-config is not valid JSON: %v", err)
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	ask, _ := servers["ask"].(map[string]any)
	if ask["type"] != "sdk" {
		t.Errorf("ask server must be sdk type; got %v", ask)
	}
}

func TestWriteClaudeSystemPromptFile(t *testing.T) {
	const want = "You are ask.\nA sizeable system prompt.\n"
	path, err := writeClaudeSystemPromptFile(want)
	if err != nil {
		t.Fatalf("writeClaudeSystemPromptFile: %v", err)
	}
	defer os.Remove(path)
	if path == "" {
		t.Fatal("returned path is empty")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if string(got) != want {
		t.Errorf("temp file content = %q, want %q", got, want)
	}
	// Each invocation gets a distinct random id so concurrent children never
	// share a file.
	path2, err := writeClaudeSystemPromptFile(want)
	if err != nil {
		t.Fatalf("writeClaudeSystemPromptFile (2): %v", err)
	}
	defer os.Remove(path2)
	if path2 == path {
		t.Errorf("two invocations returned the same path %q", path)
	}
}

func TestCCArgv_DefaultModelOmitsModelFlag(t *testing.T) {
	argv := ccArgv("default", "", "/tmp/ask-claude-system-xyz.txt", "", false)
	if indexOf(argv, "--model") >= 0 {
		t.Errorf("the default model must not pass --model; got %v", argv)
	}
	if indexOf(argv, "--effort") >= 0 {
		t.Errorf("empty effort must not pass --effort; got %v", argv)
	}
	if indexOf(argv, "--resume") >= 0 {
		t.Errorf("no seed must not pass --resume; got %v", argv)
	}
}

// A seeded child resumes from the transcript file, and still persists
// nothing of its own.
func TestCCArgv_SeedResumesFromFile(t *testing.T) {
	argv := ccArgv("opus", "", "/tmp/sys.txt", "/tmp/ask-claude-seed-1.jsonl", false)
	if i := indexOf(argv, "--resume"); i < 0 || argv[i+1] != "/tmp/ask-claude-seed-1.jsonl" {
		t.Fatalf("--resume must carry the seed path; got %v", argv)
	}
	if indexOf(argv, "--no-session-persistence") < 0 {
		t.Fatalf("a seeded child must still not persist a session; got %v", argv)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// ---- context window probe ----

// contextUsageFrame is a control_response carrying response, the way the CLI
// answers a get_context_usage request.
func contextUsageFrame(response map[string]any) ccFrame {
	b, _ := json.Marshal(response)
	return ccFrame{Type: "control_response", Response: b}
}

// windowProbeAnswering is a probe child that reports window as the model's.
func windowProbeAnswering(window int64) *fakeConn {
	fc := newFakeConn(4)
	fc.push(contextUsageFrame(map[string]any{
		"subtype": "success", "request_id": "window",
		"response": map[string]any{"maxTokens": window, "rawMaxTokens": window},
	}))
	return fc
}

// isolateWindowProbes swaps ccDial for dial and, when the test ends, waits out
// every window probe the test started before restoring it, then forgets every
// window learned: no probe outlives the fake it dialed, and no window leaks
// into another test.
func isolateWindowProbes(t *testing.T, dial func(context.Context, ClaudeCodeStartArgs) (ccConn, error)) {
	t.Helper()
	prev := ccDial
	ccDial = dial
	t.Cleanup(func() {
		claudeCodeWindowProbes.mu.Lock()
		probes := claudeCodeWindowProbes.byID
		claudeCodeWindowProbes.byID = map[string]chan struct{}{}
		claudeCodeWindowProbes.mu.Unlock()
		for _, done := range probes {
			<-done
		}
		ccDial = prev
		claudeCodeContextWindows.mu.Lock()
		claudeCodeContextWindows.byID = map[string]int64{}
		claudeCodeContextWindows.mu.Unlock()
	})
}

// A Claude Code model learns its context window from the CLI as soon as it is
// built, before any turn, so the first request of a resumed session is
// measured against the real window rather than the catalog's guess. The meter
// reads the same number, and one probe serves every model on the id.
func TestClaudeCode_BuildModelLearnsTheWindowFromTheCLI(t *testing.T) {
	const id = "window-probe-test"
	fc := windowProbeAnswering(1_000_000)
	var launches []ClaudeCodeStartArgs
	isolateWindowProbes(t, func(_ context.Context, args ClaudeCodeStartArgs) (ccConn, error) {
		launches = append(launches, args)
		return fc, nil
	})
	if got := (ClaudeCode{}).ContextWindow(id); got != ClaudeCodeContextWindow {
		t.Fatalf("before the probe the window is the catalog's %d, got %d", ClaudeCodeContextWindow, got)
	}

	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "sh")
	for i := 0; i < 2; i++ {
		m, err := ClaudeCode{}.BuildModel(context.Background(), pc, id)
		if err != nil {
			t.Fatalf("BuildModel: %v", err)
		}
		if w, ok := ResolveContextWindow(context.Background(), m); !ok || w != 1_000_000 {
			t.Fatalf("model %d resolved a %d-token window (ok=%v), want the CLI's 1000000", i, w, ok)
		}
	}
	if got := (ClaudeCode{}).ContextWindow(id); got != 1_000_000 {
		t.Fatalf("the meter's window = %d, want the CLI's 1000000", got)
	}

	if len(launches) != 1 {
		t.Fatalf("%d probe children for one model id, want 1", len(launches))
	}
	argv := launches[0].Argv
	if i := indexOf(argv, "--model"); i < 0 || i+1 >= len(argv) || argv[i+1] != id {
		t.Errorf("the probe did not ask about %s: argv %v", id, argv)
	}
	if indexOf(argv, "--mcp-config") >= 0 || indexOf(argv, "--system-prompt-file") >= 0 {
		t.Errorf("the probe is a bare handshake, got argv %v", argv)
	}
	var asked []any
	for _, s := range fc.sent {
		if req, ok := s["request"].(map[string]any); ok {
			asked = append(asked, req["subtype"])
		}
	}
	if !slices.Equal(asked, []any{"initialize", "get_context_usage"}) {
		t.Errorf("the probe sent %v, want initialize then get_context_usage", asked)
	}
	if !fc.closed {
		t.Error("the probe child outlived its answer")
	}
}

// The default model is probed without --model, and its window is found under
// an empty id as well as "default".
func TestClaudeCode_DefaultModelWindowUnderEitherName(t *testing.T) {
	const window = 500_000
	var argv []string
	isolateWindowProbes(t, func(_ context.Context, args ClaudeCodeStartArgs) (ccConn, error) {
		argv = args.Argv
		return windowProbeAnswering(window), nil
	})
	if got := (ClaudeCode{}).ContextWindow(ClaudeCodeDefaultModel); got == window {
		t.Fatalf("the catalog already says %d; pick a window it does not", window)
	}
	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "sh")
	m, err := ClaudeCode{}.BuildModel(context.Background(), pc, "")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	if w, _ := ResolveContextWindow(context.Background(), m); w != window {
		t.Fatalf("resolved %d, want %d", w, window)
	}
	if indexOf(argv, "--model") >= 0 {
		t.Errorf("the default model is the CLI's own; the probe must not name one: %v", argv)
	}
	for _, name := range []string{"", ClaudeCodeDefaultModel} {
		if got := (ClaudeCode{}).ContextWindow(name); got != window {
			t.Errorf("ContextWindow(%q) = %d, want %d", name, got, window)
		}
	}
}

// A probe that fails leaves the catalog's window in place, and the next model
// built for the id asks again.
func TestClaudeCode_FailedWindowProbeFallsBackAndIsRetried(t *testing.T) {
	const id = "window-probe-retry"
	var dials int
	isolateWindowProbes(t, func(context.Context, ClaudeCodeStartArgs) (ccConn, error) {
		dials++
		if dials == 1 {
			return nil, errChildExited
		}
		return windowProbeAnswering(1_000_000), nil
	})
	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "sh")

	m, err := ClaudeCode{}.BuildModel(context.Background(), pc, id)
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	if w, _ := ResolveContextWindow(context.Background(), m); w != ClaudeCodeContextWindow {
		t.Fatalf("after a failed probe the window is %d, want the catalog's %d", w, ClaudeCodeContextWindow)
	}
	if m, err = (ClaudeCode{}).BuildModel(context.Background(), pc, id); err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	if w, _ := ResolveContextWindow(context.Background(), m); w != 1_000_000 || dials != 2 {
		t.Fatalf("second model resolved %d after %d probes, want 1000000 after 2", w, dials)
	}
}

// A call waits for the probe no longer than its own context allows: a CLI that
// never answers costs the call its deadline, and the call goes out against the
// catalog's window.
func TestClaudeCodeModel_WindowWaitIsBoundedByTheCall(t *testing.T) {
	release := make(chan struct{})
	isolateWindowProbes(t, func(context.Context, ClaudeCodeStartArgs) (ccConn, error) {
		<-release
		return nil, errChildExited
	})
	defer close(release)

	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "sh")
	m, err := ClaudeCode{}.BuildModel(context.Background(), pc, "window-probe-hang")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if w, _ := ResolveContextWindow(ctx, m); w != ClaudeCodeContextWindow {
		t.Fatalf("resolved %d while the probe hung, want the catalog's %d", w, ClaudeCodeContextWindow)
	}
}

func TestProbeClaudeCodeContextWindow_ReadsTheCLIsAnswer(t *testing.T) {
	answer := func(response map[string]any) ccFrame {
		return contextUsageFrame(map[string]any{"subtype": "success", "request_id": "window", "response": response})
	}
	cases := []struct {
		name      string
		model     string
		frames    []ccFrame
		exits     bool
		want      int64
		wantErr   bool
		wantModel bool
	}{
		{
			name:  "the model's raw window, past the handshake's noise",
			model: "opus",
			frames: []ccFrame{
				{Type: "system", Subtype: "init"},
				contextUsageFrame(map[string]any{"subtype": "success", "request_id": "init", "response": map[string]any{"models": []any{}}}),
				answer(map[string]any{"rawMaxTokens": 1_000_000, "maxTokens": 967_000}),
			},
			want:      1_000_000,
			wantModel: true,
		},
		{name: "the budgeted window when no raw one is given", model: "default", frames: []ccFrame{answer(map[string]any{"maxTokens": 200_000})}, want: 200_000},
		{name: "an error answer", model: "opus", frames: []ccFrame{contextUsageFrame(map[string]any{"subtype": "error", "request_id": "window", "error": "no session"})}, wantErr: true, wantModel: true},
		{name: "an answer with no window", model: "opus", frames: []ccFrame{answer(map[string]any{})}, wantErr: true, wantModel: true},
		{name: "a child that exits first", model: "opus", exits: true, wantErr: true, wantModel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeConn(len(tc.frames) + 1)
			for _, f := range tc.frames {
				fc.push(f)
			}
			if tc.exits {
				close(fc.in)
			}
			var argv []string
			prev := ccDial
			ccDial = func(_ context.Context, args ClaudeCodeStartArgs) (ccConn, error) {
				argv = args.Argv
				return fc, nil
			}
			t.Cleanup(func() { ccDial = prev })

			w, err := probeClaudeCodeContextWindow(context.Background(), "claude", tc.model)
			if (err != nil) != tc.wantErr || w != tc.want {
				t.Fatalf("probe = %d, %v; want %d (error %v)", w, err, tc.want, tc.wantErr)
			}
			if got := indexOf(argv, "--model") >= 0; got != tc.wantModel {
				t.Errorf("--model in argv = %v, want %v: %v", got, tc.wantModel, argv)
			}
			if !fc.closed {
				t.Error("the probe child was not stopped")
			}
		})
	}
}
