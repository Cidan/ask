package main

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/watcher"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// useWatcherHub installs a fresh hub as the global one for the test, in an
// isolated home.
func useWatcherHub(t *testing.T) *watcherHub {
	t.Helper()
	isolateHome(t)
	h := newWatcherHub()
	prev := globalWatcherHub
	globalWatcherHub = h
	t.Cleanup(func() {
		h.close()
		globalWatcherHub = prev
	})
	return h
}

// stubScript stands in for every script the hub runs.
func stubScript(t *testing.T, fn func(context.Context, watcher.ScriptSpec) watcher.ScriptResult) {
	t.Helper()
	prev := watcher.RunScript
	watcher.RunScript = fn
	t.Cleanup(func() { watcher.RunScript = prev })
}

func printing(out string) func(context.Context, watcher.ScriptSpec) watcher.ScriptResult {
	return func(context.Context, watcher.ScriptSpec) watcher.ScriptResult {
		return watcher.ScriptResult{Stdout: out, StdoutBytes: len(out)}
	}
}

// stubModelRun stands in for the model; prompts records what it was handed.
func stubModelRun(t *testing.T, fn func(context.Context, *watcherHub, watcher.Def, *watcherLive, string) watcherModelOutcome) {
	t.Helper()
	prev := watcherModelRun
	watcherModelRun = fn
	t.Cleanup(func() { watcherModelRun = prev })
}

// testWatcherProjects gives each test one project for its watchers, so
// watchers a test saves by name can be looked up by name (wid).
var testWatcherProjects = map[*testing.T]string{}

func testWatcherProject(t *testing.T) string {
	t.Helper()
	if p, ok := testWatcherProjects[t]; ok {
		return p
	}
	p := t.TempDir()
	testWatcherProjects[t] = p
	t.Cleanup(func() { delete(testWatcherProjects, t) })
	return p
}

// wid is the id of the test's watcher called name.
func wid(t *testing.T, name string) string {
	t.Helper()
	return watcher.Ref{Project: testWatcherProject(t), Name: name}.ID()
}

func saveTestWatcher(t *testing.T, name, schedule string, mutate ...func(*watcher.Def)) watcher.Def {
	t.Helper()
	d := watcher.Def{
		Name:         name,
		Schedule:     schedule,
		Command:      "./check.sh",
		Cwd:          testWatcherProject(t),
		Instructions: "handle what it found",
		CreatedAt:    time.Now().Add(-time.Hour).UTC(),
	}
	for _, fn := range mutate {
		fn(&d)
	}
	saved, err := watcher.Save(watcher.Ref{}, d, nil)
	if err != nil {
		t.Fatalf("save watcher: %v", err)
	}
	return saved
}

func waitIdle(t *testing.T, h *watcherHub, name string) {
	t.Helper()
	id := wid(t, name)
	waitFor(t, func() bool { return !h.isLive(id) })
}

func TestWatcherHub_OneWindowLeads(t *testing.T) {
	a := useWatcherHub(t)
	b := newWatcherHub()
	t.Cleanup(b.close)
	saveTestWatcher(t, "prs", "daily 09:00")
	a.tick(time.Now())
	b.tick(time.Now())
	if !a.snapshot().leading || b.snapshot().leading {
		t.Fatalf("exactly the first window must lead: a=%v b=%v", a.snapshot().leading, b.snapshot().leading)
	}
	if pid := b.snapshot().state.Leader.PID; pid != os.Getpid() {
		t.Errorf("the follower must see the leader's pid, got %d", pid)
	}
	if len(b.snapshot().defs) != 1 {
		t.Error("a follower lists the watchers too")
	}
	a.close()
	b.tick(time.Now())
	if !b.snapshot().leading {
		t.Fatal("the lead must pass to the next window once the leader exits")
	}
}

