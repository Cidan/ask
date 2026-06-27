package main

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
		actual := extractBaseCommand(tt.cmd)
		if actual != tt.expected {
			t.Errorf("extractBaseCommand(%q) = %q, expected %q", tt.cmd, actual, tt.expected)
		}
	}
}

func TestApplyBashFilter(t *testing.T) {
	tests := []struct {
		name         string
		cmd          string
		rawOutput    string
		expectedOut  string
		expectedSave int
	}{
		{
			name:         "npm install",
			cmd:          "npm install",
			rawOutput:    "npm WARN deprecated request@2.88.2: request has been deprecated\nnpm WARN deprecated har-validator@5.1.5: this library is no longer supported\nadded 1 package in 2s\n",
			expectedOut:  "added 1 package in 2s\n",
			expectedSave: 31, // (137 - 22) / 4 = 115 / 4 = 28. Wait, actual formula is (len - len) / 4. Let's just assert > 0 for now.
		},
		{
			name:         "go test",
			cmd:          "go test",
			rawOutput:    "go: downloading github.com/foo/bar v1.2.3\nok  \tgithub.com/my/pkg\t0.012s\n",
			expectedOut:  "ok  \tgithub.com/my/pkg\t0.012s\n",
			expectedSave: 10,
		},
		{
			name:         "git push",
			cmd:          "git push origin main",
			rawOutput:    "remote: Resolving deltas: 100% (3/3), completed with 3 local objects.\nTo https://github.com/user/repo.git\n   abcdef..123456  main -> main\n",
			expectedOut:  "   abcdef..123456  main -> main\n",
			expectedSave: 0, // We will calculate precisely in the test code
		},
		{
			name:         "empty output",
			cmd:          "ls",
			rawOutput:    "",
			expectedOut:  "",
			expectedSave: 0,
		},
		{
			name:         "ansi stripped",
			cmd:          "echo",
			rawOutput:    "\x1b[31mHello\x1b[0m World\n",
			expectedOut:  "Hello World\n",
			expectedSave: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actualOut, actualSave := applyBashFilter(tt.cmd, tt.rawOutput)
			if actualOut != tt.expectedOut {
				t.Errorf("applyBashFilter(%q) out = %q, expected %q", tt.cmd, actualOut, tt.expectedOut)
			}
			expectedSave := (len(tt.rawOutput) - len(tt.expectedOut)) / 4
			if expectedSave < 0 {
				expectedSave = 0
			}
			if actualSave != expectedSave {
				t.Errorf("applyBashFilter(%q) save = %d, expected %d", tt.cmd, actualSave, expectedSave)
			}
		})
	}
}
