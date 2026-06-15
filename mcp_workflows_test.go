package main

import (
	"context"

	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// workflowTestTenant is the post-bridge stand-in: a cwd + tabID with
// thin delegators onto the shared cores, so the suite below exercises
// exactly what the native workflow tools run.
type workflowTestTenant struct {
	cwd   string
	tabID int
}

func (b workflowTestTenant) workflowListTool(_ context.Context, _ *mcp.CallToolRequest, in workflowListInput) (*mcp.CallToolResult, workflowListOutput, error) {
	return workflowListCore(b.cwd, in)
}

func (b workflowTestTenant) workflowGetTool(_ context.Context, _ *mcp.CallToolRequest, in workflowGetInput) (*mcp.CallToolResult, workflowGetOutput, error) {
	return workflowGetCore(b.cwd, in)
}

func (b workflowTestTenant) workflowCreateTool(_ context.Context, _ *mcp.CallToolRequest, in workflowCreateInput) (*mcp.CallToolResult, workflowCreateOutput, error) {
	return workflowCreateCore(b.cwd, in)
}

func (b workflowTestTenant) workflowEditTool(_ context.Context, _ *mcp.CallToolRequest, in workflowEditInput) (*mcp.CallToolResult, workflowEditOutput, error) {
	return workflowEditCore(b.cwd, in)
}

func (b workflowTestTenant) workflowDeleteTool(_ context.Context, _ *mcp.CallToolRequest, in workflowDeleteInput) (*mcp.CallToolResult, workflowDeleteOutput, error) {
	return workflowDeleteCore(b.cwd, in)
}

func (b workflowTestTenant) workflowRunTool(_ context.Context, _ *mcp.CallToolRequest, in workflowRunInput) (*mcp.CallToolResult, workflowRunOutput, error) {
	return workflowRunCore(b.cwd, b.tabID, in)
}

func (b workflowTestTenant) workflowCopyTool(_ context.Context, _ *mcp.CallToolRequest, in workflowCopyInput) (*mcp.CallToolResult, workflowCopyOutput, error) {
	return workflowCopyCore(b.cwd, in)
}

// newWorkflowMCPTestBridge stages a tenant bound to a tmp HOME + a
// project cwd inside it, with the fake provider registered so
// `validateProviderID` accepts the "fake" id. Restores the provider
// registry on test cleanup.
func newWorkflowMCPTestBridge(t *testing.T, tabID int) (workflowTestTenant, string) {
	t.Helper()
	cwd := isolateHome(t)
	withRegisteredProviders(t, newFakeProvider())
	return workflowTestTenant{cwd: cwd, tabID: tabID}, cwd
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
	b := workflowTestTenant{tabID: 1}
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
	b := workflowTestTenant{cwd: home, tabID: 1}
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

func TestWorkflowRun_RejectsEmptyAppend(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "alpha",
		Steps: []workflowStep{{Name: "s1", Provider: "fake"}},
	}})
	captured := installSpawnCaptor(t)
	// append is the only context channel into the fresh workflow session,
	// so an empty/whitespace value is rejected before dispatch — the model
	// must submit the full plan.
	for _, append := range []string{"", "   \n\t "} {
		res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "alpha", Append: append})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !res.IsError {
			t.Errorf("empty append %q must yield IsError", append)
		}
		if !strings.Contains(mcpResultText(res), "append is required") {
			t.Errorf("error must mention append is required; got %q", mcpResultText(res))
		}
	}
	if len(*captured) != 0 {
		t.Errorf("rejected run must not dispatch; got %d", len(*captured))
	}
}

func TestWorkflowRun_NotFound(t *testing.T) {
	b, _ := newWorkflowMCPTestBridge(t, 1)
	captured := installSpawnCaptor(t)
	res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "ghost", Append: "plan"})
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
	res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "stub", Append: "plan"})
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
	res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "bad", Append: "plan"})
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
	res, _, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "alpha", Append: "plan"})
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