func TestWatcherHub_NoOutputWakesNothing(t *testing.T) {
	h := useWatcherHub(t)
	stubScript(t, printing(" \n"))
	called := false
	stubModelRun(t, func(context.Context, *watcherHub, watcher.Def, *watcherLive, string) watcherModelOutcome {
		called = true
		return watcherModelOutcome{}
	})
	saveTestWatcher(t, "quiet", "every 5m")
	h.tick(time.Now())
	waitIdle(t, h, "quiet")
	st := h.snapshot().status(wid(t, "quiet"))
	if called || len(st.Runs) != 0 {
		t.Fatalf("blank output must not wake the model: called=%v runs=%d", called, len(st.Runs))
	}
	if st.LastNote != "no output" || st.LastEnded.IsZero() || st.Phase != watcher.PhaseIdle {
		t.Errorf("status after a quiet check = %+v", st)
	}
	if want := st.LastEnded.Add(5 * time.Minute); !st.NextRun.Equal(want) {
		t.Errorf("next run %v, want five minutes after the check ended (%v)", st.NextRun, want)
	}
}

func TestWatcherHub_OutputWakesTheModelOnceAndNeverBackToBack(t *testing.T) {
	h := useWatcherHub(t)
	var specs []watcher.ScriptSpec
	var mu sync.Mutex
	stubScript(t, func(_ context.Context, spec watcher.ScriptSpec) watcher.ScriptResult {
		mu.Lock()
		specs = append(specs, spec)
		mu.Unlock()
		return watcher.ScriptResult{Stdout: "#412 fix the race\n", StdoutBytes: 18, Stderr: "fetched 1 PR"}
	})
	release := make(chan struct{})
	var prompt string
	stubModelRun(t, func(_ context.Context, _ *watcherHub, _ watcher.Def, live *watcherLive, p string) watcherModelOutcome {
		prompt = p
		<-release
		return watcherModelOutcome{provider: "fake", model: "m", sessionID: "sess-1", sessionCwd: "/w", response: "Reviewed #412.", costUSD: 0.04, costKnown: true}
	})
	d := saveTestWatcher(t, "prs", "every 5m")

	h.tick(time.Now())
	waitFor(t, func() bool { return h.snapshot().status(wid(t, "prs")).Phase == watcher.PhaseModel })
	// The run takes longer than the interval: ticks meanwhile start nothing.
	for i := 0; i < 3; i++ {
		h.tick(time.Now().Add(10 * time.Minute))
	}
	mu.Lock()
	if len(specs) != 1 {
		t.Fatalf("a running watcher was checked again: %d checks", len(specs))
	}
	mu.Unlock()
	close(release)
	waitIdle(t, h, "prs")

	st := h.snapshot().status(wid(t, "prs"))
	if len(st.Runs) != 1 {
		t.Fatalf("runs = %+v", st.Runs)
	}
	r := st.Runs[0]
	if r.Status != watcher.RunDone || r.Response != "Reviewed #412." || r.SessionID != "sess-1" || !r.CostKnown || r.Output != "#412 fix the race\n" || r.Stderr != "fetched 1 PR" {
		t.Errorf("run = %+v", r)
	}
	if !strings.Contains(prompt, "<instructions>\nhandle what it found\n</instructions>") || !strings.Contains(prompt, "<output>\n#412 fix the race\n</output>") {
		t.Errorf("prompt = %q", prompt)
	}
	if want := st.LastEnded.Add(5 * time.Minute); !st.NextRun.Equal(want) {
		t.Errorf("next run %v, want five minutes after the run ended (%v)", st.NextRun, want)
	}
	h.tick(time.Now())
	if h.isLive(wid(t, "prs")) {
		t.Error("a check started right after the last one ended")
	}
	if specs[0].Env[0] != watcher.EnvName+"="+d.Name || specs[0].Dir != d.Cwd {
		t.Errorf("script spec = %+v", specs[0])
	}
	if !st.Unseen() {
		t.Error("a fresh result is unseen until the user opens it")
	}
}

