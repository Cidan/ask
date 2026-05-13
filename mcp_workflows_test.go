package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newWorkflowMCPTestBridge stages a fresh bridge bound to a tmp HOME
// + a project cwd inside it, with the fake provider registered so
// `validateProviderID` accepts the "fake" id. Restores the provider
// registry on test cleanup.
func newWorkflowMCPTestBridge(t *testing.T, tabID int) (*mcpBridge, string) {
	t.Helper()
	cwd := isolateHome(t)
	withRegisteredProviders(t, newFakeProvider())
	b := &mcpBridge{tabID: tabID}
	b.setCwd(cwd)
	return b, cwd
}

// installSpawnCaptor swaps mcpSpawnWorkflowTab for a captor that
// records every dispatched message. Restores the production
// implementation on cleanup. The returned slice is mutated by the
// captor — read it after dispatch.
func installSpawnCaptor(t *testing.T) *[]spawnWorkflowTabMsg {
	t.Helper()
	prev := mcpSpawnWorkflowTab
	captured := &[]spawnWorkflowTabMsg{}
	var mu sync.Mutex
	mcpSpawnWorkflowTab = func(msg spawnWorkflowTabMsg) error {
		mu.Lock()
		defer mu.Unlock()
		*captured = append(*captured, msg)
		return nil
	}
	t.Cleanup(func() { mcpSpawnWorkflowTab = prev })
	return captured
}

// installFailingSpawn swaps mcpSpawnWorkflowTab for a function that
// always errors with msg. Used to drive the "ask UI not ready"
// branch deterministically.
func installFailingSpawn(t *testing.T, message string) {
	t.Helper()
	prev := mcpSpawnWorkflowTab
	mcpSpawnWorkflowTab = func(_ spawnWorkflowTabMsg) error {
		return fmt.Errorf("%s", message)
	}
	t.Cleanup(func() { mcpSpawnWorkflowTab = prev })
}

// callTool is a tiny shim that fakes the request the SDK normally
// builds. The handlers don't read most of req.Params, so a zero
// CallToolRequest is fine.
func newCallToolReq() *mcp.CallToolRequest {
	return &mcp.CallToolRequest{}
}

func connectWorkflowMCPClient(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "workflow-test-client", Version: "0.1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// seedWorkflows writes a fresh project-config workflows list to disk
// for cwd. Used by tests that want a known starting state without
// going through workflow_create.
func seedWorkflows(t *testing.T, cwd string, items []workflowDef) {
	t.Helper()
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = items
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seedWorkflows: %v", err)
	}
}

// readWorkflows reads the on-disk workflow list back so tests can
// assert against persisted state.
func readWorkflows(t *testing.T, cwd string) []workflowDef {
	t.Helper()
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	return pc.Workflows.Items
}

// ----- workflow_list -----

func TestWorkflowList_EmptyProject(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	res, out, err := b.workflowListTool(context.Background(), newCallToolReq(), workflowListInput{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("empty project should not error; got %+v", res)
	}
	if len(out.Workflows) != 0 {
		t.Errorf("expected empty list, got %+v", out.Workflows)
	}
}

func TestWorkflowList_TrimsPromptFromSteps(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name: "one",
		Steps: []workflowStep{
			{Name: "a", Provider: "fake", Model: "m", Prompt: "secret prompt body"},
		},
	}})
	_, out, err := b.workflowListTool(context.Background(), newCallToolReq(), workflowListInput{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out.Workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(out.Workflows))
	}
	w := out.Workflows[0]
	if w.Name != "one" || len(w.Steps) != 1 {
		t.Errorf("unexpected listing: %+v", w)
	}
	// workflowListStepView intentionally omits the Prompt field; the
	// JSON projection of the listing must therefore not contain it.
	body := fmt.Sprintf("%+v", out.Workflows[0])
	if strings.Contains(body, "secret prompt body") {
		t.Errorf("workflow_list MUST trim prompts; got %q", body)
	}
}

