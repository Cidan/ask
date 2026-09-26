package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// fakeClaude stands in for the `claude` CLI across every child a session
// starts. Each child speaks the stream-json protocol over pipes, loads its
// --resume seed, and counts every token it holds the way the real child's
// context grows: system prompt, seed, user frames, tool results, and its own
// output. It reports each API call on message_start / message_delta stream
// events, prices it, and keeps a cumulative total_cost_usd like the CLI; an
// interrupt ends its turn with a result frame, as the CLI's does, and a
// get_context_usage request is answered with window. A task is a run of tool
// calls through ask's MCP bridge that survives a child being replaced
// mid-task.
type fakeClaude struct {
	t            *testing.T
	callsPerTask []int
	window       int

	mu         sync.Mutex
	task       int
	remaining  int
	calls      int
	maxContext int
	seeds      []string
	sysFiles   []string
	nudges     int
	children   []*fakeClaudeChild
}

// fakeCostPerToken is what the fake charges per token of context read, so a
// session's cost is a known sum across every child.
const fakeCostPerToken = 1e-6

// totalCost is what every child reported spending, in all.
func (f *fakeClaude) totalCost() float64 {
	var total float64
	for _, c := range f.children {
		total += c.cost
	}
	return total
}

// fakeTokens is the fake's own tokenizer: a third of the text, denser than
// ask's chars/4 estimate.
func fakeTokens(s string) int { return len(s) / 3 }

func (f *fakeClaude) start(_ context.Context, args providers.ClaudeCodeStartArgs) (providers.ClaudeCodeProcess, error) {
	c := &fakeClaudeChild{f: f, done: make(chan struct{})}
	c.stdinR, c.stdinW = io.Pipe()
	c.stdoutR, c.stdoutW = io.Pipe()

	if !containsString(args.Env, "DISABLE_COMPACT=1") {
		f.t.Errorf("child started without DISABLE_COMPACT=1; its own compaction would fight ask's")
	}
	for i := 0; i+1 < len(args.Argv); i++ {
		switch args.Argv[i] {
		case "--system-prompt-file":
			b, err := os.ReadFile(args.Argv[i+1])
			if err != nil {
				return nil, err
			}
			c.context += fakeTokens(string(b))
			f.mu.Lock()
			f.sysFiles = append(f.sysFiles, args.Argv[i+1])
			f.mu.Unlock()
		case "--resume":
			if err := c.loadSeed(args.Argv[i+1]); err != nil {
				return nil, err
			}
		}
	}
	f.mu.Lock()
	f.children = append(f.children, c)
	f.mu.Unlock()
	go c.run()
	return c, nil
}

type fakeClaudeChild struct {
	f               *fakeClaude
	stdinR, stdoutR *io.PipeReader
	stdinW, stdoutW *io.PipeWriter
	context         int
	cost            float64
	busy            bool
	interrupted     bool
	done            chan struct{}
}

func (c *fakeClaudeChild) Stdin() io.WriteCloser { return c.stdinW }
func (c *fakeClaudeChild) Stdout() io.Reader     { return c.stdoutR }
func (c *fakeClaudeChild) StderrTail() string    { return "" }
func (c *fakeClaudeChild) Stop() error {
	_ = c.stdinW.Close()
	_ = c.stdoutW.Close()
	<-c.done
	return nil
}

func (c *fakeClaudeChild) loadSeed(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var l struct {
			Type      string          `json:"type"`
			UUID      string          `json:"uuid"`
			SessionID string          `json:"sessionId"`
			Timestamp string          `json:"timestamp"`
			Message   json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return fmt.Errorf("seed line: %w", err)
		}
		if l.UUID == "" || l.SessionID == "" || l.Timestamp == "" || (l.Type != "user" && l.Type != "assistant") {
			return fmt.Errorf("seed line missing what --resume needs: %s", line)
		}
		c.context += fakeTokens(string(l.Message))
	}
	c.f.mu.Lock()
	c.f.seeds = append(c.f.seeds, path)
	c.f.mu.Unlock()
	return nil
}

func (c *fakeClaudeChild) emit(v any) {
	b, _ := json.Marshal(v)
	_, _ = c.stdoutW.Write(append(b, '\n'))
}

