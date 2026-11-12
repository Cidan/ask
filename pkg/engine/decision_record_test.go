package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestParseDecisionAddress(t *testing.T) {
	cases := map[string]DecisionAddress{
		"decision:d4":     {ID: "d4"},
		"decision:d12/3":  {ID: "d12", Call: 3},
		" decision:d1/1 ": {ID: "d1", Call: 1},
	}
	for in, want := range cases {
		got, ok := ParseDecisionAddress(in)
		if !ok || got != want {
			t.Errorf("%q = %+v %v, want %+v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"decision:", "decision:x4", "decision:d4/0", "decision:d4/x", "src/decision:d4", "d4"} {
		if IsDecisionAddress(bad) {
			t.Errorf("%q is not a decision address", bad)
		}
	}
	if (DecisionAddress{ID: "d2", Call: 5}).String() != "decision:d2/5" || (DecisionAddress{ID: "d2"}).String() != "decision:d2" {
		t.Error("addresses render as they parse")
	}
}

func TestDecisionRecordsRoundTripThroughJSON(t *testing.T) {
	recs := map[string]any{
		decisionStatePrefix + "d10": DecisionRecord{ID: "d10", CallID: "c10"},
		decisionStatePrefix + "d2":  DecisionRecord{ID: "d2", CallID: "c2", Calls: []DecisionCall{{Tool: "grep", Args: map[string]any{"pattern": "x"}, Result: map[string]any{"listing": "a.go\n  Line 1: x"}}}},
		"unrelated":                 "kept apart",
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	got := DecisionRecordsIn(stored)
	if len(got) != 2 || got[0].ID != "d2" || got[1].ID != "d10" {
		t.Fatalf("records come back in the order they were made: %+v", got)
	}
	text, isErr := got[0].Calls[0].Text()
	if isErr || !strings.Contains(text, "Line 1: x") {
		t.Errorf("a stored call's result reads back as its text: %q", text)
	}
	if text, isErr := (DecisionCall{Tool: "bash", Error: "denied"}).Text(); !isErr || text != "denied" {
		t.Errorf("a failed call reads as its error: %q", text)
	}
	if label := DecisionCallLabel(DecisionCall{Tool: "grep", Args: map[string]any{"pattern": "x", "description": "noise"}}); label != `grep {"pattern":"x"}` {
		t.Errorf("label = %q", label)
	}
}

// A decision record is saved once, in the session file's state, survives a
// reload, and is never copied into the event that made it.
func TestFileSessionService_KeepsDecisionRecordsInState(t *testing.T) {
	ctx := context.Background()
	cwd, base := t.TempDir(), t.TempDir()
	svc := NewFileSessionServiceWithBaseDir("vertex", cwd, base)
	created, err := svc.Create(ctx, &session.CreateRequest{AppName: "ask", UserID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	rec := DecisionRecord{ID: "d1", CallID: "call-1", Answers: "a: yes", Calls: []DecisionCall{{Tool: "read", Result: map[string]any{"content": "raw output"}}}, CreatedAt: time.Now()}
	ev := session.NewEvent(ctx, "inv-1")
	ev.Author = "ask_coder"
	ev.LLMResponse.Content = genai.NewContentFromText("done", genai.RoleModel)
	ev.Actions.StateDelta = map[string]any{decisionStatePrefix + "d1": rec, "other": "x"}
	if err := svc.AppendEvent(ctx, created.Session, ev); err != nil {
		t.Fatal(err)
	}

	path, err := svc.PathFor(created.Session.ID())
	if err != nil {
		t.Fatal(err)
	}
	file, err := ReadStoredSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := file.State[decisionStatePrefix+"d1"]; !ok {
		t.Fatalf("the record is in the file's state: %v", file.State)
	}
	stored := file.Events[len(file.Events)-1].Actions.StateDelta
	if _, dup := stored[decisionStatePrefix+"d1"]; dup || stored["other"] != "x" {
		t.Errorf("the stored event keeps its other state but not the record: %v", stored)
	}
	raw, _ := os.ReadFile(path)
	if strings.Count(string(raw), "raw output") != 1 {
		t.Errorf("the raw result is stored exactly once")
	}

	reloaded, err := NewFileSessionServiceWithBaseDir("vertex", cwd, base).Get(ctx, &session.GetRequest{AppName: "ask", UserID: "user", SessionID: created.Session.ID()})
	if err != nil {
		t.Fatal(err)
	}
	v, err := reloaded.Session.State().Get(decisionStatePrefix + "d1")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := decodeDecision(v)
	if !ok || got.CallID != "call-1" || got.Calls[0].Result["content"] != "raw output" {
		t.Fatalf("a reloaded session restores the record: %+v", got)
	}
	if LastDecisionIndex(reloaded.Session.State()) != 1 {
		t.Error("a resumed session continues numbering after its records")
	}
}

// Inner calls hand the decision model the instructions and rules their
// reads touch — including what the session's model already had in its
// prompt — once per decide, and never mark them delivered to the session's
// model.
func TestContextAwareTool_InnerCallsGetRulesWithoutDeliveringThem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestDoc(t, cwd, "CLAUDE.md", "root-marker")
	writeTestDoc(t, cwd, "src/api/CLAUDE.md", "subdir-marker")
	writeTestDoc(t, cwd, "src/api/handler.go", "package api\n")
	rules := []Rule{{Path: "/r/api.md", Rel: "api.md", Paths: []string{"src/api/**/*.go"}, Body: "API rule body."}}
	wrapped := WrapContextAwareTools([]Tool{&fakeRuleTestTool{cwd: cwd}}, cwd, rules)
	read := func(ctx context.Context) string {
		t.Helper()
		resp, err := RunADKTool(NewStandaloneAgentContext(ctx), wrapped[0], map[string]any{"file_path": "src/api/handler.go", "description": "read"})
		if err != nil {
			t.Fatal(err)
		}
		return mustToolText(resp)
	}

	decide := NewInnerCall()
	first := read(decide.ForCall().Context(testAgentCtx()))
	for _, want := range []string{"root-marker", "subdir-marker", "API rule body."} {
		if !strings.Contains(first, want) {
			t.Errorf("an inner read carries %q for the decision model:\n%s", want, first)
		}
	}
	if again := read(decide.ForCall().Context(testAgentCtx())); strings.Contains(again, "API rule body.") || strings.Contains(again, "subdir-marker") {
		t.Errorf("one decide hands each rule over once:\n%s", again)
	}

	session := read(context.Background())
	if !strings.Contains(session, "API rule body.") || !strings.Contains(session, "subdir-marker") {
		t.Errorf("what an inner call saw is still undelivered to the session's model:\n%s", session)
	}
	if strings.Contains(session, "root-marker") {
		t.Error("the session's model already has the root instructions in its prompt")
	}
	if other := read(NewInnerCall().ForCall().Context(testAgentCtx())); !strings.Contains(other, "API rule body.") {
		t.Error("a later decide starts its delivery fresh")
	}
}

func TestInnerCallScope(t *testing.T) {
	if InnerCallOf(context.Background()) != nil || InnerCallOf(testAgentCtx()) != nil {
		t.Fatal("a model's own call is not an inner call")
	}
	scope := NewInnerCall()
	call := scope.ForCall()
	ctx := call.Context(testAgentCtx())
	if InnerCallOf(ctx) != call {
		t.Fatal("the marked context carries its call's scope")
	}
	call.AddImage(genai.NewPartFromBytes([]byte("png"), "image/png"))
	if len(call.Images()) != 1 || len(scope.ForCall().Images()) != 0 {
		t.Error("each inner call keeps its own images")
	}
	if call.delivery != scope.delivery {
		t.Error("the calls of one decide share its rule delivery")
	}
}
