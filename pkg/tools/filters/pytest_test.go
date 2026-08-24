package filters

import (
	"strings"
	"testing"
)

// A clean pytest run collapses to just its summary line.
func TestPytest_SuccessCollapses(t *testing.T) {
	raw := strings.Join([]string{
		"============================= test session starts ==============================",
		"platform linux -- Python 3.11.0, pytest-7.4.0",
		"rootdir: /home/x/proj",
		"collected 42 items",
		"",
		"tests/test_a.py ......................                                    [ 50%]",
		"tests/test_b.py ....................                                      [100%]",
		"",
		"============================== 42 passed in 1.23s ==============================",
	}, "\n") + "\n"
	out, saved := Apply("pytest -q", raw, 0)
	if out != "============================== 42 passed in 1.23s ==============================\n" {
		t.Fatalf("pytest success = %q", out)
	}
	if saved <= 0 {
		t.Errorf("expected savings, got %d", saved)
	}
}

// A failing run keeps the FAILURES section and drops the preamble.
func TestPytest_FailureKeepsDetail(t *testing.T) {
	raw := strings.Join([]string{
		"============================= test session starts ==============================",
		"collected 3 items",
		"",
		"tests/test_a.py .F.                                                       [100%]",
		"",
		"=================================== FAILURES ===================================",
		"_________________________________ test_math __________________________________",
		"",
		"    def test_math():",
		">       assert 1 + 1 == 3",
		"E       assert 2 == 3",
		"",
		"tests/test_a.py:5: AssertionError",
		"=========================== short test summary info ============================",
		"FAILED tests/test_a.py::test_math - assert 2 == 3",
		"========================= 1 failed, 2 passed in 0.10s ==========================",
	}, "\n") + "\n"
	out, _ := Apply("python -m pytest", raw, 1)
	if strings.Contains(out, "test session starts") {
		t.Errorf("preamble not dropped: %q", out)
	}
	if !strings.Contains(out, "assert 2 == 3") || !strings.Contains(out, "FAILURES") {
		t.Errorf("failure detail dropped: %q", out)
	}
	if !strings.Contains(out, "1 failed, 2 passed") {
		t.Errorf("summary line dropped: %q", out)
	}
}

// Output with no recognizable pytest summary is left untouched.
func TestPytest_UnknownShapePreserved(t *testing.T) {
	raw := "ImportError while loading conftest\nTraceback (most recent call last):\n"
	out, _ := Apply("pytest", raw, 2)
	if out != raw {
		t.Errorf("unknown pytest shape not preserved: %q", out)
	}
}
