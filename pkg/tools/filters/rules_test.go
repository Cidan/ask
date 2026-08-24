package filters

import (
	"strings"
	"testing"
)

func TestRules_MakeOnEmpty(t *testing.T) {
	raw := "make[1]: Entering directory '/x'\nmake[1]: Nothing to be done for 'all'.\nmake[1]: Leaving directory '/x'\n"
	out, _ := Apply("make all", raw, 0)
	if out != "make: nothing to do\n" {
		t.Fatalf("make on_empty = %q", out)
	}
}

func TestRules_CargoStripsCompiling(t *testing.T) {
	raw := strings.Join([]string{
		"   Compiling libc v0.2.0",
		"   Compiling serde v1.0.0",
		"    Updating crates.io index",
		"warning: unused variable: `x`",
		"    Finished dev [unoptimized] target(s) in 3.21s",
	}, "\n") + "\n"
	out, _ := Apply("cargo build", raw, 0)
	if strings.Contains(out, "Compiling") || strings.Contains(out, "Updating") {
		t.Errorf("cargo noise survived: %q", out)
	}
	if !strings.Contains(out, "warning: unused variable") {
		t.Errorf("cargo warning dropped: %q", out)
	}
	if !strings.Contains(out, "Finished dev") {
		t.Errorf("cargo Finished line dropped: %q", out)
	}
}

// A cargo build error survives (strip-only never touches error lines).
func TestRules_CargoErrorSurvives(t *testing.T) {
	raw := "   Compiling app v0.1.0\nerror[E0308]: mismatched types\n  --> src/main.rs:2:5\n"
	out, _ := Apply("cargo build", raw, 101)
	if !strings.Contains(out, "error[E0308]: mismatched types") {
		t.Errorf("cargo error dropped: %q", out)
	}
	if strings.Contains(out, "Compiling") {
		t.Errorf("cargo noise survived on failure: %q", out)
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

func TestRules_GradleStripsUpToDate(t *testing.T) {
	raw := strings.Join([]string{
		"> Task :compileJava UP-TO-DATE",
		"> Task :processResources NO-SOURCE",
		"> Task :test",
		"BUILD SUCCESSFUL in 2s",
	}, "\n") + "\n"
	out, _ := Apply("./gradlew build", raw, 0)
	if strings.Contains(out, "UP-TO-DATE") || strings.Contains(out, "NO-SOURCE") {
		t.Errorf("gradle noise survived: %q", out)
	}
	if !strings.Contains(out, "> Task :test") || !strings.Contains(out, "BUILD SUCCESSFUL") {
		t.Errorf("gradle signal dropped: %q", out)
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

func TestRules_BazelStripsProgress(t *testing.T) {
	raw := strings.Join([]string{
		"Loading: 0 packages loaded",
		"Analyzing: 2 targets",
		"[1,234 / 5,678] Compiling foo.cc",
		"ERROR: missing input file //x:y",
		"INFO: Elapsed time: 3.2s",
	}, "\n") + "\n"
	out, _ := Apply("bazel build //...", raw, 1)
	// KeepOnError: a failed bazel run is preserved verbatim.
	if out != raw {
		t.Errorf("failed bazel not preserved: %q", out)
	}
	// A successful run strips progress/loading noise.
	ok := "Loading: 0 packages loaded\n[100 / 200] Compiling bar.cc\nINFO: Build completed successfully\n"
	out2, _ := Apply("bazel build //...", ok, 0)
	if strings.Contains(out2, "Loading:") || strings.Contains(out2, "[100 / 200]") {
		t.Errorf("bazel progress survived: %q", out2)
	}
	if !strings.Contains(out2, "Build completed successfully") {
		t.Errorf("bazel result dropped: %q", out2)
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

func TestRules_DockerBuildKeepsLogsDropsBookkeeping(t *testing.T) {
	raw := strings.Join([]string{
		"#8 [internal] load build context",
		"#8 DONE 0.1s",
		"#10 [builder 3/5] RUN go build ./...",
		"#10 12.34 building the app",
		"#10 DONE 13.0s",
		"#14 exporting to image",
		"#14 sha256:abc123",
	}, "\n") + "\n"
	out, _ := Apply("docker build -t app .", raw, 0)
	if !strings.Contains(out, "RUN go build") || !strings.Contains(out, "building the app") {
		t.Errorf("docker build logs dropped: %q", out)
	}
	if strings.Contains(out, "DONE") || strings.Contains(out, "sha256:") || strings.Contains(out, "[internal]") {
		t.Errorf("docker bookkeeping survived: %q", out)
	}
}