func (c *fakeClaudeChild) run() {
	defer close(c.done)
	sc := bufio.NewScanner(c.stdinR)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var fr map[string]any
		if json.Unmarshal(sc.Bytes(), &fr) != nil {
			continue
		}
		switch fr["type"] {
		case "control_request":
			req, _ := fr["request"].(map[string]any)
			switch req["subtype"] {
			case "interrupt":
				if c.busy {
					c.busy, c.interrupted = false, true
					c.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "total_cost_usd": c.cost})
				}
			case "get_context_usage":
				c.emit(map[string]any{"type": "control_response", "response": map[string]any{
					"subtype": "success", "request_id": fr["request_id"],
					"response": map[string]any{"maxTokens": c.f.window, "rawMaxTokens": c.f.window},
				}})
			}
		case "user":
			msg, _ := json.Marshal(fr["message"])
			c.context += fakeTokens(string(msg))
			text := ""
			if m, ok := fr["message"].(map[string]any); ok {
				if blocks, ok := m["content"].([]any); ok && len(blocks) > 0 {
					if b, ok := blocks[0].(map[string]any); ok {
						text, _ = b["text"].(string)
					}
				}
			}
			c.f.mu.Lock()
			if strings.HasPrefix(text, "[Context was compacted while you were mid-task") {
				c.f.nudges++
			} else {
				c.f.remaining = c.f.callsPerTask[c.f.task]
				c.f.task++
			}
			c.f.mu.Unlock()
			c.interrupted = false
			c.step()
		case "control_response":
			if c.interrupted {
				continue
			}
			resp, _ := fr["response"].(map[string]any)
			inner, _ := resp["response"].(map[string]any)
			if mcp, ok := inner["mcp_response"].(map[string]any); ok {
				result, _ := json.Marshal(mcp["result"])
				c.context += fakeTokens(string(result))
				c.step()
			}
		}
	}
}

// step is one API call: the model reads its whole context, then either asks
// for the next tool or ends the task.
func (c *fakeClaudeChild) step() {
	c.f.mu.Lock()
	if c.context > c.f.maxContext {
		c.f.maxContext = c.context
	}
	more := c.f.remaining > 0
	n := c.f.calls
	if more {
		c.f.remaining--
		c.f.calls++
	}
	c.f.mu.Unlock()

	start := map[string]any{"input_tokens": c.context, "output_tokens": 1}
	c.emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"usage": start}}})
	c.cost += float64(c.context) * fakeCostPerToken
	c.context += 20
	delta := map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": 20}}}
	if !more {
		c.busy = false
		c.emit(map[string]any{"type": "assistant", "message": map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "finished"}}, "usage": start,
		}})
		c.emit(delta)
		c.emit(map[string]any{"type": "result", "subtype": "success", "result": "finished", "total_cost_usd": c.cost})
		return
	}
	c.busy = true
	c.emit(map[string]any{"type": "assistant", "message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("reading %d", n)}}, "usage": start,
	}})
	c.emit(delta)
	call, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": n, "method": "tools/call",
		"params": map[string]any{
			"name": "dump", "arguments": map[string]any{"n": n},
			"_meta": map[string]any{"claudecode/toolUseId": fmt.Sprintf("toolu_%d", n)},
		},
	})
	c.emit(map[string]any{
		"type": "control_request", "request_id": fmt.Sprintf("req_%d", n),
		"request": map[string]any{"subtype": "mcp_message", "server_name": "ask", "message": json.RawMessage(call)},
	})
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// A Claude Code session is compacted exactly as every other provider is: the
// compactor cuts the history ask sends, and the child — which holds its own
// copy — is replaced by one seeded with that cut view, mid-task included. The
// child's context never outgrows the window, no tool call is lost or run
// twice, and the temp files go with the children.
func TestScenario_ClaudeCodeChildRebuiltOnCompaction(t *testing.T) {
	isolateTestHome(t)
	fake := &fakeClaude{t: t, callsPerTask: []int{24, 12}, window: scenarioWindow}
	prevStart := providers.ClaudeCodeStart
	providers.ClaudeCodeStart = fake.start
	t.Cleanup(func() { providers.ClaudeCodeStart = prevStart })
	t.Setenv(providers.ClaudeCodeEnvBinary, os.Args[0])

	llm, err := providers.ClaudeCode{}.BuildModel(context.Background(), config.ProviderConfig{}, "fake-model")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	providers.Register(compactStubProvider{window: scenarioWindow})

	var mu sync.Mutex
	var compactions int
	var failures []string
	var reported float64
	turnDone := make(chan struct{}, 4)
	var toolRuns atomic.Int64
	sess := NewSession(
		SessionArgs{TabID: 1, Cwd: t.TempDir(), Provider: "compacttest", Model: "fake-model"},
		llm, "system prompt", []Tool{dumpTool(t, 3000, &toolRuns)},
		func(ev EngineEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch e := ev.(type) {
			case ContextCompactedEvent:
				compactions++
			case UsageEvent:
				if e.Usage.CostSource == providers.CostReported {
					reported += e.Usage.CostUSD
				}
			case DoneEvent:
				if e.Result.IsError {
					failures = append(failures, e.Result.Result)
				}
			case TurnCompleteEvent:
				turnDone <- struct{}{}
			}
		},
		HeadlessInteractionHandler{AutoApproveTools: true},
	)

	for _, prompt := range []string{"read every file", "now read them again"} {
		if err := sess.QueueTurn(prompt); err != nil {
			t.Fatal(err)
		}
		select {
		case <-turnDone:
		case <-time.After(20 * time.Second):
			t.Fatalf("turn %q never completed", prompt)
		}
	}
	sess.Close()

	mu.Lock()
	defer mu.Unlock()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(failures) > 0 {
		t.Fatalf("turns failed: %v", failures)
	}
	if fake.maxContext > scenarioWindow {
		t.Fatalf("the child held %d tokens, over the %d-token window", fake.maxContext, scenarioWindow)
	}
	if compactions == 0 || len(fake.seeds) == 0 {
		t.Fatalf("no compaction reached the child: %d compactions, %d seeded children", compactions, len(fake.seeds))
	}
	if fake.nudges == 0 {
		t.Fatal("no child was rebuilt mid-task; the long task should outgrow the window")
	}
	// Every child's spend reached the session, including what the children
	// replaced mid-task had spent.
	if want := fake.totalCost(); want == 0 || reported < want-1e-9 || reported > want+1e-9 {
		t.Fatalf("session saw $%.6f reported, the children spent $%.6f", reported, want)
	}
	if toolRuns.Load() != 36 || fake.calls != 36 {
		t.Fatalf("tool ran %d times for %d calls, want 36 each — none lost, none repeated", toolRuns.Load(), fake.calls)
	}
	for _, p := range append(append([]string(nil), fake.seeds...), fake.sysFiles...) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("temp file %s outlived its child (stat err %v)", p, err)
		}
	}
}