func TestWatcherHub_ScriptFailureIsARunButWakesNothing(t *testing.T) {
	h := useWatcherHub(t)
	stubScript(t, func(context.Context, watcher.ScriptSpec) watcher.ScriptResult {
		return watcher.ScriptResult{ExitCode: 3, Stdout: "partial", Stderr: "token expired"}
	})
	stubModelRun(t, func(context.Context, *watcherHub, watcher.Def, *watcherLive, string) watcherModelOutcome {
		t.Error("a failed check must not wake the model")
		return watcherModelOutcome{}
	})
	saveTestWatcher(t, "broken", "every 1m")
	h.tick(time.Now())
	waitIdle(t, h, "broken")
	st := h.snapshot().status(wid(t, "broken"))
	if len(st.Runs) != 1 || st.Runs[0].Status != watcher.RunFailed || st.Runs[0].Error != "exit 3" || st.Runs[0].Stderr != "token expired" {
		t.Fatalf("runs = %+v", st.Runs)
	}
	hl := watcherHeadlineFor(watcher.Def{Name: "broken"}, st, time.Now())
	if hl.glyph != "✗" {
		t.Errorf("a failing watcher shows ✗, got %q", hl.glyph)
	}
}

func TestWatcherHub_PausedWatchersNeverRun(t *testing.T) {
	h := useWatcherHub(t)
	stubScript(t, func(context.Context, watcher.ScriptSpec) watcher.ScriptResult {
		t.Error("a paused watcher ran")
		return watcher.ScriptResult{}
	})
	saveTestWatcher(t, "off", "every 1m", func(d *watcher.Def) { d.Paused = true })
	h.tick(time.Now())
	if err := h.requestRun(wid(t, "off")); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Now())
	if h.isLive(wid(t, "off")) {
		t.Fatal("a paused watcher started")
	}
	if !h.snapshot().status(wid(t, "off")).RunRequested.IsZero() {
		t.Error("a run request on a paused watcher is dropped, not kept for later")
	}
}

func TestWatcherHub_RequestsFromAnotherWindow(t *testing.T) {
	leader := useWatcherHub(t)
	follower := newWatcherHub()
	t.Cleanup(follower.close)
	stubScript(t, printing("alert\n"))
	started := make(chan struct{}, 1)
	stubModelRun(t, func(ctx context.Context, _ *watcherHub, _ watcher.Def, _ *watcherLive, _ string) watcherModelOutcome {
		started <- struct{}{}
		<-ctx.Done()
		return watcherModelOutcome{provider: "fake"}
	})
	saveTestWatcher(t, "pager", "daily 09:00", func(d *watcher.Def) { d.CreatedAt = time.Now().UTC() })
	leader.tick(time.Now())
	follower.tick(time.Now())
	if leader.isLive(wid(t, "pager")) {
		t.Fatal("a time-of-day watcher created just now is not due")
	}

	if err := follower.requestRun(wid(t, "pager")); err != nil {
		t.Fatal(err)
	}
	leader.tick(time.Now())
	<-started
	follower.tick(time.Now())
	if follower.snapshot().status(wid(t, "pager")).Phase != watcher.PhaseModel {
		t.Error("the follower must see the run the leader started")
	}

	if err := follower.requestStop(wid(t, "pager")); err != nil {
		t.Fatal(err)
	}
	leader.tick(time.Now())
	waitIdle(t, leader, "pager")
	r, _ := leader.snapshot().status(wid(t, "pager")).LatestRun()
	if r.Status != watcher.RunStopped {
		t.Errorf("a stopped run reads stopped, got %+v", r)
	}
}