func TestWorkflowList_RequiresCwd(t *testing.T) {
	b := &mcpBridge{tabID: 1}
	res, _, err := b.workflowListTool(context.Background(), newCallToolReq(), workflowListInput{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("missing cwd should produce IsError")
	}
}

func TestWorkflowList_PropagatesConfigReadError(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".config", "ask", "ask.json")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir config-as-dir: %v", err)
	}
	b := &mcpBridge{tabID: 1}
	b.setCwd(home)
	_, _, err := b.workflowListTool(context.Background(), newCallToolReq(), workflowListInput{})
	if err == nil {
		t.Fatal("workflow_list should return a tool error when config cannot be read")
	}
}

// ----- workflow_get -----

func TestWorkflowGet_HappyPath(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name: "one",
		Steps: []workflowStep{
			{Name: "a", Provider: "fake", Prompt: "do things"},
		},
	}})
	res, out, err := b.workflowGetTool(context.Background(), newCallToolReq(), workflowGetInput{Name: "one"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Errorf("happy path should not error: %+v", res)
	}
	if out.Workflow.Name != "one" {
		t.Errorf("name: got %q", out.Workflow.Name)
	}
	if len(out.Workflow.Steps) != 1 || out.Workflow.Steps[0].Prompt != "do things" {
		t.Errorf("step prompt should be returned in full; got %+v", out.Workflow.Steps)
	}
}

func TestWorkflowGet_NotFound(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	res, _, err := b.workflowGetTool(context.Background(), newCallToolReq(), workflowGetInput{Name: "missing"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("missing workflow should yield IsError; got %+v", res)
	}
}

func TestWorkflowGet_RejectsBlankName(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	res, _, err := b.workflowGetTool(context.Background(), newCallToolReq(), workflowGetInput{Name: "  "})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("blank name should error; got %+v", res)
	}
}

// ----- workflow_create -----

func TestWorkflowCreate_Persists(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	in := workflowCreateInput{
		Name: "alpha",
		Steps: []workflowStepView{
			{Name: "build", Provider: "fake", Model: "m1", Prompt: "p1"},
			{Name: "review", Provider: "fake", Prompt: "p2"},
		},
	}
	res, out, err := b.workflowCreateTool(context.Background(), newCallToolReq(), in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("happy path should not error: %+v", res)
	}
	if out.Workflow.Name != "alpha" {
		t.Errorf("output name: got %q want alpha", out.Workflow.Name)
	}
	saved := readWorkflows(t, cwd)
	if len(saved) != 1 || saved[0].Name != "alpha" || len(saved[0].Steps) != 2 {
		t.Errorf("not persisted as expected: %+v", saved)
	}
	if saved[0].Steps[0].Prompt != "p1" || saved[0].Steps[0].Model != "m1" {
		t.Errorf("step 0 fields lost: %+v", saved[0].Steps[0])
	}
}

func TestWorkflowCreate_RejectsEmptyName(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	res, _, err := b.workflowCreateTool(context.Background(), newCallToolReq(), workflowCreateInput{Name: "  "})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("empty name must yield IsError")
	}
}

func TestWorkflowCreate_RejectsDuplicateName(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{Name: "alpha"}})
	res, _, err := b.workflowCreateTool(context.Background(), newCallToolReq(), workflowCreateInput{Name: "alpha"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("duplicate name must yield IsError")
	}
	if got := readWorkflows(t, cwd); len(got) != 1 {
		t.Errorf("duplicate-create attempt must not append; got %+v", got)
	}
}

func TestWorkflowCreate_RejectsUnknownProvider(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	in := workflowCreateInput{
		Name: "alpha",
		Steps: []workflowStepView{
			{Name: "s1", Provider: "no-such-provider"},
		},
	}
	res, _, err := b.workflowCreateTool(context.Background(), newCallToolReq(), in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("unknown provider must yield IsError")
	}
	if got := readWorkflows(t, cwd); len(got) != 0 {
		t.Errorf("rejected create must not persist; got %+v", got)
	}
}

func TestWorkflowCreate_RejectsEmptyStepName(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	in := workflowCreateInput{
		Name: "alpha",
		Steps: []workflowStepView{
			{Name: "  ", Provider: "fake"},
		},
	}
	res, _, err := b.workflowCreateTool(context.Background(), newCallToolReq(), in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("empty step name must yield IsError")
	}
}

