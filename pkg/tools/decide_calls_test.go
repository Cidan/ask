package tools

import (
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

// mapState is a session state for tests.
type mapState struct {
	mu sync.Mutex
	m  map[string]any
}

func (s *mapState) Get(k string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}

func (s *mapState) Set(k string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

func (s *mapState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for k, v := range s.m {
			if !yield(k, v) {
				return
			}
		}
	}
}

// sessionCtx is a tool context with a session state and a call id, as ADK
// hands a tool during a turn.
type sessionCtx struct {
	agent.Context
	st     *mapState
	callID string
}

func (c sessionCtx) State() session.State   { return c.st }
func (c sessionCtx) FunctionCallID() string { return c.callID }

// decideSession is a session's tools as a tab assembles them, decide's
// lookup included, with every event its tools emit recorded.
type decideSession struct {
	env    *ToolEnv
	tools  []Tool
	st     *mapState
	mu     sync.Mutex
	events []engine.EngineEvent
}

func newDecideSession(t *testing.T, dir string) *decideSession {
	t.Helper()
	ds := &decideSession{st: &mapState{m: map[string]any{}}}
	ds.env = NewToolEnv(dir, 1, true, func(ev engine.EngineEvent) {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		ds.events = append(ds.events, ev)
	}, nil)
	ds.env.ImageSink = NewImageSink()
	registry := func() []Tool { return nil }
	ds.tools = withProjectContext(CoreTools(ds.env, registry, false), dir)
	ds.env.Tools = SessionTools{Core: func() []Tool { return ds.tools }, Registry: registry}
	return ds
}

func (ds *decideSession) ctx(callID string) agent.Context {
	return sessionCtx{Context: testAgentCtx(), st: ds.st, callID: callID}
}

func (ds *decideSession) run(t *testing.T, name, callID, args string) ToolResponse {
	t.Helper()
	for _, tl := range ds.tools {
		if tl.Name() == name {
			resp, err := RunToolWithJSON(ds.ctx(callID), tl, args)
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}
	}
	t.Fatalf("no tool %s", name)
	return ToolResponse{}
}

func writeDecideFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// resultText is a successful tool response's human-readable text.
func resultText(t *testing.T, resp ToolResponse) string {
	t.Helper()
	if resp.IsError {
		t.Fatalf("tool failed: %s", resp.Content)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(resp.Content), &m); err != nil {
		t.Fatalf("decode %q: %v", resp.Content, err)
	}
	text, _ := engine.ToolResultText(m)
	return text
}

func decodeDecide(t *testing.T, resp ToolResponse) DecideResult {
	t.Helper()
	if resp.IsError {
		t.Fatalf("decide failed: %s", resp.Content)
	}
	var out DecideResult
	if err := json.Unmarshal([]byte(resp.Content), &out); err != nil {
		t.Fatalf("decode %q: %v", resp.Content, err)
	}
	return out
}

func TestDecideCalls_RunTheSessionsToolsAndKeepTheirResults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDecideFile(t, dir, "src/api/handler.go", "package api\n\n// token refresh happens here\nfunc Refresh() {}\n")
	writeDecideFile(t, dir, ".claude/rules/api.md", "---\npaths:\n  - \"src/api/**\"\n---\nAPI rule body.\n")
	d := &fakeDecider{answer: func(_ string, _ string, q providers.Question) (providers.Answer, error) {
		return providers.Answer{Kind: q.Kind, Yes: 0.9}, nil
	}}
	useFakeDecider(t, d)
	ds := newDecideSession(t, dir)

	out := decodeDecide(t, ds.run(t, "decide", "call-7", `{
		"description": "triage the auth code",
		"calls": [
			{"tool": "read", "args": {"file_path": "src/api/handler.go"}},
			{"tool": "grep", "args": {"pattern": "refresh", "path": "src"}, "description": "find refresh"},
			{"tool": "write", "args": {"file_path": "made.txt", "content": "x"}},
			{"tool": "no_such_tool", "args": {}}
		],
		"questions": [{"key": "relevant", "type": "yes_no", "question": "Is this about token refresh?"}]}`))

	if out.ID != "d1" || !strings.Contains(out.Answers, "relevant: yes — p(yes) 0.90") {
		t.Fatalf("reply: %+v", out)
	}
	for _, want := range []string{"decision:d1/1  read", "decision:d1/2  grep", "decision:d1/3  write", "cannot run inside decide", "unknown tool no_such_tool"} {
		if !strings.Contains(out.Answers, want) {
			t.Errorf("the reply indexes every call (%q):\n%s", want, out.Answers)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "made.txt")); !os.IsNotExist(err) {
		t.Error("a refused call never runs")
	}

	if len(d.reqs) != 1 {
		t.Fatalf("scope all asks once: %d requests", len(d.reqs))
	}
	var calls []map[string]any
	for _, p := range d.reqs[0].Input {
		var c map[string]any
		if json.Unmarshal([]byte(p.Text), &c) == nil {
			calls = append(calls, c)
		}
	}
	if len(calls) != 4 || calls[0]["tool"] != "read" || calls[0]["call"] != float64(1) {
		t.Fatalf("each call reaches the decision model as JSON: %+v", calls)
	}
	readResult, _ := calls[0]["result"].(map[string]any)
	if content, _ := readResult["content"].(string); !strings.Contains(content, "token refresh") || !strings.Contains(content, "API rule body.") {
		t.Errorf("the decision model gets the read and the rule it brings in: %v", readResult)
	}
	if args, _ := calls[1]["args"].(map[string]any); args["description"] != "find refresh" {
		t.Errorf("a call's own description rides its arguments: %v", args)
	}
	if calls[2]["error"] == nil || calls[3]["error"] == nil {
		t.Errorf("failed calls are reported to the decision model too: %v %v", calls[2], calls[3])
	}

	rec, err := engine.LoadDecision(ds.ctx("x"), "d1")
	if err != nil || rec.CallID != "call-7" || len(rec.Calls) != 4 || rec.Answers == "" {
		t.Fatalf("the record is kept in session state: %+v %v", rec, err)
	}

	var parents []string
	for _, ev := range ds.events {
		switch e := ev.(type) {
		case engine.ToolCallEvent:
			parents = append(parents, "call:"+e.ToolName+"@"+e.Parent)
		case engine.ToolResultEvent:
			parents = append(parents, "result:"+e.ToolName+"@"+e.Parent)
		}
	}
	if got := strings.Join(parents, ","); !strings.Contains(got, "call:read@call-7") || !strings.Contains(got, "result:grep@call-7") || strings.Contains(got, "write") {
		t.Errorf("every call that ran is shown under the decide: %s", got)
	}

	if !ds.env.Files.LastRead(filepath.Join(dir, "src/api/handler.go")).IsZero() {
		t.Error("decide's read does not count as the model having read the file")
	}
	if edit := ds.run(t, "edit", "call-8", `{"file_path": "src/api/handler.go", "old_string": "Refresh", "new_string": "Renew"}`); !edit.IsError {
		t.Errorf("an edit after only decide read the file is refused: %s", edit.Content)
	}

	peek := ds.run(t, "read", "call-9", `{"file_path": "decision:d1/1"}`)
	var peeked ReadResult
	if peek.IsError || json.Unmarshal([]byte(peek.Content), &peeked) != nil || !strings.Contains(peeked.Content, "     3\t") || !strings.Contains(peeked.Content, "token refresh") {
		t.Fatalf("read of a call's address shows its raw result, numbered: %+v", peek)
	}
	all := resultText(t, ds.run(t, "read", "call-9", `{"file_path": "decision:d1", "files": [{"file_path": "decision:d1/2"}]}`))
	if !strings.Contains(all, "### decision:d1/2 · grep") || !strings.Contains(all, "==> decision:d1/2 lines") {
		t.Fatalf("read of the decision shows every call under its address, batched too:\n%s", all)
	}
	found := resultText(t, ds.run(t, "grep", "call-9", `{"pattern": "(?i)refresh", "path": "decision:d1"}`))
	if !strings.Contains(found, "decision:d1/1\n  Line ") || !strings.Contains(found, "decision:d1/2\n  Line ") {
		t.Fatalf("grep searches every call's raw result:\n%s", found)
	}
	if missing := ds.run(t, "read", "call-9", `{"file_path": "decision:d9"}`); !missing.IsError {
		t.Error("an unknown decision is an error")
	}

	again := decodeDecide(t, ds.run(t, "decide", "call-10", `{"input": "x", "questions": [{"key": "a", "type": "yes_no", "question": "?"}]}`))
	if again.ID != "d2" {
		t.Errorf("ids continue through the session: %q", again.ID)
	}
}

