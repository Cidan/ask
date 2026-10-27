package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func strPtr(s string) *string { return &s }

func sampleDef(name string) Def {
	return Def{
		Name:         name,
		Schedule:     "every 5m",
		Command:      "./check.sh",
		Cwd:          "/tmp",
		Instructions: "review what it found",
		CreatedAt:    time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
	}
}

// gitProject is a directory that is a git checkout's root.
func gitProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProjectOf(t *testing.T) {
	root := gitProject(t)
	sub := filepath.Join(root, "pkg", "x")
	wt := filepath.Join(root, ".claude", "worktrees", "ask-tree", "pkg")
	for _, dir := range []string{sub, wt} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for dir, want := range map[string]string{root: root, sub: root, wt: root} {
		if got := ProjectOf(dir); got != want {
			t.Errorf("ProjectOf(%s) = %s, want %s", dir, got, want)
		}
	}
	plain := t.TempDir()
	if got := ProjectOf(plain); got != plain {
		t.Errorf("a directory outside any checkout is its own project: %s", got)
	}
	a, b := ProjectDir("/home/me/git/ask"), ProjectDir("/srv/ask")
	if !strings.HasPrefix(a, "ask-") || !strings.HasPrefix(b, "ask-") || a == b {
		t.Errorf("two projects named ask must get distinct readable dirs: %s %s", a, b)
	}
}

func TestStore_CreateGetListDelete(t *testing.T) {
	isolate(t)
	created, err := Save(Ref{}, sampleDef("prs"), nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Project != "/tmp" {
		t.Errorf("Save sets the project from the working directory: %q", created.Project)
	}
	if _, err := Save(Ref{}, sampleDef("prs"), nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create: %v", err)
	}
	b := sampleDef("audit")
	b.Command = ""
	audit, err := Save(Ref{}, b, strPtr("#!/bin/sh\necho hi\n"))
	if err != nil {
		t.Fatalf("create with script: %v", err)
	}
	got, err := Get(audit.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasScript {
		t.Error("a stored script must surface as HasScript")
	}
	fi, err := os.Stat(ScriptPath(audit.Ref()))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("script must be stored executable: %v %v", fi.Mode(), err)
	}
	if body, _ := ReadScript(audit.Ref()); body != "#!/bin/sh\necho hi\n" {
		t.Errorf("ReadScript = %q", body)
	}

	// A malformed file and a file stored in the wrong place are skipped.
	dir := filepath.Dir(DefPath(audit.Ref()))
	_ = os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "liar.json"), []byte(`{"name":"liar","project":"/elsewhere"}`), 0o644)
	list := List()
	if len(list) != 2 || list[0].Name != "audit" || list[1].Name != "prs" {
		t.Fatalf("List = %+v", list)
	}

	_ = os.MkdirAll(StateDir(audit.Ref()), 0o700)
	_ = MutateState(func(s *State) error { s.Watcher(audit.ID()).LastNote = "no output"; return nil })
	if err := Delete(audit.Ref()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{DefPath(audit.Ref()), ScriptPath(audit.Ref()), StateDir(audit.Ref())} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived Delete", p)
		}
	}
	s, _ := LoadState()
	if _, ok := s.Watchers[audit.ID()]; ok {
		t.Error("Delete must drop the watcher's shared state")
	}
	if err := Delete(audit.Ref()); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v", err)
	}
}

