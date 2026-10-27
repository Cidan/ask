package tools

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/engine"
)

func TestResolveAgentShell(t *testing.T) {
	lookPath := func(paths map[string]string) func(string) (string, error) {
		return func(name string) (string, error) {
			if p, ok := paths[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		}
	}
	getenv := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}

	tests := []struct {
		name    string
		paths   map[string]string
		environ map[string]string
		want    string
	}{
		{
			name:    "prefers bash over zsh and $SHELL",
			paths:   map[string]string{"bash": "/usr/bin/bash", "zsh": "/usr/bin/zsh"},
			environ: map[string]string{"SHELL": "/usr/bin/fish"},
			want:    "/usr/bin/bash",
		},
		{
			name:    "falls back to zsh when bash missing",
			paths:   map[string]string{"zsh": "/usr/bin/zsh"},
			environ: map[string]string{"SHELL": "/usr/bin/fish"},
			want:    "/usr/bin/zsh",
		},
		{
			name:    "uses $SHELL when neither bash nor zsh present",
			paths:   map[string]string{},
			environ: map[string]string{"SHELL": "/usr/bin/fish"},
			want:    "/usr/bin/fish",
		},
		{
			name:    "ignores empty lookPath result and falls through",
			paths:   map[string]string{"bash": ""},
			environ: map[string]string{"SHELL": "/usr/bin/fish"},
			want:    "/usr/bin/fish",
		},
		{
			name:    "falls back to /bin/sh when nothing is available",
			paths:   map[string]string{},
			environ: map[string]string{},
			want:    "/bin/sh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveAgentShell(lookPath(tt.paths), getenv(tt.environ))
			if got != tt.want {
				t.Errorf("ResolveAgentShell() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBashToolSync(t *testing.T) {
	env, _ := newTestToolEnv(t)
	oldRun := RunShell
	defer func() { RunShell = oldRun }()

	RunShell = func(dir, command string, extraEnv ...string) (*ShellHandle, error) {
		out := make(chan string, 1)
		done := make(chan ShellResult, 1)
		out <- "command executed\n"
		close(out)
		done <- ShellResult{ExitCode: 0}
		return &ShellHandle{Output: out, Done: done, Kill: func() {}}, nil
	}

	tool := BashTool(env)
	resp := runTool(t, tool, BashParams{Command: "echo hello"})
	if resp.IsError {
		t.Fatalf("bash failed: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "command executed") {
		t.Errorf("expected output, got: %s", resp.Content)
	}
}

func TestBashToolBackground(t *testing.T) {
	env, events := newTestToolEnvSnapshot(t)
	oldRun := RunShell
	defer func() { RunShell = oldRun }()

	doneCh := make(chan struct{})
	RunShell = func(dir, command string, extraEnv ...string) (*ShellHandle, error) {
		out := make(chan string, 2)
		done := make(chan ShellResult, 1)
		go func() {
			out <- "first chunk\n"
			<-doneCh
			out <- "second chunk\n"
			close(out)
			done <- ShellResult{ExitCode: 0}
		}()
		return &ShellHandle{
			Output: out,
			Done:   done,
			Kill:   func() { close(doneCh) },
		}, nil
	}

	bashTool := BashTool(env)
	outputTool := JobOutputTool(env)
	killTool := JobKillTool(env)

	resp := runTool(t, bashTool, BashParams{Command: "long task", RunInBackground: true})
	if resp.IsError {
		t.Fatalf("bg bash failed: %s", resp.Content)
	}
	jobID := "job-1"

	// Check output while still running
	time.Sleep(20 * time.Millisecond)
	resp = runTool(t, outputTool, JobOutputParams{JobID: jobID})
	if resp.IsError || !strings.Contains(resp.Content, "first chunk") {
		t.Errorf("expected first chunk, got: %s", resp.Content)
	}

	// Kill job
	resp = runTool(t, killTool, JobKillParams{JobID: jobID})
	if resp.IsError {
		t.Fatalf("kill failed: %s", resp.Content)
	}

	// The ended event is emitted after the job is marked done, which is
	// what job_kill waits for.
	startedFound := false
	endedFound := false
	for deadline := time.Now().Add(2 * time.Second); !endedFound && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		for _, ev := range events() {
			if _, ok := ev.(engine.BgTaskStartedEvent); ok {
				startedFound = true
			}
			if _, ok := ev.(engine.BgTaskEndedEvent); ok {
				endedFound = true
			}
		}
	}
	if !startedFound || !endedFound {
		t.Errorf("expected started and ended bg task events, got: %+v", events())
	}
}

func TestValidateSudoCommand(t *testing.T) {
	tests := []struct {
		cmd   string
		valid bool
	}{
		{"sudo -A apt update", true},
		{"sudo --askpass make install", true},
		{"sudo apt update", false},
		{"echo hi && sudo -A cat /etc/shadow", true},
		{"echo hi && sudo cat /etc/shadow", false},
		{"FOO=1 sudo -A systemctl restart nginx", true},
		{"FOO=1 sudo systemctl restart nginx", false},
	}

	for _, tt := range tests {
		err := ValidateSudoCommand(tt.cmd)
		if (err == nil) != tt.valid {
			t.Errorf("ValidateSudoCommand(%q) valid=%v, want %v (err=%v)", tt.cmd, err == nil, tt.valid, err)
		}
	}
}

func TestValidateSudoCommand_FollowsSudosOptionGrammar(t *testing.T) {
	tests := []struct {
		cmd   string
		valid bool
	}{
		{"sudo -u postgres -A psql", true},
		{"sudo -E -A make install", true},
		{"sudo -nA true", true},
		{"sudo -uroot -A id", true},
		{"sudo --user=root --askpass id", true},
		{"sudo --user root --ask id", true},
		{"sudo -p 'pw: ' -A id", true},
		{"sudo FOO=1 -A env", true},
		{"sudo -n true", true},
		{"sudo --non-interactive true", true},
		{"sudo -k", true},
		{"sudo -K", true},
		{"sudo -V", true},
		{"sudo --version", true},
		{"sudo -h", true},
		{"sudo -k ls", false},
		{"sudo -ks", false},
		{"sudo -l", false},
		{"sudo -v", false},
		{"sudo -S cat /etc/shadow", false},
		{"sudo -u root apt update", false},
		{"sudo -p -A id", false},
		{"sudo -h host id", false},
		{"sudo -- -A", false},
		{"sudo --bogus -A id", false},
		{"sudo", false},
	}
	for _, tt := range tests {
		if err := ValidateSudoCommand(tt.cmd); (err == nil) != tt.valid {
			t.Errorf("ValidateSudoCommand(%q) valid=%v, want %v (err=%v)", tt.cmd, err == nil, tt.valid, err)
		}
	}
}

func TestValidateSudoCommand_SeesSudoBehindWrappers(t *testing.T) {
	tests := []struct {
		cmd   string
		valid bool
	}{
		{"env sudo apt update", false},
		{"env -i PATH=/usr/bin sudo apt update", false},
		{"env -u HOME sudo apt update", false},
		{"nice -n 10 sudo make install", false},
		{"nice -10 sudo make install", false},
		{"nohup sudo make install", false},
		{"timeout 60 sudo zypper refresh", false},
		{"timeout -s KILL 60 sudo zypper refresh", false},
		{"command sudo id", false},
		{"exec sudo id", false},
		{"time -p sudo id", false},
		{"stdbuf -oL sudo journalctl", false},
		{"setsid sudo id", false},
		{"ionice -c 3 sudo id", false},
		{"taskset 0x1 sudo id", false},
		{"find . -name '*.o' | xargs sudo rm", false},
		{"xargs -I {} sudo cp {} /opt", false},
		{"xargs -I{} sudo cp {} /opt", false},
		{"find /tmp -name x -exec sudo rm {} \\;", false},
		{"find /tmp -name x -execdir sudo rm {} +", false},
		{`bash -c "sudo zypper refresh"`, false},
		{`bash -lc 'sudo zypper refresh'`, false},
		{`sh -c "echo hi; sudo id"`, false},
		{`bash -c "bash -c 'sudo id'"`, false},
		{"timeout 60 nice sudo id", false},
		{"env sudo -A apt update", true},
		{"timeout 60 sudo -A zypper refresh", true},
		{"xargs -0 sudo -A rm", true},
		{`bash -lc "sudo -A make install"`, true},
		{"find . -exec sudo -A rm {} \\;", true},
		{"command -v sudo", true},
		{"timeout 5 grep sudo /var/log/messages", true},
		{"env FOO=sudo ls", true},
		{"echo sudo", true},
		{"xargs -I{} echo sudo {}", true},
		{"bash script.sh sudo", true},
		{"ionice -p 1234", true},
	}
	for _, tt := range tests {
		if err := ValidateSudoCommand(tt.cmd); (err == nil) != tt.valid {
			t.Errorf("ValidateSudoCommand(%q) valid=%v, want %v (err=%v)", tt.cmd, err == nil, tt.valid, err)
		}
	}
}

func TestAgentShellEnv_DropsHerdrPaneVariables(t *testing.T) {
	got := agentShellEnv(
		[]string{"PATH=/bin", "HERDR_ENV=1", "HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH=/s", "HOME=/u"},
		"SUDO_ASKPASS=/x",
	)
	want := []string{"PATH=/bin", "HOME=/u", "SUDO_ASKPASS=/x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("agentShellEnv = %v, want %v: a nested agent in a bash command must not claim ask's herdr pane", got, want)
	}
}

// A background sub-agent stays owed to the model until the model has its
// output: read after it finished, killed by the model, or handed over.
// Reading it while it runs does not count, and shell jobs are never owed.
func TestJobManager_UndeliveredAgents(t *testing.T) {
	env, _ := newTestToolEnv(t)
	jobs := env.Jobs
	jobs.Add("sleep 100", false, nil)
	first := jobs.AddAgent("agent: one", nil)
	second := jobs.AddAgent("agent reviewer: two", nil)
	var third *Job
	third = jobs.AddAgent("agent: three", func() { third.Finish(ShellResult{ExitCode: 1}) })
	owed := func() string {
		var ids []string
		for _, j := range jobs.UndeliveredAgents() {
			ids = append(ids, j.ID)
		}
		return strings.Join(ids, ",")
	}
	if got := owed(); got != "job-2,job-3,job-4" {
		t.Fatalf("owed = %q, want the three agents in start order", got)
	}

	read := JobOutputTool(env)
	runTool(t, read, JobOutputParams{JobID: first.ID})
	if got := owed(); got != "job-2,job-3,job-4" {
		t.Errorf("reading a running agent must not count as delivered: %q", got)
	}
	first.AppendOutput("report one")
	first.Finish(ShellResult{ExitCode: 0})
	runTool(t, read, JobOutputParams{JobID: first.ID})
	if got := owed(); got != "job-3,job-4" {
		t.Errorf("reading a finished agent delivers it: %q", got)
	}

	runTool(t, JobKillTool(env), JobKillParams{JobID: third.ID})
	if got := owed(); got != "job-3" {
		t.Errorf("an agent the model killed is not owed: %q", got)
	}
	second.MarkDelivered()
	if got := owed(); got != "" {
		t.Errorf("owed after the last delivery = %q", got)
	}
}