func TestWorkflowCreate_RejectsEmptyProvider(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	in := workflowCreateInput{
		Name: "alpha",
		Steps: []workflowStepView{
			{Name: "s1", Provider: ""},
		},
	}
	res, _, err := b.workflowCreateTool(context.Background(), newCallToolReq(), in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("empty provider must yield IsError")
	}
}

func TestWorkflowCreate_AllowsEmptySteps(t *testing.T) {
	// Empty steps are allowed at create time — the user can fill them
	// in later via workflow_edit. This matches the builder UI which
	// also creates empty-stepped workflows.
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	res, out, err := b.workflowCreateTool(context.Background(), newCallToolReq(), workflowCreateInput{Name: "stub"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("empty steps allowed at create; got %+v", res)
	}
	if out.Workflow.Name != "stub" {
		t.Errorf("output name: got %q want stub", out.Workflow.Name)
	}
	saved := readWorkflows(t, cwd)
	if len(saved) != 1 || saved[0].Name != "stub" {
		t.Errorf("expected one stub workflow on disk; got %+v", saved)
	}
}

// ----- workflow_edit -----

func TestWorkflowEdit_RenameOnly(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "old",
		Steps: []workflowStep{{Name: "s1", Provider: "fake", Prompt: "keep me"}},
	}})
	res, out, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
		Name:    "old",
		NewName: "new",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("rename should not error: %+v", res)
	}
	if out.Workflow.Name != "new" {
		t.Errorf("output name: got %q want new", out.Workflow.Name)
	}
	saved := readWorkflows(t, cwd)
	if len(saved) != 1 || saved[0].Name != "new" {
		t.Errorf("not renamed on disk: %+v", saved)
	}
	// Steps must be preserved on rename-only.
	if len(saved[0].Steps) != 1 || saved[0].Steps[0].Prompt != "keep me" {
		t.Errorf("rename-only must preserve steps; got %+v", saved[0].Steps)
	}
}

func TestWorkflowEdit_ReplaceSteps(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "alpha",
		Steps: []workflowStep{{Name: "old-step", Provider: "fake"}},
	}})
	newSteps := []workflowStepView{
		{Name: "first", Provider: "fake", Prompt: "1"},
		{Name: "second", Provider: "fake", Prompt: "2"},
	}
	res, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
		Name:  "alpha",
		Steps: &newSteps,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("replace should not error: %+v", res)
	}
	saved := readWorkflows(t, cwd)
	if len(saved) != 1 || len(saved[0].Steps) != 2 {
		t.Fatalf("steps not replaced: %+v", saved)
	}
	if saved[0].Steps[0].Name != "first" || saved[0].Steps[1].Name != "second" {
		t.Errorf("step ordering wrong: %+v", saved[0].Steps)
	}
}

func TestWorkflowEdit_ReplaceWithEmptySteps(t *testing.T) {
	// Edge case: Steps non-nil but len=0 means "clear all steps" —
	// the user wants the workflow stripped to an empty pipeline.
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name: "alpha",
		Steps: []workflowStep{
			{Name: "s1", Provider: "fake"},
			{Name: "s2", Provider: "fake"},
		},
	}})
	empty := []workflowStepView{}
	res, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
		Name:  "alpha",
		Steps: &empty,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("clearing steps must succeed; got %+v", res)
	}
	saved := readWorkflows(t, cwd)
	if len(saved) != 1 || len(saved[0].Steps) != 0 {
		t.Errorf("steps not cleared: %+v", saved[0].Steps)
	}
}

func TestWorkflowEdit_KeepsStepsWhenNil(t *testing.T) {
	// When Steps is nil (omitted), the workflow's existing steps must
	// be preserved untouched.
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name: "alpha",
		Steps: []workflowStep{
			{Name: "keepme", Provider: "fake", Prompt: "p"},
		},
	}})
	// Note: not setting Steps at all — this is the "rename only" case
	// but we also verify steps survive even without explicit rename.
	res, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{Name: "alpha"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("no-op edit should succeed; got %+v", res)
	}
	saved := readWorkflows(t, cwd)
	if len(saved[0].Steps) != 1 || saved[0].Steps[0].Prompt != "p" {
		t.Errorf("nil Steps must preserve existing; got %+v", saved[0].Steps)
	}
}

func TestWorkflowEdit_NotFound(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	res, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{Name: "nope"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("missing workflow must yield IsError")
	}
}

