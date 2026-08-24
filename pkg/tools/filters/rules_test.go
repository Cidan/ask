package filters

import (
	"strings"
	"testing"
)

// make is pass/fail: any successful build collapses to "make: ok".
func TestRules_MakePassFailSuccess(t *testing.T) {
	raw := strings.Join([]string{
		"Building llama.cpp static libraries...",
		"[  5%] Building CXX object foo.o",
		"[100%] Built target llama",
		"gmake[1]: Leaving directory '/x'",
		"go build -o bin/ask ./cmd/ask",
	}, "\n") + "\n"
	out, saved := Apply("make build", raw, 0)
	if out != "make: ok\n" {
		t.Fatalf("make success = %q, want 'make: ok'", out)
	}
	if saved <= 0 {
		t.Errorf("expected savings, got %d", saved)
	}
}

// A failed make strips the cmake progress/probe noise but keeps the error.
func TestRules_MakePassFailFailure(t *testing.T) {
	raw := strings.Join([]string{
		"[ 12%] Building CXX object foo.o",
		"-- Detecting C compiler ABI info",
		"src/foo.c:9:2: error: undefined reference to `bar'",
		"gmake[1]: *** [Makefile:5: all] Error 1",
	}, "\n") + "\n"
	out, _ := Apply("make build", raw, 2)
	if strings.Contains(out, "12%") || strings.Contains(out, "Detecting C compiler") {
		t.Errorf("progress/probe noise survived on failure: %q", out)
	}
	if !strings.Contains(out, "error: undefined reference") || !strings.Contains(out, "Error 1") {
		t.Errorf("failure detail dropped: %q", out)
	}
}

// cargo build is pass/fail: a clean build collapses to "cargo: ok".
func TestRules_CargoBuildPassFail(t *testing.T) {
	raw := strings.Join([]string{
		"   Compiling libc v0.2.0",
		"    Updating crates.io index",
		"    Finished dev [unoptimized] target(s) in 3.21s",
	}, "\n") + "\n"
	if out, _ := Apply("cargo build", raw, 0); out != "cargo: ok\n" {
		t.Fatalf("cargo build success = %q, want 'cargo: ok'", out)
	}
}

// A cargo build error survives, with the Compiling chatter stripped.
func TestRules_CargoBuildErrorSurvives(t *testing.T) {
	raw := "   Compiling app v0.1.0\nerror[E0308]: mismatched types\n  --> src/main.rs:2:5\n"
	out, _ := Apply("cargo build", raw, 101)
	if !strings.Contains(out, "error[E0308]: mismatched types") {
		t.Errorf("cargo error dropped: %q", out)
	}
	if strings.Contains(out, "Compiling") {
		t.Errorf("cargo noise survived on failure: %q", out)
	}
}

// cargo run is not a build — the program's output passes through.
func TestRules_CargoRunPassesThrough(t *testing.T) {
	if out, _ := Apply("cargo run", "hello from rust\n", 0); out != "hello from rust\n" {
		t.Errorf("cargo run should pass through, got %q", out)
	}
}

func TestRules_PipStripsResolverNoise(t *testing.T) {
	raw := strings.Join([]string{
		"Requirement already satisfied: pip in ./venv (23.0)",
		"Collecting requests",
		"  Downloading requests-2.31.0-py3-none-any.whl (62 kB)",
		"  Using cached urllib3-2.0.0.whl",
		"Installing collected packages: urllib3, requests",
		"Successfully installed requests-2.31.0 urllib3-2.0.0",
	}, "\n") + "\n"
	out, _ := Apply("pip install requests", raw, 0)
	if out != "Successfully installed requests-2.31.0 urllib3-2.0.0\n" {
		t.Fatalf("pip out = %q", out)
	}
}

// `python -m pip install` matches the same rule.
func TestRules_PipViaPythonM(t *testing.T) {
	raw := "Collecting flask\nSuccessfully installed flask-3.0.0\n"
	out, _ := Apply("python -m pip install flask", raw, 0)
	if out != "Successfully installed flask-3.0.0\n" {
		t.Errorf("python -m pip out = %q", out)
	}
}

