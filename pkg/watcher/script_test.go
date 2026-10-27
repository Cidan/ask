package watcher

import (
	"strings"
	"testing"
	"time"
)

func TestHeadTailBuffer_KeepsHeadAndTail(t *testing.T) {
	b := &headTailBuffer{max: 10}
	for _, chunk := range []string{"abc", "defgh", "ijklmnop", "qrstuvwxyz"} {
		_, _ = b.Write([]byte(chunk))
	}
	got := b.String()
	if !strings.HasPrefix(got, "abcde\n") || !strings.HasSuffix(got, "\nvwxyz") {
		t.Fatalf("head/tail = %q", got)
	}
	if !strings.Contains(got, "[16 bytes cut]") || b.total != 26 {
		t.Errorf("cut marker or total wrong: %q %d", got, b.total)
	}

	small := &headTailBuffer{max: 10}
	_, _ = small.Write([]byte("tiny"))
	if small.String() != "tiny" {
		t.Errorf("a short write must pass through: %q", small.String())
	}

	tail := &headTailBuffer{max: 4, tailOnly: true}
	_, _ = tail.Write([]byte("0123456789"))
	if got := tail.String(); got != "[truncated] …\n6789" {
		t.Errorf("tailOnly = %q", got)
	}

	// A cut through a multi-byte character drops the pieces.
	r := &headTailBuffer{max: 4}
	_, _ = r.Write([]byte("aéééb"))
	if s := r.String(); !strings.HasPrefix(s, "a\n") || !strings.HasSuffix(s, "\nb") {
		t.Errorf("rune-split cut = %q", s)
	}
}

func TestSpecFor_CommandOrScriptAndEnvironment(t *testing.T) {
	isolate(t)
	d := sampleDef("prs")
	d.Project = "/tmp"
	last := time.Date(2026, 10, 26, 9, 5, 0, 0, time.UTC)
	spec := SpecFor(d, "/bin/bash", last)
	if spec.Command != "./check.sh" || spec.Path != "" || spec.Shell != "/bin/bash" || spec.Dir != "/tmp" {
		t.Errorf("command spec = %+v", spec)
	}
	if spec.Timeout != DefaultTimeout {
		t.Errorf("default timeout = %v", spec.Timeout)
	}
	want := []string{
		EnvName + "=prs",
		EnvStateDir + "=" + StateDir(d.Ref()),
		EnvLastRun + "=2026-10-26T09:05:00Z",
	}
	if strings.Join(spec.Env, "|") != strings.Join(want, "|") {
		t.Errorf("env = %v, want %v", spec.Env, want)
	}

	d.Command, d.HasScript, d.TimeoutSecs = "", true, 30
	spec = SpecFor(d, "/bin/bash", time.Time{})
	if spec.Command != "" || spec.Path != ScriptPath(d.Ref()) || spec.Timeout != 30*time.Second {
		t.Errorf("script spec = %+v", spec)
	}
	if spec.Env[2] != EnvLastRun+"=" {
		t.Errorf("first run must pass an empty last-run time: %q", spec.Env[2])
	}
}

func TestScriptResult_Failure(t *testing.T) {
	cases := []struct {
		res  ScriptResult
		want string
	}{
		{ScriptResult{}, ""},
		{ScriptResult{ExitCode: 3}, "exit 3"},
		{ScriptResult{TimedOut: true, ExitCode: -1}, "timed out"},
		{ScriptResult{Cancelled: true, ExitCode: -1}, "stopped"},
	}
	for _, c := range cases {
		if got := c.res.FailureText(); got != c.want {
			t.Errorf("FailureText(%+v) = %q, want %q", c.res, got, c.want)
		}
		if c.res.Failed() != (c.want != "") {
			t.Errorf("Failed(%+v) = %v", c.res, c.res.Failed())
		}
	}
}

func TestPrompt_InstructionsThenOutput(t *testing.T) {
	d := sampleDef("prs")
	at := time.Date(2026, 10, 26, 9, 5, 0, 0, time.UTC)
	p := Prompt(d, ScriptResult{Stdout: "#412 fix it\n", StdoutBytes: 12}, at)
	for _, want := range []string{
		"<instructions>\nreview what it found\n</instructions>",
		"The watcher \"prs\" ran `./check.sh` in /tmp at " + at.Local().Format("2006-01-02 15:04 MST"),
		"<output>\n#412 fix it\n</output>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Index(p, "<instructions>") > strings.Index(p, "<output>") {
		t.Error("instructions must come before the output")
	}

	d.Instructions = ""
	cut := Prompt(d, ScriptResult{Stdout: "head…tail", StdoutBytes: 500000}, at)
	if strings.Contains(cut, "<instructions>") || !strings.Contains(cut, "The output was 500000 bytes") {
		t.Errorf("prompt without instructions / with a cut:\n%s", cut)
	}
}