func TestDecideCalls_ScopeEachAsksOfEveryCall(t *testing.T) {
	dir := t.TempDir()
	writeDecideFile(t, dir, "a.go", "alpha needle\n")
	writeDecideFile(t, dir, "b.go", "beta\n")
	d := &fakeDecider{answer: func(input string, _ string, q providers.Question) (providers.Answer, error) {
		yes := 0.1
		if strings.Contains(input, "alpha needle") {
			yes = 0.8
		}
		return providers.Answer{Kind: q.Kind, Yes: yes}, nil
	}}
	useFakeDecider(t, d)
	ds := newDecideSession(t, dir)
	out := decodeDecide(t, ds.run(t, "decide", "c1", `{
		"input": "Looking for the needle.",
		"scope": "each",
		"calls": [{"tool": "read", "args": {"file_path": "a.go"}}, {"tool": "read", "args": {"file_path": "b.go"}}],
		"questions": [{"key": "has", "type": "yes_no", "question": "Does this contain the needle?"}]}`))
	if len(d.reqs) != 2 {
		t.Fatalf("one request per call: %d", len(d.reqs))
	}
	for _, r := range d.reqs {
		if r.Input[0].Text != "Looking for the needle." || len(r.Input) != 2 {
			t.Errorf("each request is the shared input and one call: %+v", r.Input)
		}
	}
	for _, want := range []string{"[1] decision:d1/1  read", "  has: yes — p(yes) 0.80", "[2] decision:d1/2  read", "  has: no — p(yes) 0.10"} {
		if !strings.Contains(out.Answers, want) {
			t.Errorf("answers missing %q:\n%s", want, out.Answers)
		}
	}
	if resp := ds.run(t, "decide", "c2", `{"input": "x", "scope": "each", "questions": [{"key": "a", "type": "yes_no", "question": "?"}]}`); !resp.IsError {
		t.Error(`scope "each" without calls is an error`)
	}
}