// A gradle build that prints BUILD SUCCESSFUL collapses to "gradle: ok".
func TestRules_GradleBuildSuccessCollapses(t *testing.T) {
	raw := strings.Join([]string{
		"> Task :compileJava UP-TO-DATE",
		"> Task :test",
		"BUILD SUCCESSFUL in 2s",
	}, "\n") + "\n"
	if out, _ := Apply("./gradlew build", raw, 0); out != "gradle: ok\n" {
		t.Fatalf("gradle success = %q, want 'gradle: ok'", out)
	}
}

// A long-running gradle task with no BUILD SUCCESSFUL is not collapsed — its
// output (minus the UP-TO-DATE/SKIPPED chatter) passes through.
func TestRules_GradleRunNotCollapsed(t *testing.T) {
	raw := strings.Join([]string{
		"> Task :compileJava UP-TO-DATE",
		"> Task :bootRun",
		"Tomcat started on port 8080",
	}, "\n") + "\n"
	out, _ := Apply("./gradlew bootRun", raw, 0)
	if strings.Contains(out, "UP-TO-DATE") {
		t.Errorf("gradle chatter survived: %q", out)
	}
	if !strings.Contains(out, "Tomcat started on port 8080") {
		t.Errorf("gradle run output was swallowed: %q", out)
	}
}

func TestRules_CargoTestFailuresOnly(t *testing.T) {
	raw := strings.Join([]string{
		"   Compiling app v0.1.0",
		"    Finished test [unoptimized] target(s) in 2.0s",
		"     Running unittests src/lib.rs",
		"running 3 tests",
		"test tests::ok_a ... ok",
		"test tests::ok_b ... ok",
		"test tests::bad ... FAILED",
		"",
		"failures:",
		"",
		"---- tests::bad stdout ----",
		"thread 'tests::bad' panicked at 'assertion failed'",
		"",
		"test result: FAILED. 2 passed; 1 failed; 0 ignored",
	}, "\n") + "\n"
	out, _ := Apply("cargo test", raw, 101)
	if strings.Contains(out, "... ok") || strings.Contains(out, "Compiling") || strings.Contains(out, "running 3 tests") {
		t.Errorf("cargo test noise survived: %q", out)
	}
	if !strings.Contains(out, "tests::bad ... FAILED") || !strings.Contains(out, "panicked at") {
		t.Errorf("cargo test failure dropped: %q", out)
	}
	if !strings.Contains(out, "test result: FAILED") {
		t.Errorf("cargo test result dropped: %q", out)
	}
}

// cargo test routes to the test rule, not the generic cargo build rule.
func TestRules_CargoTestSuccessCollapses(t *testing.T) {
	raw := strings.Join([]string{
		"   Compiling app v0.1.0",
		"    Finished test target(s) in 1.0s",
		"     Running unittests src/lib.rs",
		"running 2 tests",
		"test a ... ok",
		"test b ... ok",
		"",
		"test result: ok. 2 passed; 0 failed; 0 ignored; finished in 0.00s",
	}, "\n") + "\n"
	out, _ := Apply("cargo test --all", raw, 0)
	if out != "test result: ok. 2 passed; 0 failed; 0 ignored; finished in 0.00s\n" {
		t.Fatalf("cargo test success = %q", out)
	}
}

func TestRules_JSTestStripsPassing(t *testing.T) {
	raw := strings.Join([]string{
		"PASS src/a.test.js",
		"PASS src/b.test.js",
		"FAIL src/c.test.js",
		"  ● renders › fails",
		"    expect(received).toBe(expected)",
		"",
		"Test Suites: 1 failed, 2 passed, 3 total",
		"Tests:       1 failed, 5 passed, 6 total",
	}, "\n") + "\n"
	out, _ := Apply("jest", raw, 1)
	if strings.Contains(out, "PASS src/") {
		t.Errorf("jest passing suites survived: %q", out)
	}
	if !strings.Contains(out, "FAIL src/c.test.js") || !strings.Contains(out, "toBe(expected)") {
		t.Errorf("jest failure detail dropped: %q", out)
	}
	if !strings.Contains(out, "Tests:       1 failed, 5 passed") {
		t.Errorf("jest summary dropped: %q", out)
	}
}

