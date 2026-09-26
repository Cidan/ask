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
)

// fakeClaude stands in for the `claude` CLI across every child a session
// starts. Each child speaks the stream-json protocol over pipes, loads its
// --resume seed, and counts every token it holds the way the real child's
// context grows: system prompt, seed, user frames, tool results, and its own
// output. A task is a run of tool calls through ask's MCP bridge that
// survives a child being replaced mid-task.
type fakeClaude struct {
	t            *testing.T
	callsPerTask []int

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
			c.step()
		case "control_response":
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

	usage := map[string]any{"input_tokens": c.context, "output_tokens": 20}
	c.context += 20
	if !more {
		c.emit(map[string]any{"type": "assistant", "message": map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "finished"}}, "usage": usage,
		}})
		c.emit(map[string]any{"type": "result", "subtype": "success", "result": "finished", "usage": usage})
		return
	}
	c.emit(map[string]any{"type": "assistant", "message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("reading %d", n)}}, "usage": usage,
	}})
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
	fake := &fakeClaude{t: t, callsPerTask: []int{24, 12}}
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
	if toolRuns.Load() != 36 || fake.calls != 36 {
		t.Fatalf("tool ran %d times for %d calls, want 36 each — none lost, none repeated", toolRuns.Load(), fake.calls)
	}
	for _, p := range append(append([]string(nil), fake.seeds...), fake.sysFiles...) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("temp file %s outlived its child (stat err %v)", p, err)
		}
	}
}
