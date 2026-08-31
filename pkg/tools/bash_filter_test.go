package tools

import (
	"testing"
)

func TestExtractBaseCommand(t *testing.T) {
	tests := []struct {
		cmd      string
		expected string
	}{
		{"go test ./...", "go test"},
		{"npm install --save react", "npm install"},
		{"git push origin main", "git push"},
		{"yarn build", "yarn build"},
		{"ls -la", "ls"},
		{"", ""},
	}

	for _, tt := range tests {
		actual := ExtractBaseCommand(tt.cmd)
		if actual != tt.expected {
			t.Errorf("ExtractBaseCommand(%q) = %q, expected %q", tt.cmd, actual, tt.expected)
		}
	}
}

// ExtractBaseCommand resolves the ledger key past global flags and
// pipelines, so the ledger key agrees with the filter that ran.
func TestExtractBaseCommand_FlagsAndPipelines(t *testing.T) {
	tests := []struct{ cmd, want string }{
		{"git --no-pager diff", "git diff"},
		{"git -C /repo status", "git status"},
		{"go test ./... 2>&1 | tail -n 40", "go test"},
		{"cd build && cmake .. && make -j4", "make"},
		{"kubectl get pods -o wide | head", "kubectl get"},
		{"sudo -A make install", "make"},
		{`python3 -c "import sys; print('|'.join(sys.argv))"`, "python3"},
	}
	for _, tt := range tests {
		if got := ExtractBaseCommand(tt.cmd); got != tt.want {
			t.Errorf("ExtractBaseCommand(%q) = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

// IsTrivialCommand excludes pagers/builtins/file reads but keeps real
// build/test/tooling commands even when they are piped into a pager.
func TestIsTrivialCommand(t *testing.T) {
	for _, c := range []string{"cat foo", "head -100 log", "ls -la", "grep -rn x .", ""} {
		if !IsTrivialCommand(c) {
			t.Errorf("IsTrivialCommand(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"go test ./...", "git --no-pager diff | head", "kubectl get pods | head", "make build"} {
		if IsTrivialCommand(c) {
			t.Errorf("IsTrivialCommand(%q) = true, want false", c)
		}
	}
}

func TestApplyBashFilter(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		rawOutput   string
		expectedOut string
	}{
		{
			name:        "npm install",
			cmd:         "npm install",
			rawOutput:   "npm WARN deprecated request@2.88.2: request has been deprecated\nnpm WARN deprecated har-validator@5.1.5: this library is no longer supported\nadded 1 package in 2s\n",
			expectedOut: "added 1 package in 2s\n",
		},
		{
			name:        "go test",
			cmd:         "go test",
			rawOutput:   "go: downloading github.com/foo/bar v1.2.3\nok  \tgithub.com/my/pkg\t0.012s\n",
			expectedOut: "ok  \tgithub.com/my/pkg\t0.012s\n",
		},
		{
			name:        "git push",
			cmd:         "git push origin main",
			rawOutput:   "remote: Resolving deltas: 100% (3/3), completed with 3 local objects.\nTo https://github.com/user/repo.git\n   abcdef..123456  main -> main\n",
			expectedOut: "   abcdef..123456  main -> main\n",
		},
		{
			name:        "empty output",
			cmd:         "ls",
			rawOutput:   "",
			expectedOut: "",
		},
		{
			name:        "ansi stripped",
			cmd:         "echo",
			rawOutput:   "\x1b[31mHello\x1b[0m World\n",
			expectedOut: "Hello World\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actualOut, actualSave := ApplyBashFilter(tt.cmd, tt.rawOutput, 0)
			if actualOut != tt.expectedOut {
				t.Errorf("ApplyBashFilter(%q) out = %q, expected %q", tt.cmd, actualOut, tt.expectedOut)
			}
			expectedSave := (len(tt.rawOutput) - len(tt.expectedOut)) / 4
			if expectedSave < 0 {
				expectedSave = 0
			}
			if actualSave != expectedSave {
				t.Errorf("ApplyBashFilter(%q) save = %d, expected %d", tt.cmd, actualSave, expectedSave)
			}
		})
	}
}
