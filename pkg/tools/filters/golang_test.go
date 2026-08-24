package filters

import (
	"strings"
	"testing"
)

// go test -json for an all-passing run collapses to a single summary line.
func TestGo_JSONPassCollapses(t *testing.T) {
	raw := strings.Join([]string{
		`{"Action":"run","Package":"x/p","Test":"TestA"}`,
		`{"Action":"output","Package":"x/p","Test":"TestA","Output":"=== RUN   TestA\n"}`,
		`{"Action":"pass","Package":"x/p","Test":"TestA","Elapsed":0.01}`,
		`{"Action":"run","Package":"x/p","Test":"TestB"}`,
		`{"Action":"pass","Package":"x/p","Test":"TestB","Elapsed":0.02}`,
		`{"Action":"pass","Package":"x/p","Elapsed":0.05}`,
	}, "\n") + "\n"

	out, saved := Apply("go test -json ./...", raw, 0)
	if out != "go test: 2 passed across 1 packages (0.1s)\n" {
		t.Fatalf("summary = %q", out)
	}
	if saved <= 0 {
		t.Errorf("expected savings, got %d", saved)
	}
}

// A failing -json run keeps the summary plus the failing test's output,
// and drops the passing tests' output entirely.
func TestGo_JSONFailKeepsDetail(t *testing.T) {
	raw := strings.Join([]string{
		`{"Action":"run","Package":"x/p","Test":"TestOK"}`,
		`{"Action":"output","Package":"x/p","Test":"TestOK","Output":"noise that should vanish\n"}`,
		`{"Action":"pass","Package":"x/p","Test":"TestOK","Elapsed":0.01}`,
		`{"Action":"run","Package":"x/p","Test":"TestBad"}`,
		`{"Action":"output","Package":"x/p","Test":"TestBad","Output":"=== RUN   TestBad\n"}`,
		`{"Action":"output","Package":"x/p","Test":"TestBad","Output":"    foo_test.go:42: want 1 got 2\n"}`,
		`{"Action":"fail","Package":"x/p","Test":"TestBad","Elapsed":0.0}`,
		`{"Action":"fail","Package":"x/p","Elapsed":0.03}`,
	}, "\n") + "\n"

	out, _ := Apply("go test -json ./...", raw, 1)
	if !strings.Contains(out, "1 passed, 1 failed across 1 packages") {
		t.Errorf("summary missing counts: %q", out)
	}
	if !strings.Contains(out, "FAIL x/p TestBad") {
		t.Errorf("failing test header missing: %q", out)
	}
	if !strings.Contains(out, "foo_test.go:42: want 1 got 2") {
		t.Errorf("failure detail dropped: %q", out)
	}
	if strings.Contains(out, "noise that should vanish") {
		t.Errorf("passing-test output was kept: %q", out)
	}
	if strings.Contains(out, "=== RUN") {
		t.Errorf("RUN marker survived inside detail: %q", out)
	}
}

// Events all pass but the process exited nonzero — something unmodeled
// broke, so the raw stream is preserved rather than a misleading summary.
func TestGo_JSONPassButNonzeroExitPreservesRaw(t *testing.T) {
	raw := `{"Action":"pass","Package":"x/p","Elapsed":0.05}` + "\n"
	out, _ := Apply("go test -json ./...", raw, 2)
	if out != raw {
		t.Errorf("unmodeled failure not preserved: %q", out)
	}
}

// Human (non-json) go test drops RUN/PASS chatter but keeps ok lines.
func TestGo_TextModeStripsChatter(t *testing.T) {
	raw := strings.Join([]string{
		"=== RUN   TestA",
		"--- PASS: TestA (0.00s)",
		"=== RUN   TestB",
		"--- PASS: TestB (0.00s)",
		"PASS",
		"ok  \tx/p\t0.012s",
	}, "\n") + "\n"

	out, _ := Apply("go test ./...", raw, 0)
	if out != "ok  \tx/p\t0.012s\n" {
		t.Fatalf("text-mode out = %q", out)
	}
}

// Human go test with a failure keeps the FAIL block and the assertion line.
func TestGo_TextModeKeepsFailure(t *testing.T) {
	raw := strings.Join([]string{
		"=== RUN   TestBad",
		"    foo_test.go:9: boom",
		"--- FAIL: TestBad (0.00s)",
		"FAIL",
		"FAIL\tx/p\t0.010s",
	}, "\n") + "\n"

	out, _ := Apply("go test ./...", raw, 1)
	if !strings.Contains(out, "--- FAIL: TestBad") || !strings.Contains(out, "foo_test.go:9: boom") {
		t.Errorf("failure detail dropped: %q", out)
	}
	if strings.Contains(out, "=== RUN") {
		t.Errorf("RUN chatter survived: %q", out)
	}
}

// A nonzero exit with no FAIL marker (compile error, panic before a test)
// must pass through untouched.
func TestGo_TextModeCompileErrorPreserved(t *testing.T) {
	raw := "# x/p\n./foo.go:3:2: undefined: Bar\n"
	out, _ := Apply("go test ./...", raw, 2)
	if out != raw {
		t.Errorf("compile error not preserved: %q", out)
	}
}

// go build is pass/fail: success collapses to "go: ok", a failure keeps the
// compile error with download chatter stripped.
func TestGo_BuildPassFail(t *testing.T) {
	if out, _ := Apply("go build ./...", "go: downloading example.com/m v1.2.3\n", 0); out != "go: ok\n" {
		t.Errorf("go build success = %q, want 'go: ok'", out)
	}
	fail := "go: downloading example.com/m v1.2.3\n# pkg\n./foo.go:3:2: undefined: Bar\n"
	out, _ := Apply("go build ./...", fail, 2)
	if strings.Contains(out, "downloading") || !strings.Contains(out, "undefined: Bar") {
		t.Errorf("go build failure = %q", out)
	}
}

// go run is not filtered as a build — the program's output passes through.
func TestGo_RunPassesThrough(t *testing.T) {
	if out, _ := Apply("go run ./cmd/x", "hello from program\n", 0); out != "hello from program\n" {
		t.Errorf("go run should pass through, got %q", out)
	}
}

// go mod keeps everything but download chatter.
func TestGo_ModStripsDownloads(t *testing.T) {
	out, _ := Apply("go mod download", "go: downloading example.com/m v1.2.3\nall good\n", 0)
	if out != "all good\n" {
		t.Errorf("go mod = %q", out)
	}
}