func TestWorkflowEdit_RejectsRenameOntoExistingName(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{
		{Name: "alpha"},
		{Name: "beta"},
	})
	res, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
		Name:    "alpha",
		NewName: "beta",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("rename collision must yield IsError")
	}
	saved := readWorkflows(t, cwd)
	if len(saved) != 2 || saved[0].Name != "alpha" {
		t.Errorf("collision must not mutate disk; got %+v", saved)
	}
}

func TestWorkflowEdit_RejectsBlankNewName(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{Name: "alpha"}})
	res, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
		Name:    "alpha",
		NewName: "   ",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("blank new_name must yield IsError; got %+v", res)
	}
}

func TestWorkflowEdit_RejectsUnknownProviderInSteps(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{Name: "alpha"}})
	bad := []workflowStepView{{Name: "s1", Provider: "ghost"}}
	res, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
		Name:  "alpha",
		Steps: &bad,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("unknown provider in edit must yield IsError")
	}
}

func TestWorkflowEdit_RejectsRunningWorkflow(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{Name: "alpha"}})
	resetWorkflowTrackerForTest()
	t.Cleanup(resetWorkflowTrackerForTest)
	workflowTracker().markWorking(cwd, "k1", "alpha", 7)
	res, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
		Name:    "alpha",
		NewName: "alpha2",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("editing a running workflow must yield IsError")
	}
	if got := readWorkflows(t, cwd); got[0].Name != "alpha" {
		t.Errorf("rejected edit must not mutate disk; got %+v", got)
	}
}

func TestWorkflowEdit_RejectsRenameOntoRunningWorkflow(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{Name: "alpha"}, {Name: "beta"}})
	resetWorkflowTrackerForTest()
	t.Cleanup(resetWorkflowTrackerForTest)
	workflowTracker().markWorking(cwd, "k1", "beta", 1)
	res, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
		Name:    "alpha",
		NewName: "beta",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("renaming onto a running workflow's name must yield IsError; got %+v", res)
	}
}

// ----- workflow_delete -----

func TestWorkflowDelete_HappyPath(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{Name: "alpha"}})
	res, out, err := b.workflowDeleteTool(context.Background(), newCallToolReq(), workflowDeleteInput{Name: "alpha"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("delete should succeed; got %+v", res)
	}
	if !out.Deleted {
		t.Errorf("output Deleted: got %v want true", out.Deleted)
	}
	if got := readWorkflows(t, cwd); len(got) != 0 {
		t.Errorf("not deleted on disk; got %+v", got)
	}
}

func TestWorkflowDelete_NotFound(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	res, _, err := b.workflowDeleteTool(context.Background(), newCallToolReq(), workflowDeleteInput{Name: "ghost"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("missing workflow must yield IsError")
	}
}

func TestWorkflowDelete_RejectsRunningWorkflow(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{Name: "alpha"}})
	resetWorkflowTrackerForTest()
	t.Cleanup(resetWorkflowTrackerForTest)
	workflowTracker().markWorking(cwd, "k1", "alpha", 1)
	res, _, err := b.workflowDeleteTool(context.Background(), newCallToolReq(), workflowDeleteInput{Name: "alpha"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("deleting a running workflow must yield IsError")
	}
	if got := readWorkflows(t, cwd); len(got) != 1 {
		t.Errorf("rejected delete must not mutate disk; got %+v", got)
	}
}

func TestWorkflowDelete_RejectsBlankName(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	res, _, err := b.workflowDeleteTool(context.Background(), newCallToolReq(), workflowDeleteInput{Name: ""})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("blank name must yield IsError")
	}
}

// ----- workflow_run -----