func TestWatcherHub_ApprovalsWaitForTheUser(t *testing.T) {
	h := useWatcherHub(t)
	stubScript(t, printing("deploy failed\n"))
	got := make(chan approvalReply, 1)
	var sends []tea.Msg
	var sendMu sync.Mutex
	prev := agentSendToProgram
	agentSendToProgram = func(msg tea.Msg) bool {
		sendMu.Lock()
		sends = append(sends, msg)
		sendMu.Unlock()
		return true
	}
	t.Cleanup(func() { agentSendToProgram = prev })
	stubModelRun(t, func(ctx context.Context, h *watcherHub, _ watcher.Def, live *watcherLive, _ string) watcherModelOutcome {
		reply := make(chan approvalReply, 1)
		if !h.parkApproval(approvalRequestMsg{tabID: live.tabID, toolName: "bash", input: map[string]any{"command": "kubectl rollout undo"}, reply: reply}) {
			t.Error("the hub must take its own run's approval")
		}
		got <- <-reply
		return watcherModelOutcome{provider: "fake", response: "rolled back"}
	})
	saveTestWatcher(t, "deploys", "every 5m")
	h.tick(time.Now())
	waitFor(t, func() bool { return h.snapshot().status(wid(t, "deploys")).Phase == watcher.PhaseApproval })
	if st := h.snapshot().status(wid(t, "deploys")); st.Approval != "bash" {
		t.Errorf("status names the tool: %+v", st)
	}
	if reason := h.blockedReason(); reason != "watcher deploys: approve bash" {
		t.Errorf("herdr reason = %q", reason)
	}
	if h.parkApproval(approvalRequestMsg{tabID: 99}) {
		t.Error("a tab's own approval must not be taken by the hub")
	}

	first, ok := h.openApproval(wid(t, "deploys"))
	if !ok || first.toolName != "bash" {
		t.Fatalf("openApproval = %+v %v", first, ok)
	}
	second, _ := h.openApproval(wid(t, "deploys"))
	first.reply <- approvalReply{allow: true}
	if r := <-got; !r.allow {
		t.Fatal("the user's answer must reach the run")
	}
	waitIdle(t, h, "deploys")
	second.reply <- approvalReply{allow: false}
	waitFor(t, func() bool {
		sendMu.Lock()
		defer sendMu.Unlock()
		for _, m := range sends {
			if c, ok := m.(watcherApprovalClosedMsg); ok && len(c.relays) == 2 {
				return true
			}
		}
		return false
	})
	if r, _ := h.snapshot().status(wid(t, "deploys")).LatestRun(); r.Status != watcher.RunDone || r.Response != "rolled back" {
		t.Errorf("run after approval = %+v", r)
	}
}