// A resumed Claude Code session is measured against the window the CLI
// reports, not the registry's guess — here half of it, the way the catalog's
// 200k is of a 1M model. A history that overflows the guess but fits the real
// window goes out whole on the first call, before any turn could have
// reported the window.
func TestScenario_ClaudeCodeResumeMeasuredAgainstTheCLIsWindow(t *testing.T) {
	isolateTestHome(t)
	const (
		modelID       = "fake-model-resume"
		cliWindow     = 100_000
		guessedWindow = cliWindow / 2
	)
	fake := &fakeClaude{t: t, callsPerTask: []int{0}, window: cliWindow}
	prevStart := providers.ClaudeCodeStart
	providers.ClaudeCodeStart = fake.start
	t.Cleanup(func() { providers.ClaudeCodeStart = prevStart })
	t.Setenv(providers.ClaudeCodeEnvBinary, os.Args[0])
	providers.Register(compactStubProvider{window: guessedWindow})

	cwd := t.TempDir()
	ctx := context.Background()
	svc := NewFileSessionService("compacttest", cwd)
	created, err := svc.Create(ctx, &session.CreateRequest{AppName: "ask", UserID: "user", SessionID: "resumed"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		role, author := genai.Role(genai.RoleUser), "user"
		if i%2 == 1 {
			role, author = genai.RoleModel, "ask_coder"
		}
		if err := svc.AppendEvent(ctx, created.Session, &session.Event{
			Author:      author,
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(fmt.Sprintf("old %d %s", i, strings.Repeat("h", 6000)), role)},
			Timestamp:   time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	llm, err := providers.ClaudeCode{}.BuildModel(ctx, config.ProviderConfig{}, modelID)
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	var mu sync.Mutex
	var compactions []ContextCompactedEvent
	var failures []string
	turnDone := make(chan struct{}, 1)
	sess := NewSession(
		SessionArgs{TabID: 1, Cwd: cwd, Provider: "compacttest", Model: modelID, SessionID: "resumed"},
		llm, "system prompt", nil,
		func(ev EngineEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch e := ev.(type) {
			case ContextCompactedEvent:
				compactions = append(compactions, e)
			case DoneEvent:
				if e.Result.IsError {
					failures = append(failures, e.Result.Result)
				}
			case TurnCompleteEvent:
				turnDone <- struct{}{}
			}
		},
		HeadlessInteractionHandler{AutoApproveTools: true},
	)
	if err := sess.QueueTurn("continue"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-turnDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the resumed turn never completed")
	}
	sess.Close()

	mu.Lock()
	defer mu.Unlock()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(failures) > 0 {
		t.Fatalf("turn failed: %v", failures)
	}
	if len(compactions) != 0 {
		t.Fatalf("compacted against a %d-token window, the CLI's is %d: %+v", compactions[0].ContextWindow, cliWindow, compactions)
	}
	if fake.maxContext <= guessedWindow || fake.maxContext > cliWindow {
		t.Fatalf("the child held %d tokens; the history should overflow the guessed %d and fit the CLI's %d", fake.maxContext, guessedWindow, cliWindow)
	}
}