func TestWorkflowRun_DispatchesSpawnMessage(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 42)
	seedWorkflows(t, cwd, []workflowDef{{
		Name: "alpha",
		Steps: []workflowStep{
			{Name: "s1", Provider: "fake"},
		},
	}})
	captured := installSpawnCaptor(t)
	res, out, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{
		Name:   "alpha",
		Append: "extra context",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("run should succeed; got %+v", res)
	}
	if out.Workflow != "alpha" {
		t.Errorf("output workflow: got %q want alpha", out.Workflow)
	}
	if !strings.HasPrefix(out.SessionKey, "mcp:42:") {
		t.Errorf("session key should be tabID-prefixed; got %q", out.SessionKey)
	}
	if out.StartedAt == "" {
		t.Errorf("started_at should be populated")
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(*captured))
	}
	msg := (*captured)[0]
	if msg.OriginTabID != 42 {
		t.Errorf("OriginTabID: got %d want 42", msg.OriginTabID)
	}
	if msg.Cwd != cwd {
		t.Errorf("Cwd: got %q want %q", msg.Cwd, cwd)
	}
	if msg.Workflow.Name != "alpha" {
		t.Errorf("Workflow.Name: got %q want alpha", msg.Workflow.Name)
	}
	if msg.Source.Kind != workflowSourceText {
		t.Errorf("Source.Kind: got %v want workflowSourceText", msg.Source.Kind)
	}
	if msg.Source.TextAppend != "extra context" {
		t.Errorf("Source.TextAppend: got %q want extra context", msg.Source.TextAppend)
	}
	if msg.Source.Key() != out.SessionKey {
		t.Errorf("Source.Key %q != output SessionKey %q", msg.Source.Key(), out.SessionKey)
	}
}

func TestWorkflowRun_AppendEmptyOmitsRefBlock(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "alpha",
		Steps: []workflowStep{{Name: "s1", Provider: "fake"}},
	}})
	captured := installSpawnCaptor(t)
	res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "alpha"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("run should succeed: %+v", res)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(*captured))
	}
	if got := (*captured)[0].Source.RefBlock(); got != "" {
		t.Errorf("empty append should yield empty RefBlock; got %q", got)
	}
}

func TestWorkflowRun_NotFound(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	captured := installSpawnCaptor(t)
	res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "ghost"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("missing workflow must yield IsError")
	}
	if len(*captured) != 0 {
		t.Errorf("rejected run must not dispatch; got %d", len(*captured))
	}
}

func TestWorkflowRun_RejectsEmptyStepWorkflow(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{Name: "stub"}})
	captured := installSpawnCaptor(t)
	res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "stub"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("empty-step workflow must not run; got %+v", res)
	}
	if len(*captured) != 0 {
		t.Errorf("must not dispatch; got %d", len(*captured))
	}
}

func TestWorkflowRun_RejectsBadProviderInSteps(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	// Seed a workflow with an unregistered provider — simulates a
	// disk file that survived a refactor that removed the provider.
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "bad",
		Steps: []workflowStep{{Name: "s1", Provider: "ghost"}},
	}})
	captured := installSpawnCaptor(t)
	res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "bad"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("bad provider on disk must yield IsError; got %+v", res)
	}
	if len(*captured) != 0 {
		t.Errorf("must not dispatch; got %d", len(*captured))
	}
}

func TestWorkflowRun_AskUINotReady(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "alpha",
		Steps: []workflowStep{{Name: "s1", Provider: "fake"}},
	}})
	installFailingSpawn(t, "ask UI not ready")
	res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "alpha"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("UI-not-ready must yield IsError")
	}
}

// ----- Concurrency -----

// TestWorkflowMCP_ConcurrentEditsAreSerialized fires N goroutines
// running workflow_edit on the same workflow; without the config
// mutex one writer would clobber another and the final state would
// not reflect every successful write. With the lock, the saves are
// serial and the final state must be one of the inputs.
//
// Run this with `go test -race` to also catch data-race bugs in the
// surrounding state.
func TestWorkflowMCP_ConcurrentEditsAreSerialized(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "alpha",
		Steps: []workflowStep{{Name: "s0", Provider: "fake"}},
	}})

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			steps := []workflowStepView{
				{Name: fmt.Sprintf("s-%d", i), Provider: "fake", Prompt: fmt.Sprintf("p-%d", i)},
			}
			_, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
				Name:  "alpha",
				Steps: &steps,
			})
			if err != nil {
				t.Errorf("goroutine %d errored: %v", i, err)
			}
		}()
	}
	wg.Wait()

	saved := readWorkflows(t, cwd)
	if len(saved) != 1 {
		t.Fatalf("workflow count after concurrent edits: %d (want 1)", len(saved))
	}
	if len(saved[0].Steps) != 1 {
		t.Fatalf("steps after concurrent edits: %d (want 1)", len(saved[0].Steps))
	}
	stepName := saved[0].Steps[0].Name
	if !strings.HasPrefix(stepName, "s-") {
		t.Errorf("final step must be from one of the goroutines; got %q", stepName)
	}
}