func TestStore_RejectsInvalidDefinitions(t *testing.T) {
	isolate(t)
	cases := map[string]func(*Def) *string{
		"invalid":          func(d *Def) *string { d.Name = "Bad Name"; return nil },
		"needs a command":  func(d *Def) *string { d.Command = ""; return nil },
		"not both":         func(d *Def) *string { return strPtr("#!/bin/sh\n") },
		"absolute path":    func(d *Def) *string { d.Cwd = "relative"; return nil },
		"unknown schedule": func(d *Def) *string { d.Schedule = "whenever"; return nil },
		"not both.":        func(d *Def) *string { d.Agent, d.Workflow = "a", "w"; return nil },
		"steps pick":       func(d *Def) *string { d.Workflow, d.Model = "w", "m"; return nil },
		"needs instructions": func(d *Def) *string {
			d.Instructions = " "
			return nil
		},
		"#!": func(d *Def) *string { d.Command = ""; return strPtr("echo hi") },
		"timeout": func(d *Def) *string {
			d.TimeoutSecs = int(MaxTimeout/time.Second) + 1
			return nil
		},
	}
	for want, mutate := range cases {
		d := sampleDef("x")
		script := mutate(&d)
		_, err := Save(Ref{}, d, script)
		if err == nil || !strings.Contains(err.Error(), strings.TrimSuffix(want, ".")) {
			t.Errorf("%s: Save error = %v", want, err)
		}
	}
	// An agent or workflow target carries its own instructions.
	d := sampleDef("agented")
	d.Instructions, d.Agent = "", "reviewer"
	if _, err := Save(Ref{}, d, nil); err != nil {
		t.Errorf("agent target without instructions: %v", err)
	}
}