func TestWatcherHub_NewLeaderSettlesWhatTheOldOneLeft(t *testing.T) {
	h := useWatcherHub(t)
	saveTestWatcher(t, "prs", "daily 09:00")
	err := watcher.MutateState(func(s *watcher.State) error {
		st := s.Watcher(wid(t, "prs"))
		st.Phase, st.Activity = watcher.PhaseModel, "Reading the diff"
		st.AddRun(watcher.Run{ID: "orphan", Status: watcher.RunRunning, StartedAt: time.Now().Add(-time.Minute)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h.tick(time.Now())
	st := h.snapshot().status(wid(t, "prs"))
	if st.Phase != watcher.PhaseIdle || st.Activity != "" {
		t.Errorf("a dead leader's phase must reset: %+v", st)
	}
	if st.Runs[0].Status != watcher.RunStopped || !strings.Contains(st.Runs[0].Error, "closed") {
		t.Errorf("a dead leader's run must read stopped: %+v", st.Runs[0])
	}
}

func TestWatcherHub_SuspendGivesUpTheLead(t *testing.T) {
	a := useWatcherHub(t)
	b := newWatcherHub()
	t.Cleanup(b.close)
	stubScript(t, printing("x\n"))
	stubModelRun(t, func(ctx context.Context, _ *watcherHub, _ watcher.Def, _ *watcherLive, _ string) watcherModelOutcome {
		<-ctx.Done()
		return watcherModelOutcome{}
	})
	saveTestWatcher(t, "w", "every 5m")
	a.tick(time.Now())
	waitFor(t, func() bool { return a.snapshot().status(wid(t, "w")).Phase == watcher.PhaseModel })

	a.suspend()
	waitIdle(t, a, "w")
	if a.snapshot().leading {
		t.Fatal("a suspended window must give the lead up")
	}
	b.tick(time.Now())
	if !b.snapshot().leading {
		t.Fatal("another window takes the lead while the first is suspended")
	}
	a.resume()
	a.tick(time.Now())
	if a.snapshot().leading {
		t.Error("a resumed window does not take the lead back while another holds it")
	}
}

// installWatcherModel answers every model call with text and records the
// requests.
func installWatcherModel(t *testing.T, answer string) *[]*adkmodel.LLMRequest {
	t.Helper()
	var mu sync.Mutex
	var reqs []*adkmodel.LLMRequest
	prev := engine.ModelBuilder
	engine.ModelBuilder = func(_ context.Context, _ providers.Provider, _ config.Config, modelID string) (adkmodel.LLM, error) {
		return &mockADKModel{name: modelID, generateFunc: func(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			mu.Lock()
			reqs = append(reqs, req)
			mu.Unlock()
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText(answer, genai.RoleModel)}, nil)
			}
		}}, nil
	}
	t.Cleanup(func() { engine.ModelBuilder = prev })
	return &reqs
}

func systemText(req *adkmodel.LLMRequest) string {
	if req == nil || req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// A chat-target watcher runs a real background session: its messages never
// reach the program, its transcript is on disk, and its answer is the run's.
func TestWatcherHub_ChatTargetRunsABackgroundSession(t *testing.T) {
	h := useWatcherHub(t)
	withRegisteredProviders(t, agentAPIProvider{prov: windowProvider{id: "wtest", window: 8000}})
	reqs := installWatcherModel(t, "Reviewed #412: looks good.")
	var leaked []tea.Msg
	var mu sync.Mutex
	prev := agentSendToProgram
	agentSendToProgram = func(msg tea.Msg) bool {
		mu.Lock()
		defer mu.Unlock()
		switch msg.(type) {
		case watchersChangedMsg, watcherApprovalClosedMsg:
		default:
			leaked = append(leaked, msg)
		}
		return true
	}
	t.Cleanup(func() { agentSendToProgram = prev })
	stubScript(t, printing("#412 fix the race\n"))
	saveTestWatcher(t, "prs", "every 5m")

	h.tick(time.Now())
	waitIdle(t, h, "prs")
	r, ok := h.snapshot().status(wid(t, "prs")).LatestRun()
	if !ok || r.Status != watcher.RunDone || r.Provider != "wtest" || r.SessionID == "" {
		t.Fatalf("run = %+v", r)
	}
	if r.Response != "Reviewed #412: looks good." {
		t.Errorf("response = %q", r.Response)
	}
	mu.Lock()
	if len(leaked) != 0 {
		t.Errorf("a background session sent %d messages to the program: %T", len(leaked), leaked[0])
	}
	mu.Unlock()
	if len(*reqs) == 0 || !strings.Contains(systemText((*reqs)[0]), `"ask_coder"`) {
		t.Error("a chat target runs ask's own agent")
	}
	items, err := (&agentSessionStore{provider: "wtest"}).loadTranscript(r.SessionID)
	if err != nil {
		t.Fatalf("the run's session must be on disk: %v", err)
	}
	var sawPrompt, sawAnswer bool
	for _, it := range items {
		sawPrompt = sawPrompt || (it.kind == trUser && strings.Contains(it.text, "#412 fix the race"))
		sawAnswer = sawAnswer || (it.kind == trAssistant && strings.Contains(it.text, "looks good"))
	}
	if !sawPrompt || !sawAnswer {
		t.Errorf("transcript = %+v", items)
	}
}

func TestWatcherHub_AgentTargetRunsAsTheAgent(t *testing.T) {
	h := useWatcherHub(t)
	withRegisteredProviders(t, agentAPIProvider{prov: windowProvider{id: "wtest", window: 8000}})
	reqs := installWatcherModel(t, "done")
	stubScript(t, printing("#7 bump deps\n"))
	d := saveTestWatcher(t, "deps", "every 5m", func(d *watcher.Def) {
		d.Instructions = ""
		d.Agent = "reviewer"
	})
	writeTestFile(t, filepath.Join(d.Cwd, ".ask", "agents"), "reviewer.md",
		"---\nname: reviewer\ndescription: reviews pull requests\ntools: read, grep\n---\nYou are the reviewer.\n")
	writeTestFile(t, d.Cwd, "CLAUDE.md", "WATCHER_PROJECT_MARKER\n")

	h.tick(time.Now())
	waitIdle(t, h, "deps")
	if r, _ := h.snapshot().status(wid(t, "deps")).LatestRun(); r.Status != watcher.RunDone || r.Target != "agent reviewer" {
		t.Fatalf("run = %+v", r)
	}
	sys := systemText((*reqs)[0])
	if !strings.HasPrefix(sys, "You are the reviewer.") || !strings.Contains(sys, `"reviewer"`) {
		t.Errorf("an agent target runs on its own instructions and name:\n%s", sys)
	}
	if !strings.Contains(sys, "WATCHER_PROJECT_MARKER") {
		t.Error("an agent target works with its project's instructions, as a tab does")
	}
	var names []string
	for _, tl := range (*reqs)[0].Config.Tools {
		for _, fd := range tl.FunctionDeclarations {
			names = append(names, fd.Name)
		}
	}
	got := strings.Join(names, ",")
	if !strings.Contains(got, "read") || !strings.Contains(got, "grep") || !strings.Contains(got, "load_memory") || strings.Contains(got, "bash") {
		t.Errorf("an agent target gets the tools its definition grants, plus memory lookup: %s", got)
	}
}

func TestWatcherHub_MissingAgentFailsTheRun(t *testing.T) {
	h := useWatcherHub(t)
	stubScript(t, printing("x\n"))
	saveTestWatcher(t, "orphan", "every 5m", func(d *watcher.Def) {
		d.Instructions = ""
		d.Agent = "gone"
	})
	h.tick(time.Now())
	waitIdle(t, h, "orphan")
	r, _ := h.snapshot().status(wid(t, "orphan")).LatestRun()
	if r.Status != watcher.RunFailed || !strings.Contains(r.Error, `no agent named "gone"`) {
		t.Errorf("run = %+v", r)
	}
}

// requestText is every text part a model request carries.
func requestText(req *adkmodel.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil {
				b.WriteString(p.Text)
			}
		}
	}
	return b.String()
}

