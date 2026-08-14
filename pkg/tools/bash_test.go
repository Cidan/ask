package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/engine"
)

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