func TestStore_RenameMovesEverything(t *testing.T) {
	isolate(t)
	d := sampleDef("old")
	d.Command = ""
	old, err := Save(Ref{}, d, strPtr("#!/bin/sh\n"))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(StateDir(old.Ref()), 0o700)
	_ = os.WriteFile(filepath.Join(StateDir(old.Ref()), "seen"), []byte("1"), 0o600)
	_ = MutateState(func(s *State) error { s.Watcher(old.ID()).LastNote = "no output"; return nil })

	d.Name = "new"
	renamed, err := Save(old.Ref(), d, nil)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := os.Stat(DefPath(old.Ref())); !errors.Is(err, os.ErrNotExist) {
		t.Error("old definition survived the rename")
	}
	if body, _ := ReadScript(renamed.Ref()); body != "#!/bin/sh\n" {
		t.Errorf("script did not move: %q", body)
	}
	if data, _ := os.ReadFile(filepath.Join(StateDir(renamed.Ref()), "seen")); string(data) != "1" {
		t.Error("scratch directory did not move")
	}
	s, _ := LoadState()
	if s.Watchers[renamed.ID()] == nil || s.Watchers[renamed.ID()].LastNote != "no output" || s.Watchers[old.ID()] != nil {
		t.Errorf("shared state did not move: %+v", s.Watchers)
	}

	// Switching from the script to a command removes the script.
	d.Command = "./check.sh"
	if _, err := Save(renamed.Ref(), d, strPtr("")); err != nil {
		t.Fatalf("switch to command: %v", err)
	}
	if got, _ := Get(renamed.Ref()); got.HasScript {
		t.Error("script survived the switch to a command")
	}

	if _, err := SetPaused(renamed.Ref(), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(renamed.Ref()); !got.Paused {
		t.Error("SetPaused did not stick")
	}
	if _, err := Save(Ref{Project: "/tmp", Name: "ghost"}, sampleDef("ghost"), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("editing a missing watcher = %v", err)
	}
}

// Each project has its own set of watchers: the same name lives in two,
// Resolve prefers the asking project's and refuses to guess between
// others, and moving a watcher to another project's directory moves it
// to that project.
func TestStore_WatchersArePerProject(t *testing.T) {
	isolate(t)
	ask, other := gitProject(t), gitProject(t)
	inAsk := sampleDef("prs")
	inAsk.Cwd = ask
	inOther := sampleDef("prs")
	inOther.Cwd = other
	a, err := Save(Ref{}, inAsk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Save(Ref{}, inOther, nil); err != nil {
		t.Fatalf("the same name in another project: %v", err)
	}
	if len(List()) != 2 {
		t.Fatalf("List = %+v", List())
	}
	if got, err := Resolve(ask, "prs"); err != nil || got.Project != ask {
		t.Errorf("Resolve prefers the asking project's: %+v %v", got, err)
	}
	if _, err := Resolve(t.TempDir(), "prs"); err == nil || !strings.Contains(err.Error(), "several projects") {
		t.Errorf("Resolve must not guess between projects: %v", err)
	}
	solo := sampleDef("solo")
	solo.Cwd = other
	if _, err := Save(Ref{}, solo, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := Resolve(ask, "solo"); err != nil || got.Project != other {
		t.Errorf("Resolve finds a watcher another project has alone: %+v %v", got, err)
	}

	sub := filepath.Join(ask, "tools")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	moved := a
	moved.Cwd = sub
	if got, err := Save(a.Ref(), moved, nil); err != nil || got.Project != ask {
		t.Errorf("a subdirectory stays in its project: %+v %v", got, err)
	}
	moved.Cwd = other
	if _, err := Save(a.Ref(), moved, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("moving onto another project's watcher of the same name must refuse: %v", err)
	}
	moved.Name = "prs-ask"
	got, err := Save(a.Ref(), moved, nil)
	if err != nil || got.Project != other {
		t.Fatalf("move to another project: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Dir(DefPath(a.Ref()))); !errors.Is(err, os.ErrNotExist) {
		t.Error("a project's store directory goes once its last watcher leaves")
	}
}

func TestState_RunsAndRequests(t *testing.T) {
	isolate(t)
	err := MutateState(func(s *State) error {
		st := s.Watcher("prs")
		for i := 0; i < MaxRuns+5; i++ {
			st.AddRun(Run{ID: string(rune('a' + i)), Status: RunDone})
		}
		st.AddRun(Run{ID: "live", Status: RunRunning})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st := s.Watchers["prs"]
	if len(st.Runs) != MaxRuns || st.Runs[0].ID != "live" {
		t.Fatalf("runs not capped newest-first: %d %q", len(st.Runs), st.Runs[0].ID)
	}
	if st.Unseen() {
		t.Error("a running run is not an unseen result")
	}
	st.UpdateRun("live", func(r *Run) { r.Status = RunDone })
	if !st.Unseen() {
		t.Error("a finished run the user has not opened is unseen")
	}
	st.SeenRun = "live"
	if st.Unseen() {
		t.Error("SeenRun clears Unseen")
	}

	clone := s.Clone()
	clone.Watchers["prs"].Runs[0].ID = "changed"
	if s.Watchers["prs"].Runs[0].ID != "live" {
		t.Error("Clone shares runs with the original")
	}

	stamp1, ok := CurrentStateStamp()
	if !ok {
		t.Fatal("no stamp for an existing state file")
	}
	time.Sleep(10 * time.Millisecond)
	_ = MutateState(func(s *State) error { s.Watcher("prs").RunRequested = time.Now(); return nil })
	if stamp2, _ := CurrentStateStamp(); stamp2 == stamp1 {
		t.Error("a write must change the stamp readers poll")
	}
}

func TestClip(t *testing.T) {
	if got := Clip("héllo", 2); got != "h\n… [truncated]" {
		t.Errorf("Clip cut inside a rune: %q", got)
	}
	if got := ClipTail("abcdef", 3); got != "[truncated] …\ndef" {
		t.Errorf("ClipTail = %q", got)
	}
	if got := Clip("short", 10); got != "short" {
		t.Errorf("Clip changed a short string: %q", got)
	}
}

func TestLead_OneHolderAtATime(t *testing.T) {
	isolate(t)
	first, err := TryLead()
	if err != nil || first == nil {
		t.Fatalf("first TryLead = %v, %v", first, err)
	}
	second, err := TryLead()
	if err != nil || second != nil {
		t.Fatalf("a second holder got the lead: %v, %v", second, err)
	}
	first.Release()
	third, err := TryLead()
	if err != nil || third == nil {
		t.Fatalf("the lead was not free after Release: %v, %v", third, err)
	}
	third.Release()
}