// A watcher's session that starts a sub-agent in the background and ends
// its turn does not end the run: the run waits for the agent, hands its
// report to the model as one more turn, and that turn's answer is the
// run's.
func TestWatcherHub_WaitsForBackgroundAgents(t *testing.T) {
	h := useWatcherHub(t)
	providers.Register(windowProvider{id: "wtest-bg", window: 8000})
	withRegisteredProviders(t, agentAPIProvider{prov: windowProvider{id: "wtest-bg", window: 8000}})
	release := make(chan struct{})
	var mu sync.Mutex
	var finalRequest string
	reply := func(parts ...*genai.Part) iter.Seq2[*adkmodel.LLMResponse, error] {
		return func(yield func(*adkmodel.LLMResponse, error) bool) {
			yield(&adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: parts}, FinishReason: genai.FinishReasonStop}, nil)
		}
	}
	prev := engine.ModelBuilder
	engine.ModelBuilder = func(_ context.Context, _ providers.Provider, _ config.Config, modelID string) (adkmodel.LLM, error) {
		return &mockADKModel{name: modelID, generateFunc: func(ctx context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			if strings.Contains(systemText(req), "You are the helper.") {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return reply(genai.NewPartFromText("HELPER_REPORT: #7 looks fine"))
			}
			text := requestText(req)
			last := req.Contents[len(req.Contents)-1]
			switch {
			case strings.Contains(text, "HELPER_REPORT"):
				mu.Lock()
				finalRequest = text
				mu.Unlock()
				return reply(genai.NewPartFromText("Final: the helper says #7 looks fine."))
			case len(last.Parts) > 0 && last.Parts[0].FunctionResponse != nil:
				return reply(genai.NewPartFromText("Started the helper."))
			}
			return reply(genai.NewPartFromFunctionCall("task", map[string]any{
				"prompt": "review #7", "agent": "helper", "run_in_background": true, "description": "review #7",
			}))
		}}, nil
	}
	t.Cleanup(func() { engine.ModelBuilder = prev })
	stubScript(t, printing("#7 bump deps\n"))
	d := saveTestWatcher(t, "prs", "every 5m", func(d *watcher.Def) { d.Provider = "wtest-bg" })
	writeTestFile(t, filepath.Join(d.Cwd, ".ask", "agents"), "helper.md",
		"---\nname: helper\ndescription: reviews one PR\ntools: read\n---\nYou are the helper.\n")

	h.tick(time.Now())
	waitFor(t, func() bool { return h.snapshot().status(wid(t, "prs")).Activity == "waiting for 1 background agent" })
	if r, _ := h.snapshot().status(wid(t, "prs")).LatestRun(); r.Finished() {
		t.Fatal("the run must not end while its background agent is out")
	}
	close(release)
	waitIdle(t, h, "prs")

	r, _ := h.snapshot().status(wid(t, "prs")).LatestRun()
	if r.Status != watcher.RunDone || r.Response != "Final: the helper says #7 looks fine." {
		t.Fatalf("run = %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(finalRequest, `<agent_report job="job-1" task="agent helper: review #7" status="finished">`) {
		t.Errorf("the report turn did not carry the agent's report:\n%s", finalRequest)
	}
}

