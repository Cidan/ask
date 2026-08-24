package filters

import (
	"strings"
	"testing"
)

func TestInstall_NpmStripsWarnings(t *testing.T) {
	raw := strings.Join([]string{
		"npm WARN deprecated request@2.88.2: request has been deprecated",
		"npm WARN deprecated har-validator@5.1.5: no longer supported",
		"added 1 package in 2s",
		"",
		"1 package is looking for funding",
		"  run `npm fund` for details",
	}, "\n") + "\n"

	out, _ := Apply("npm install", raw, 0)
	if out != "added 1 package in 2s\n" {
		t.Fatalf("npm install out = %q", out)
	}
}

func TestInstall_YarnAddMatches(t *testing.T) {
	raw := "warning package@1.0.0: deprecated\nsuccess Saved 1 new dependency.\n"
	out, _ := Apply("yarn add left-pad", raw, 0)
	if strings.Contains(out, "deprecated") {
		t.Errorf("yarn warning not stripped: %q", out)
	}
	if !strings.Contains(out, "success Saved 1 new dependency.") {
		t.Errorf("result line dropped: %q", out)
	}
}

// A script run (yarn build) is NOT an install — its warnings must survive.
func TestInstall_ScriptRunNotMatched(t *testing.T) {
	raw := "warning: deprecated API used\nbuild complete\n"
	out, _ := Apply("yarn build", raw, 0)
	if !strings.Contains(out, "deprecated API used") {
		t.Errorf("yarn build warning was stripped as install noise: %q", out)
	}
}

// A failed install keeps everything so the error survives.
func TestInstall_FailedPreserved(t *testing.T) {
	raw := "npm WARN deprecated x\nnpm ERR! code E404\nnpm ERR! 404 Not Found\n"
	out, _ := Apply("npm install", raw, 1)
	if out != raw {
		t.Errorf("failed install not preserved: %q", out)
	}
}