func TestWorkflowNative_EditDecodesStepsJSON(t *testing.T) {
	cwd := isolateHome(t)
	withRegisteredProviders(t, newFakeProvider())
	seedWorkflows(t, cwd, []workflowDef{{
		Name:  "alpha",
		Steps: []workflowStep{{Name: "old", Provider: "fake"}},
	}})
	env := newAgentToolEnv(cwd, 1, true, true, nil)
	var tool fantasy.AgentTool
	for _, bt := range agentWorkflowTools(env) {
		if bt.Info().Name == "workflow_edit" {
			tool = bt
		}
	}
	if tool == nil {
		t.Fatal("workflow_edit core tool missing")
	}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID: "1", Name: "workflow_edit",
		Input: `{"name":"alpha","new_name":"beta","steps":[{"name":"review","provider":"fake","model":"m1","prompt":"check it"}]}`,
	})
	if err != nil {
		t.Fatalf("workflow_edit: %v", err)
	}
	if resp.IsError {
		t.Fatalf("workflow_edit errored: %s", resp.Content)
	}
	saved := readWorkflows(t, cwd)
	if len(saved) != 1 || saved[0].Name != "beta" ||
		len(saved[0].Steps) != 1 || saved[0].Steps[0].Prompt != "check it" {
		t.Fatalf("native edit did not persist: %+v", saved)
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
	bA := workflowTestTenant{cwd: cwdA, tabID: 1}
	bB := workflowTestTenant{cwd: cwdB, tabID: 2}

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

// ----- loops -----

// TestWorkflowCreate_LoopRoundTrip creates a workflow with a loop step
// via the MCP tool and asserts the loop shape persists to disk and reads
// back through workflow_get.
func TestWorkflowCreate_LoopRoundTrip(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	in := workflowCreateInput{
		Name: "cr",
		Steps: []workflowStepView{
			{Name: "scaffold", Provider: "fake", Prompt: "build"},
			{Name: "qa", Kind: "loop", MaxIterations: 3, ExitCondition: "tests green",
				Steps: []workflowInnerStepView{
					{Name: "fix", Provider: "fake", Prompt: "fix it"},
					{Name: "review", Provider: "fake", Model: "m-one", Prompt: "review it"},
				}},
			{Name: "ship", Provider: "fake", Prompt: "finalize"},
		},
	}
	res, _, err := b.workflowCreateTool(context.Background(), newCallToolReq(), in)
	if err != nil {
		t.Fatalf("create err: %v", err)
	}
	if res.IsError {
		t.Fatalf("create should succeed; got %+v", res)
	}

	items := readWorkflows(t, cwd)
	if len(items) != 1 || len(items[0].Steps) != 3 {
		t.Fatalf("disk shape: %+v", items)
	}
	loop := items[0].Steps[1]
	if !loop.isLoop() {
		t.Fatalf("step 1 should be a loop; got kind %q", loop.Kind)
	}
	if loop.MaxIterations != 3 || loop.ExitCondition != "tests green" {
		t.Errorf("loop config not persisted: maxIter=%d exit=%q", loop.MaxIterations, loop.ExitCondition)
	}
	if loop.Provider != "" || loop.Prompt != "" {
		t.Errorf("loop step should carry no provider/prompt; got provider=%q prompt=%q", loop.Provider, loop.Prompt)
	}
	if len(loop.Steps) != 2 || loop.Steps[1].Name != "review" || loop.Steps[1].Model != "m-one" {
		t.Errorf("inner steps not persisted: %+v", loop.Steps)
	}

	_, gout, err := b.workflowGetTool(context.Background(), newCallToolReq(), workflowGetInput{Name: "cr"})
	if err != nil {
		t.Fatalf("get err: %v", err)
	}
	gloop := gout.Workflow.Steps[1]
	if gloop.Kind != "loop" || gloop.MaxIterations != 3 || len(gloop.Steps) != 2 {
		t.Errorf("get loop view: %+v", gloop)
	}
	if gloop.Steps[0].Name != "fix" || gloop.Steps[1].Name != "review" {
		t.Errorf("get inner steps: %+v", gloop.Steps)
	}
}

// TestValidateSteps_LoopRules covers the loop-specific validation: empty
// loop, negative cap, and bad inner provider are rejected; a valid loop
// and an unknown kind are handled correctly.
func TestValidateSteps_LoopRules(t *testing.T) {
	withRegisteredProviders(t, newFakeProvider())

	if err := validateSteps([]workflowStepView{{Name: "l", Kind: "loop"}}); err == nil {
		t.Error("a loop with no inner steps should be rejected")
	}
	if err := validateSteps([]workflowStepView{{
		Name: "l", Kind: "loop", MaxIterations: -1,
		Steps: []workflowInnerStepView{{Name: "a", Provider: "fake"}},
	}}); err == nil {
		t.Error("negative maxIterations should be rejected")
	}
	if err := validateSteps([]workflowStepView{{
		Name: "l", Kind: "loop",
		Steps: []workflowInnerStepView{{Name: "a", Provider: "nope"}},
	}}); err == nil {
		t.Error("an inner step with an unknown provider should be rejected")
	}
	if err := validateSteps([]workflowStepView{{
		Name: "l", Kind: "loop",
		Steps: []workflowInnerStepView{{Name: "a", Provider: "fake"}},
	}}); err != nil {
		t.Errorf("a valid loop should pass; got %v", err)
	}
	if err := validateSteps([]workflowStepView{{Name: "x", Kind: "bogus"}}); err == nil {
		t.Error("an unknown step kind should be rejected")
	}
}

// ----- scopes -----

// TestWorkflowTools_ScopedLifecycle runs the full two-scope story
// through the tool cores: create in repo scope, cross-scope duplicate
// names, ambiguity errors on mutation, scoped edit/delete, copy with
// conflict handling, and scope tags in list/get output.
func TestWorkflowTools_ScopedLifecycle(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 1)
	resetWorkflowTrackerForTest()
	ctx := context.Background()

	// Create one per scope under the same name.
	res, _, err := b.workflowCreateTool(ctx, newCallToolReq(), workflowCreateInput{
		Name: "review", Scope: "repo",
		Steps: []workflowStepView{{Name: "r-step", Provider: "fake"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("repo create: err=%v res=%+v", err, res)
	}
	if _, statErr := os.Stat(filepath.Join(cwd, ".ask", "workflows", "review.json")); statErr != nil {
		t.Fatalf("repo create must write the file: %v", statErr)
	}
	res, _, err = b.workflowCreateTool(ctx, newCallToolReq(), workflowCreateInput{
		Name:  "review", // no scope → user; cross-scope duplicate is legal
		Steps: []workflowStepView{{Name: "u-step", Provider: "fake"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("user create with cross-scope duplicate name: err=%v res=%+v", err, res)
	}
	// Same-scope duplicate is not.
	res, _, _ = b.workflowCreateTool(ctx, newCallToolReq(), workflowCreateInput{Name: "review", Scope: "repo"})
	if res == nil || !res.IsError {
		t.Error("same-scope duplicate create must error")
	}
	// Unknown scope rejected.
	res, _, _ = b.workflowCreateTool(ctx, newCallToolReq(), workflowCreateInput{Name: "x", Scope: "global"})
	if res == nil || !res.IsError {
		t.Error("unknown scope must error")
	}

	// List shows both with scope tags, repo first.
	_, listOut, err := b.workflowListTool(ctx, newCallToolReq(), workflowListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listOut.Workflows) != 2 || listOut.Workflows[0].Scope != "repo" || listOut.Workflows[1].Scope != "user" {
		t.Fatalf("list scopes wrong: %+v", listOut.Workflows)
	}

	// Get without scope prefers the repo copy; explicit scope picks.
	_, getOut, _ := b.workflowGetTool(ctx, newCallToolReq(), workflowGetInput{Name: "review"})
	if getOut.Workflow.Scope != "repo" || getOut.Workflow.Steps[0].Name != "r-step" {
		t.Errorf("get should prefer repo; got %+v", getOut.Workflow)
	}
	_, getOut, _ = b.workflowGetTool(ctx, newCallToolReq(), workflowGetInput{Name: "review", Scope: "user"})
	if getOut.Workflow.Scope != "user" || getOut.Workflow.Steps[0].Name != "u-step" {
		t.Errorf("scoped get wrong: %+v", getOut.Workflow)
	}

	// Mutations on an ambiguous name demand a scope.
	res, _, _ = b.workflowEditTool(ctx, newCallToolReq(), workflowEditInput{Name: "review", NewName: "review2"})
	if res == nil || !res.IsError || !strings.Contains(mcpResultText(res), "scope") {
		t.Errorf("ambiguous edit must demand scope; got %+v", res)
	}
	res, _, _ = b.workflowDeleteTool(ctx, newCallToolReq(), workflowDeleteInput{Name: "review"})
	if res == nil || !res.IsError {
		t.Error("ambiguous delete must demand scope")
	}
	res, _, _ = b.workflowRunTool(ctx, newCallToolReq(), workflowRunInput{Name: "review", Append: "plan"})
	if res == nil || !res.IsError {
		t.Error("ambiguous run must demand scope")
	}

	// Scoped edit touches only its copy.
	res, editOut, err := b.workflowEditTool(ctx, newCallToolReq(), workflowEditInput{
		Name: "review", Scope: "user", NewName: "review-local",
	})
	if err != nil || res.IsError {
		t.Fatalf("scoped edit: err=%v res=%+v", err, res)
	}
	if editOut.Workflow.Scope != "user" || editOut.Workflow.Name != "review-local" {
		t.Errorf("scoped edit output wrong: %+v", editOut.Workflow)
	}
	if _, ok := findWorkflow(cwd, "review", workflowScopeRepo); !ok {
		t.Error("repo copy must survive a user-scope rename")
	}

	// Copy repo → user (no conflict now), then conflict + new_name.
	res, copyOut, err := b.workflowCopyTool(ctx, newCallToolReq(), workflowCopyInput{Name: "review", To: "user"})
	if err != nil || res.IsError {
		t.Fatalf("copy: err=%v res=%v", err, mcpResultText(res))
	}
	if copyOut.Workflow.Scope != "user" || copyOut.Workflow.Name != "review" {
		t.Errorf("copy output wrong: %+v", copyOut.Workflow)
	}
	res, _, _ = b.workflowCopyTool(ctx, newCallToolReq(), workflowCopyInput{Name: "review", Scope: "repo", To: "user"})
	if res == nil || !res.IsError || !strings.Contains(mcpResultText(res), "new_name") {
		t.Errorf("conflicting copy must demand new_name; got %v", mcpResultText(res))
	}
	res, copyOut, _ = b.workflowCopyTool(ctx, newCallToolReq(), workflowCopyInput{
		Name: "review", Scope: "repo", To: "user", NewName: "review-fork",
	})
	if res.IsError || copyOut.Workflow.Name != "review-fork" {
		t.Errorf("new_name copy failed: %v %+v", mcpResultText(res), copyOut.Workflow)
	}
	// Missing `to` is rejected.
	res, _, _ = b.workflowCopyTool(ctx, newCallToolReq(), workflowCopyInput{Name: "review"})
	if res == nil || !res.IsError {
		t.Error("copy without `to` must error")
	}

	// Scoped delete removes the file and leaves the user copy.
	res, delOut, err := b.workflowDeleteTool(ctx, newCallToolReq(), workflowDeleteInput{Name: "review", Scope: "repo"})
	if err != nil || res.IsError || !delOut.Deleted {
		t.Fatalf("scoped delete: err=%v res=%+v", err, res)
	}
	if _, statErr := os.Stat(filepath.Join(cwd, ".ask", "workflows", "review.json")); !os.IsNotExist(statErr) {
		t.Error("repo file must be gone after scoped delete")
	}
	if _, ok := findWorkflow(cwd, "review", workflowScopeUser); !ok {
		t.Error("user copy must survive a repo-scope delete")
	}
}

// TestWorkflowRun_RepoScopedWorkflowDispatches pins that a workflow
// living only in repo scope (a committed file, no ask.json entry) is
// runnable end to end through workflow_run.
func TestWorkflowRun_RepoScopedWorkflowDispatches(t *testing.T) {
	b, cwd := newWorkflowMCPTestBridge(t, 7)
	resetWorkflowTrackerForTest()
	captured := installSpawnCaptor(t)
	if err := saveAllWorkflows(cwd, []workflowDef{{
		Name: "shared", Scope: workflowScopeRepo,
		Steps: []workflowStep{{Name: "s", Provider: "fake"}},
	}}); err != nil {
		t.Fatal(err)
	}
	res, out, err := b.workflowRunTool(context.Background(), newCallToolReq(), workflowRunInput{Name: "shared", Append: "plan"})
	if err != nil || res.IsError {
		t.Fatalf("run: err=%v res=%v", err, mcpResultText(res))
	}
	if out.Workflow != "shared" || len(*captured) != 1 || (*captured)[0].Workflow.Name != "shared" {
		t.Errorf("dispatch wrong: out=%+v captured=%+v", out, *captured)
	}
}
