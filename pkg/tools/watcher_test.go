package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/watcher"
)

func watcherToolByName(t *testing.T, env *ToolEnv, name string) Tool {
	t.Helper()
	for _, tool := range WatcherTools(env) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("watcher tool %q missing", name)
	return nil
}

func TestWatcherTools_CoversEveryWatcherTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	want := []string{"watcher_list", "watcher_get", "watcher_create", "watcher_edit", "watcher_delete", "watcher_run"}
	got := map[string]bool{}
	for _, tool := range WatcherTools(env) {
		got[tool.Name()] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing %s", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tool count %d want %d: %v", len(got), len(want), got)
	}
}

func TestWatcherTools_LifeCycle(t *testing.T) {
	env, _ := newTestToolEnv(t)

	resp, _ := RunToolWithJSON(testAgentCtx(), watcherToolByName(t, env, "watcher_create"),
		`{"name":"prs","schedule":"every 5 minutes","command":"./prs.sh","instructions":"review each PR"}`)
	if resp.IsError {
		t.Fatalf("create: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "every 5m") {
		t.Errorf("create should confirm the canonical schedule: %s", resp.Content)
	}
	d, err := watcher.Get(watcher.Ref{Project: env.Cwd, Name: "prs"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Cwd != env.Cwd || d.Project != env.Cwd || d.CreatedAt.IsZero() || d.Target() != watcher.TargetChat {
		t.Errorf("stored definition = %+v", d)
	}

	resp, _ = RunToolWithJSON(testAgentCtx(), watcherToolByName(t, env, "watcher_list"), `{}`)
	if resp.IsError || !strings.Contains(resp.Content, "prs — every 5 minutes") || !strings.Contains(resp.Content, "has not run yet") {
		t.Errorf("list: %s", resp.Content)
	}

	// Switching to a stored script drops the command; a rename keeps it.
	resp, _ = RunToolWithJSON(testAgentCtx(), watcherToolByName(t, env, "watcher_edit"),
		`{"name":"prs","new_name":"pulls","script":"#!/usr/bin/env python3\nprint('hi')\n","schedule":"weekdays 09:00"}`)
	if resp.IsError {
		t.Fatalf("edit: %s", resp.Content)
	}
	d, err = watcher.Get(watcher.Ref{Project: env.Cwd, Name: "pulls"})
	if err != nil || d.Command != "" || !d.HasScript || d.Schedule != "weekdays 09:00" {
		t.Fatalf("edited definition = %+v %v", d, err)
	}

	resp, _ = RunToolWithJSON(testAgentCtx(), watcherToolByName(t, env, "watcher_get"), `{"name":"pulls"}`)
	if resp.IsError || !strings.Contains(resp.Content, "print('hi')") {
		t.Errorf("get should show the stored script: %s", resp.Content)
	}

	resp, _ = RunToolWithJSON(testAgentCtx(), watcherToolByName(t, env, "watcher_run"), `{"name":"pulls"}`)
	if resp.IsError {
		t.Fatalf("run: %s", resp.Content)
	}
	s, _ := watcher.LoadState()
	if st := s.Watchers[d.ID()]; st == nil || st.RunRequested.IsZero() {
		t.Error("watcher_run must file a run request in the shared state")
	}

	resp, _ = RunToolWithJSON(testAgentCtx(), watcherToolByName(t, env, "watcher_edit"), `{"name":"pulls","paused":true}`)
	if resp.IsError {
		t.Fatalf("pause: %s", resp.Content)
	}
	resp, _ = RunToolWithJSON(testAgentCtx(), watcherToolByName(t, env, "watcher_run"), `{"name":"pulls"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "paused") {
		t.Errorf("running a paused watcher should refuse: %s", resp.Content)
	}

	resp, _ = RunToolWithJSON(testAgentCtx(), watcherToolByName(t, env, "watcher_delete"), `{"name":"pulls"}`)
	if resp.IsError {
		t.Fatalf("delete: %s", resp.Content)
	}
	if len(watcher.List()) != 0 {
		t.Error("delete left the watcher behind")
	}
}

func TestWatcherCreate_ChecksWhatItPointsAt(t *testing.T) {
	env, _ := newTestToolEnv(t)
	create := watcherToolByName(t, env, "watcher_create")
	cases := map[string]string{
		`{"name":"a","schedule":"every 5m","command":"x","instructions":"i","cwd":"/does/not/exist"}`: "does not exist",
		`{"name":"a","schedule":"every 5m","command":"x","agent":"nobody"}`:                           `no agent named "nobody"`,
		`{"name":"a","schedule":"every 5m","command":"x","workflow":"nothing"}`:                       `workflow "nothing"`,
		`{"name":"a","schedule":"every 5m","command":"x","instructions":"i","provider":"nope"}`:       `unknown provider "nope"`,
		`{"name":"a","schedule":"now and then","command":"x","instructions":"i"}`:                     "unknown schedule",
		`{"name":"a","schedule":"every 5m","command":"x"}`:                                            "needs instructions",
	}
	for in, want := range cases {
		resp, _ := RunToolWithJSON(testAgentCtx(), create, in)
		if !resp.IsError || !strings.Contains(resp.Content, want) {
			t.Errorf("%s: got %q, want an error containing %q", in, resp.Content, want)
		}
	}
	if len(watcher.List()) != 0 {
		t.Error("a rejected create stored something")
	}
}

func TestWatcherCreate_WorktreeSessionMapsToTheCheckout(t *testing.T) {
	env, _ := newTestToolEnv(t)
	root := t.TempDir()
	wt := filepath.Join(root, ".claude", "worktrees", "ask-x")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	env.Cwd = wt
	resp, _ := RunToolWithJSON(testAgentCtx(), watcherToolByName(t, env, "watcher_create"),
		`{"name":"w","schedule":"every 5m","command":"x","instructions":"i"}`)
	if resp.IsError {
		t.Fatalf("create: %s", resp.Content)
	}
	if d, _ := watcher.Get(watcher.Ref{Project: root, Name: "w"}); d.Cwd != root || d.Project != root {
		t.Errorf("cwd = %q, want the checkout %q", d.Cwd, root)
	}
}

func TestWatcherCreate_AsksForApproval(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.SkipPermissions = false
	var asked map[string]any
	allow := false
	env.Approve = func(_ context.Context, tool string, input map[string]any) (bool, error) {
		if tool == "watcher_create" {
			asked = input
		}
		return allow, nil
	}
	create := watcherToolByName(t, env, "watcher_create")
	in := `{"name":"prs","schedule":"every 5m","command":"gh pr list","instructions":"i"}`
	resp, _ := RunToolWithJSON(testAgentCtx(), create, in)
	if !resp.IsError || !strings.Contains(resp.Content, "denied") {
		t.Fatalf("a denied create should fail: %s", resp.Content)
	}
	if asked["command"] != "gh pr list" || asked["schedule"] != "every 5m" {
		t.Errorf("the approval prompt must show what will run: %v", asked)
	}
	if len(watcher.List()) != 0 {
		t.Fatal("a denied create stored the watcher")
	}
	allow = true
	if resp, _ := RunToolWithJSON(testAgentCtx(), create, in); resp.IsError {
		t.Fatalf("approved create: %s", resp.Content)
	}
	if len(watcher.List()) != 1 {
		t.Error("an approved create must store the watcher")
	}
}

// Each project has its own watchers: a session sees its project's, the
// same name can live in two projects, and project= reaches another one.
func TestWatcherTools_PerProject(t *testing.T) {
	env, _ := newTestToolEnv(t)
	here := env.Cwd
	other := t.TempDir()
	create := watcherToolByName(t, env, "watcher_create")
	for _, in := range []string{
		`{"name":"prs","schedule":"every 5m","command":"./here.sh","instructions":"i"}`,
		`{"name":"prs","schedule":"every 5m","command":"./there.sh","instructions":"i","cwd":"` + other + `"}`,
		`{"name":"lint","schedule":"every 5m","command":"./lint.sh","instructions":"i","cwd":"` + other + `"}`,
	} {
		if resp, _ := RunToolWithJSON(testAgentCtx(), create, in); resp.IsError {
			t.Fatalf("create %s: %s", in, resp.Content)
		}
	}

	list := watcherToolByName(t, env, "watcher_list")
	resp, _ := RunToolWithJSON(testAgentCtx(), list, `{}`)
	if !strings.Contains(resp.Content, "1 watcher(s) in "+here) || strings.Contains(resp.Content, "lint") || !strings.Contains(resp.Content, "2 more in other projects") {
		t.Errorf("list shows the session's project and counts the rest:\n%s", resp.Content)
	}
	resp, _ = RunToolWithJSON(testAgentCtx(), list, `{"all":true}`)
	if !strings.Contains(resp.Content, "3 watcher(s) across all projects") || !strings.Contains(resp.Content, other+":") {
		t.Errorf("all lists every project's watchers by project:\n%s", resp.Content)
	}

	get := watcherToolByName(t, env, "watcher_get")
	resp, _ = RunToolWithJSON(testAgentCtx(), get, `{"name":"prs"}`)
	if !strings.Contains(resp.Content, "./here.sh") {
		t.Errorf("a bare name means the session's project:\n%s", resp.Content)
	}
	resp, _ = RunToolWithJSON(testAgentCtx(), get, `{"name":"prs","project":"`+other+`"}`)
	if !strings.Contains(resp.Content, "./there.sh") {
		t.Errorf("project= reaches another project's watcher:\n%s", resp.Content)
	}
	resp, _ = RunToolWithJSON(testAgentCtx(), get, `{"name":"lint"}`)
	if resp.IsError || !strings.Contains(resp.Content, "Project: "+other) {
		t.Errorf("a name only another project has resolves to it:\n%s", resp.Content)
	}

	edit := watcherToolByName(t, env, "watcher_edit")
	resp, _ = RunToolWithJSON(testAgentCtx(), edit, `{"name":"lint","cwd":"`+here+`"}`)
	if resp.IsError || !strings.Contains(resp.Content, "now in "+here) {
		t.Fatalf("a cwd in another project moves the watcher:\n%s", resp.Content)
	}
	if _, err := watcher.Get(watcher.Ref{Project: here, Name: "lint"}); err != nil {
		t.Errorf("moved watcher not found in its new project: %v", err)
	}
}