// Stopping a run while it waits for a background agent ends it, and the
// agent with it.
func TestWatcherHub_StopWhileWaitingForAgents(t *testing.T) {
	h := useWatcherHub(t)
	providers.Register(windowProvider{id: "wtest-bg", window: 8000})
	withRegisteredProviders(t, agentAPIProvider{prov: windowProvider{id: "wtest-bg", window: 8000}})
	helperCancelled := make(chan struct{})
	reply := func(text string) iter.Seq2[*adkmodel.LLMResponse, error] {
		return func(yield func(*adkmodel.LLMResponse, error) bool) {
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel), FinishReason: genai.FinishReasonStop}, nil)
		}
	}
	prev := engine.ModelBuilder
	engine.ModelBuilder = func(_ context.Context, _ providers.Provider, _ config.Config, modelID string) (adkmodel.LLM, error) {
		return &mockADKModel{name: modelID, generateFunc: func(ctx context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			if strings.Contains(systemText(req), "You are the helper.") {
				<-ctx.Done()
				close(helperCancelled)
				return reply("never")
			}
			last := req.Contents[len(req.Contents)-1]
			if len(last.Parts) > 0 && last.Parts[0].FunctionResponse != nil {
				return reply("Started the helper.")
			}
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				yield(&adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromFunctionCall("task", map[string]any{
					"prompt": "review", "agent": "helper", "run_in_background": true, "description": "review",
				})}}, FinishReason: genai.FinishReasonStop}, nil)
			}
		}}, nil
	}
	t.Cleanup(func() { engine.ModelBuilder = prev })
	stubScript(t, printing("#7\n"))
	d := saveTestWatcher(t, "prs", "every 5m", func(d *watcher.Def) { d.Provider = "wtest-bg" })
	writeTestFile(t, filepath.Join(d.Cwd, ".ask", "agents"), "helper.md",
		"---\nname: helper\ndescription: reviews one PR\ntools: read\n---\nYou are the helper.\n")

	h.tick(time.Now())
	waitFor(t, func() bool { return h.snapshot().status(wid(t, "prs")).Activity == "waiting for 1 background agent" })
	if err := h.requestStop(wid(t, "prs")); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, "prs")
	if r, _ := h.snapshot().status(wid(t, "prs")).LatestRun(); r.Status != watcher.RunStopped {
		t.Errorf("run = %+v", r)
	}
	select {
	case <-helperCancelled:
	case <-time.After(2 * time.Second):
		t.Error("the background agent outlived the stopped run")
	}
}