func TestRules_MypySuccessCollapses(t *testing.T) {
	raw := "Success: no issues found in 12 source files\n"
	if out, _ := Apply("mypy .", raw, 0); out != "mypy: ok\n" {
		t.Errorf("mypy success = %q", out)
	}
	// A run with errors is not collapsed.
	bad := "app.py:3: error: Name 'x' is not defined  [name-defined]\nFound 1 error in 1 file\n"
	out, _ := Apply("mypy app.py", bad, 1)
	if !strings.Contains(out, "Name 'x' is not defined") {
		t.Errorf("mypy error dropped: %q", out)
	}
}

// bazel build is pass/fail: "bazel: ok" on success, error + stripped
// progress on failure.
func TestRules_BazelBuildPassFail(t *testing.T) {
	ok := "Loading: 0 packages loaded\n[100 / 200] Compiling bar.cc\nINFO: Build completed successfully\n"
	if out, _ := Apply("bazel build //...", ok, 0); out != "bazel: ok\n" {
		t.Fatalf("bazel build success = %q, want 'bazel: ok'", out)
	}
	fail := strings.Join([]string{
		"Analyzing: 2 targets",
		"[1,234 / 5,678] Compiling foo.cc",
		"ERROR: missing input file //x:y",
	}, "\n") + "\n"
	out, _ := Apply("bazel build //...", fail, 1)
	if strings.Contains(out, "[1,234 / 5,678]") || strings.Contains(out, "Analyzing:") {
		t.Errorf("bazel progress survived on failure: %q", out)
	}
	if !strings.Contains(out, "ERROR: missing input file") {
		t.Errorf("bazel error dropped: %q", out)
	}
}

// bazel test (not build) keeps its output via the generic bazel rule.
func TestRules_BazelTestKeepsResults(t *testing.T) {
	raw := "[10 / 20] Testing //x:y\n//x:y PASSED in 0.5s\nExecuted 1 out of 1 test: 1 test passes.\n"
	out, _ := Apply("bazel test //x:y", raw, 0)
	if strings.Contains(out, "[10 / 20]") {
		t.Errorf("bazel test progress survived: %q", out)
	}
	if !strings.Contains(out, "1 test passes") {
		t.Errorf("bazel test result dropped: %q", out)
	}
}

func TestRules_TerraformStripsRefresh(t *testing.T) {
	raw := strings.Join([]string{
		"aws_instance.web: Refreshing state... [id=i-123]",
		"data.aws_ami.ubuntu: Reading...",
		"data.aws_ami.ubuntu: Read complete after 1s [id=ami-1]",
		"Plan: 1 to add, 0 to change, 0 to destroy.",
	}, "\n") + "\n"
	out, _ := Apply("terraform plan", raw, 0)
	if strings.Contains(out, "Refreshing state") || strings.Contains(out, "Reading...") {
		t.Errorf("terraform refresh noise survived: %q", out)
	}
	if !strings.Contains(out, "Plan: 1 to add") {
		t.Errorf("terraform plan summary dropped: %q", out)
	}
}

// docker build is pass/fail: "docker build: ok" on success, full log on
// failure (KeepOnError).
func TestRules_DockerBuildPassFail(t *testing.T) {
	ok := strings.Join([]string{
		"#8 [internal] load build context",
		"#10 [builder 3/5] RUN go build ./...",
		"#10 DONE 13.0s",
		"#14 exporting to image",
	}, "\n") + "\n"
	if out, _ := Apply("docker build -t app .", ok, 0); out != "docker build: ok\n" {
		t.Fatalf("docker build success = %q, want 'docker build: ok'", out)
	}
	fail := "#10 [builder] RUN make\n#10 12.3 make: *** No rule to make target\n#10 ERROR: process did not complete\n"
	if out, _ := Apply("docker build .", fail, 1); out != fail {
		t.Errorf("failed docker build not preserved verbatim: %q", out)
	}
}