// TestWorkflowMCP_ConcurrentCreateDoesNotDuplicate fires N create
// calls with distinct names; every one must land. With the lock
// nothing is lost; without the lock, last-writer-wins on the items
// slice and we'd see fewer than N entries.
func TestWorkflowMCP_ConcurrentCreateDoesNotDuplicate(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	const N = 30
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, _, err := b.workflowCreateTool(context.Background(), newCallToolReq(), workflowCreateInput{
				Name: fmt.Sprintf("wf-%02d", i),
			})
			if err != nil {
				t.Errorf("goroutine %d errored: %v", i, err)
			}
		}()
	}
	wg.Wait()
	saved := readWorkflows(t, cwd)
	if len(saved) != N {
		t.Fatalf("expected %d workflows, got %d (lost writes!)", N, len(saved))
	}
	seen := make(map[string]struct{}, N)
	for _, w := range saved {
		seen[w.Name] = struct{}{}
	}
	if len(seen) != N {
		t.Errorf("expected %d unique names, got %d", N, len(seen))
	}
}

func TestWorkflowMCP_EditAndTrackerFinalPreserveConfigSlices(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "alpha",
		Steps: []workflowStep{{Name: "s0", Provider: "fake"}},
	}})
	resetWorkflowTrackerForTest()
	t.Cleanup(resetWorkflowTrackerForTest)

	const N = 30
	var wg sync.WaitGroup
	wg.Add(2 * N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			steps := []workflowStepView{{
				Name:     fmt.Sprintf("edit-%02d", i),
				Provider: "fake",
				Prompt:   fmt.Sprintf("prompt-%02d", i),
			}}
			_, _, err := b.workflowEditTool(context.Background(), newCallToolReq(), workflowEditInput{
				Name:  "alpha",
				Steps: &steps,
			})
			if err != nil {
				t.Errorf("edit %d: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			workflowTracker().markFinal(cwd, fmt.Sprintf("session-%02d", i),
				"alpha", workflowStatusDone, i)
		}()
	}
	wg.Wait()

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	pc := loadProjectConfig(cfg, cwd)
	if len(pc.Workflows.Items) != 1 || pc.Workflows.Items[0].Name != "alpha" {
		t.Fatalf("workflow items were lost: %+v", pc.Workflows.Items)
	}
	if len(pc.Workflows.Items[0].Steps) != 1 {
		t.Fatalf("workflow steps corrupted: %+v", pc.Workflows.Items[0].Steps)
	}
	if len(pc.Workflows.Sessions) != N {
		t.Fatalf("workflow sessions lost: got %d want %d (%+v)",
			len(pc.Workflows.Sessions), N, pc.Workflows.Sessions)
	}
}

// ----- Helpers under test -----

func TestWorkflowSourceText_RefBlock(t *testing.T) {
	src := textWorkflowSource(7, "  hello world  ")
	if got := src.Kind; got != workflowSourceText {
		t.Errorf("Kind: got %v want workflowSourceText", got)
	}
	if !strings.HasPrefix(src.Key(), "mcp:7:") {
		t.Errorf("Key prefix: got %q want mcp:7:*", src.Key())
	}
	if src.TextAppend != "hello world" {
		t.Errorf("TextAppend trim: got %q want hello world", src.TextAppend)
	}
	if got := src.RefBlock(); got != "Reference:\nhello world" {
		t.Errorf("RefBlock: got %q want %q", got, "Reference:\nhello world")
	}
}

func TestWorkflowSourceText_EmptyRefBlock(t *testing.T) {
	src := textWorkflowSource(1, "   ")
	if got := src.RefBlock(); got != "" {
		t.Errorf("empty append should yield empty RefBlock; got %q", got)
	}
	if !strings.Contains(src.Display(), "empty") {
		t.Errorf("Display should label empty source: got %q", src.Display())
	}
}

