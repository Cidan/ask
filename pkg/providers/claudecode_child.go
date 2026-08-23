package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"google.golang.org/genai"
)

// ccConn is the transport to one `claude -p` child: it writes control/user
// frames and delivers decoded incoming frames on a channel. ccDial makes a
// real subprocess-backed conn; tests swap ccDial for a scripted fake so the
// model logic runs with no process and no network.
type ccConn interface {
	// send marshals v to one NDJSON line on the child's stdin. Safe for
	// concurrent use.
	send(v any) error
	// frames delivers every decoded stdout line; it closes when the child's
	// stdout reaches EOF (the process exited).
	frames() <-chan ccFrame
	// stderrTail returns the buffered stderr, for attaching to an exit error.
	stderrTail() string
	// close interrupts nothing (the caller sends interrupt first); it closes
	// stdin and terminates the process group.
	close() error
}

// ccDialArgs is what ccDial needs to launch a child. Kept as a struct so the
// test seam has a stable signature.
type ccDialArgs struct {
	Binary string
	Argv   []string
	Dir    string
	Env    []string
}

// ccDial launches `claude` and returns a conn over its stdio. Swappable in
// tests.
var ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) {
	cmd := exec.CommandContext(ctx, args.Binary, args.Argv...)
	cmd.Dir = args.Dir
	cmd.Env = args.Env
	// Kill the whole process group when ctx is cancelled or close() runs, so
	// no orphaned node lingers.
	cmd.Cancel = func() error { return cmd.Process.Kill() }

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &ringBuffer{max: 8192}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude-code: start %s: %w", args.Binary, err)
	}

	c := &procConn{cmd: cmd, stdin: stdin, stderr: stderr, ch: make(chan ccFrame, 64)}
	go c.read(stdout)
	return c, nil
}

// procConn is the real ccConn.
type procConn struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *ringBuffer
	ch      chan ccFrame
	writeMu sync.Mutex
}

func (c *procConn) read(stdout io.Reader) {
	defer close(c.ch)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 4MB line cap, as the SDKs use
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue // sandbox noise / non-JSON, tolerated like the SDKs
		}
		var fr ccFrame
		if err := json.Unmarshal(line, &fr); err != nil {
			continue
		}
		c.ch <- fr
	}
}

func (c *procConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(b)
	return err
}

func (c *procConn) frames() <-chan ccFrame { return c.ch }
func (c *procConn) stderrTail() string     { return c.stderr.String() }

func (c *procConn) close() error {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}

// ringBuffer keeps the last max bytes written; the child's stderr goes here so
// an exit error can carry the tail without unbounded growth.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.buf))
}

// ---- MCP bridge: ask serves its tools to the child over the control channel ----

// ccReadOnlyTools are annotated readOnlyHint so the CLI issues their tools/call
// requests in parallel; every other tool runs serially (the CLI holds back the
// rest of the streamed assistant message until the call returns).
var ccReadOnlyTools = map[string]bool{
	"read": true, "glob": true, "grep": true, "ls": true, "fetch": true,
	"web_search": true, "load_memory": true, "preload_memory": true,
	"search_tools": true, "job_output": true,
}

// ccTool is one entry in the tools/list reply.
type ccTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// ccToolsFromRequest turns ADK's tool declarations into MCP tool entries. The
// bare ADK tool name (read, bash, …) is the MCP tool name; the child sees it
// as mcp__ask__<name> but its tools/call carries the bare name back.
func ccToolsFromRequest(tools []*genai.Tool) []ccTool {
	var out []ccTool
	for _, t := range tools {
		if t == nil {
			continue
		}
		for _, f := range t.FunctionDeclarations {
			if f == nil || f.Name == "" {
				continue
			}
			schema := ccToolSchema(f)
			tool := ccTool{Name: f.Name, Description: f.Description, InputSchema: schema}
			if ccReadOnlyTools[f.Name] {
				tool.Annotations = map[string]any{"readOnlyHint": true}
			}
			out = append(out, tool)
		}
	}
	return out
}

// ccToolSchema mirrors the OpenAI-compat translator: prefer the raw JSON
// Schema ADK's functiontool populates, else convert the genai.Schema. Every
// tool must advertise an object schema or the model calls it with {}.
func ccToolSchema(f *genai.FunctionDeclaration) map[string]any {
	if f.ParametersJsonSchema != nil {
		if b, err := json.Marshal(f.ParametersJsonSchema); err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil && len(m) > 0 {
				delete(m, "$schema")
				return m
			}
		}
	}
	if m := convertSchema(f.Parameters); len(m) > 0 {
		return m
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// ccMCPInitializeResult is the reply to the child's MCP initialize.
func ccMCPInitializeResult(protocolVersion string) map[string]any {
	if protocolVersion == "" {
		protocolVersion = "2025-11-25"
	}
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "ask", "version": "1"},
	}
}

// errChildExited is returned when the child's stdout closed unexpectedly.
var errChildExited = errors.New("claude-code: child process exited")

// currentEnvMinusClaude returns the process environment with every CLAUDE*
// variable removed (ask often runs inside a Claude Code session; CLAUDECODE=1
// and the messaging-socket vars must not leak into the child), plus the SDK
// entrypoint marker. ANTHROPIC_API_KEY and other non-CLAUDE vars pass through,
// so an API key in the environment is honoured; otherwise the child uses
// whatever `claude` is logged in as.
func currentEnvMinusClaude() []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+1)
	for _, kv := range src {
		if strings.HasPrefix(kv, "CLAUDE") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "CLAUDE_CODE_ENTRYPOINT=sdk-ts")
	return out
}