func TestDecideCalls_ImagesGoToTheDecisionModel(t *testing.T) {
	dir := t.TempDir()
	writeTestPNG(t, filepath.Join(dir, "shot.png"), 4, 4)
	d := &fakeDecider{answer: func(_ string, _ string, q providers.Question) (providers.Answer, error) {
		return providers.Answer{Kind: q.Kind, Yes: 0.5}, nil
	}}
	useFakeDecider(t, d)
	ds := newDecideSession(t, dir)
	ds.env.SupportsImages = func() bool { return false }
	decodeDecide(t, ds.run(t, "decide", "c1", `{"calls": [{"tool": "read", "args": {"file_path": "shot.png"}}],
		"questions": [{"key": "broken", "type": "yes_no", "question": "Is the layout broken?"}]}`))
	var images int
	for _, p := range d.reqs[0].Input {
		if p.InlineData != nil && p.InlineData.MIMEType == "image/png" {
			images++
		}
	}
	if images != 1 {
		t.Errorf("an image a call reads goes to the decision model, whatever the session's model can see: %d", images)
	}
	if pending := ds.env.ImageSink.Drain(); len(pending) != 0 {
		t.Error("and never to the session's model")
	}
}

func TestDecideCalls_NoModelRunsNothing(t *testing.T) {
	dir := t.TempDir()
	useFakeDecider(t, nil)
	ds := newDecideSession(t, dir)
	resp := ds.run(t, "decide", "c1", `{"calls": [{"tool": "grep", "args": {"pattern": "x"}}], "questions": [{"key": "a", "type": "yes_no", "question": "?"}]}`)
	if out := decodeDecide(t, resp); out.Notice != DecideNoModelNotice || out.ID != "" {
		t.Fatalf("no decision model is a notice: %+v", out)
	}
	if len(ds.events) != 0 {
		t.Errorf("no call runs when nothing can answer: %+v", ds.events)
	}
}

func TestDecideRefused(t *testing.T) {
	for _, name := range []string{"write", "edit", "task", "decide", "invoke_tool", "todos", "workflow_create", "watcher_run", "plugin_install", "memory_index", "linear_create_issue", "end_turn"} {
		if !decideRefused(name) {
			t.Errorf("%s must be refused", name)
		}
	}
	for _, name := range []string{"read", "grep", "glob", "ls", "bash", "fetch", "web_search", "load_memory", "render_design", "workflow_list", "watcher_get", "linear_list_issues", "mcp__github__create_issue"} {
		if decideRefused(name) {
			t.Errorf("%s gathers and may run", name)
		}
	}
}
