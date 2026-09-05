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
	env, events := newTestToolEnv(t)
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

	// Check bg events
	startedFound := false
	endedFound := false
	for _, ev := range *events {
		if _, ok := ev.(engine.BgTaskStartedEvent); ok {
			startedFound = true
		}
		if _, ok := ev.(engine.BgTaskEndedEvent); ok {
			endedFound = true
		}
	}
	if !startedFound || !endedFound {
		t.Errorf("expected started and ended bg task events, got: %+v", *events)
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