func TestWorkflowSourceText_KeyIsUniquePerCall(t *testing.T) {
	// Two consecutive runs on the same tab MUST produce distinct
	// keys, otherwise the workflow tracker entry from the first run
	// would be overwritten by the second.
	a := textWorkflowSource(3, "a")
	b := textWorkflowSource(3, "b")
	if a.Key() == b.Key() {
		t.Errorf("two runs on same tab produced identical keys: %q == %q", a.Key(), b.Key())
	}
}

// TestWorkflowMCP_RegisterDoesNotPanic guards the wiring path: every
// bridge built via newMCPBridge must be able to register the workflow
// tools without a panic (e.g. from duplicate tool names colliding
// with ask_user_question / approval_prompt).
func TestWorkflowMCP_RegisterDoesNotPanic(t *testing.T) {
	b, err := newMCPBridge(99)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}
	defer b.stop()
	// Smoke test the bridge accepts a workflow_list call (no real
	// network traffic — just calls the handler directly through the
	// bridge struct).
	cwd := isolateHome(t)
	b.setCwd(cwd)
	_, _, err = b.workflowListTool(context.Background(), newCallToolReq(), workflowListInput{})
	if err != nil {
		t.Errorf("workflowListTool after newMCPBridge: %v", err)
	}
}

func TestWorkflowMCP_CallToolWireDecodesEditSteps(t *testing.T) {
	cwd := isolateHome(t)
	withRegisteredProviders(t, newFakeProvider())
	b, err := newMCPBridge(100)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}
	defer b.stop()
	b.setCwd(cwd)
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "alpha",
		Steps: []workflowStep{{Name: "old", Provider: "fake"}},
	}})
	session := connectWorkflowMCPClient(t, b.server)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "workflow_edit",
		Arguments: map[string]any{
			"name":     "alpha",
			"new_name": "beta",
			"steps": []map[string]any{{
				"name":     "review",
				"provider": "fake",
				"model":    "m1",
				"prompt":   "check it",
			}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool workflow_edit: %v", err)
	}
	if res.IsError {
		t.Fatalf("workflow_edit returned IsError: %+v", res.Content)
	}
	var out workflowEditOutput
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured output: %v", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal structured output: %v; data=%s", err, data)
	}
	if out.Workflow.Name != "beta" || len(out.Workflow.Steps) != 1 ||
		out.Workflow.Steps[0].Prompt != "check it" {
		t.Fatalf("unexpected structured output: %+v", out.Workflow)
	}
	saved := readWorkflows(t, cwd)
	if len(saved) != 1 || saved[0].Name != "beta" ||
		len(saved[0].Steps) != 1 || saved[0].Steps[0].Prompt != "check it" {
		t.Fatalf("wire call did not persist edit: %+v", saved)
	}
}

// TestWorkflowMCP_TenantsByCwd verifies that two bridges with
// different cwds see independent workflow lists. This is the
// "chat-tab CRUD only ever touches the project the tab is in"
// invariant from the design.
func TestWorkflowMCP_TenantsByCwd(t *testing.T) {
	isolateHome(t)
	withRegisteredProviders(t, newFakeProvider())

	cwdA := t.TempDir()
	cwdB := t.TempDir()
	bA := &mcpBridge{tabID: 1}
	bA.setCwd(cwdA)
	bB := &mcpBridge{tabID: 2}
	bB.setCwd(cwdB)

	if _, _, err := bA.workflowCreateTool(context.Background(), newCallToolReq(),
		workflowCreateInput{Name: "in-a"}); err != nil {
		t.Fatalf("create in A: %v", err)
	}
	if _, _, err := bB.workflowCreateTool(context.Background(), newCallToolReq(),
		workflowCreateInput{Name: "in-b"}); err != nil {
		t.Fatalf("create in B: %v", err)
	}

	_, outA, err := bA.workflowListTool(context.Background(), newCallToolReq(), workflowListInput{})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	_, outB, err := bB.workflowListTool(context.Background(), newCallToolReq(), workflowListInput{})
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(outA.Workflows) != 1 || outA.Workflows[0].Name != "in-a" {
		t.Errorf("A leaked or missed; got %+v", outA.Workflows)
	}
	if len(outB.Workflows) != 1 || outB.Workflows[0].Name != "in-b" {
		t.Errorf("B leaked or missed; got %+v", outB.Workflows)
	}
}
