package tools

import (
	"path/filepath"
	"testing"
)

// RecordSavings accumulates run counts plus raw and saved tokens per
// command under an isolated config dir; LoadSavings reads them back.
func TestRecordAndLoadSavings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // os.UserConfigDir honors this on Linux

	if err := RecordSavings("go test", 1000, 900); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := RecordSavings("go test", 500, 400); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	if err := RecordSavings("git diff", 200, 100); err != nil {
		t.Fatalf("record 3: %v", err)
	}
	// A real command that saved nothing this run is still recorded — it is a
	// coverage data point. The trivial/pager exclusion happens a layer up in
	// the bash tool (IsTrivialCommand), not in RecordSavings.
	if err := RecordSavings("go build", 50, 0); err != nil {
		t.Fatalf("record 4: %v", err)
	}
	// A blank key is never recorded.
	if err := RecordSavings("", 10, 5); err != nil {
		t.Fatalf("record 5: %v", err)
	}

	s, err := LoadSavings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.TotalSavedTokens != 1400 || s.TotalRawTokens != 1750 {
		t.Errorf("totals: raw=%d saved=%d, want raw=1750 saved=1400", s.TotalRawTokens, s.TotalSavedTokens)
	}
	gt := s.ByCommand["go test"]
	if gt.Count != 2 || gt.RawTokens != 1500 || gt.SavedTokens != 1300 {
		t.Errorf("go test = %+v, want count=2 raw=1500 saved=1300", gt)
	}
	gb, ok := s.ByCommand["go build"]
	if !ok || gb.Count != 1 || gb.RawTokens != 50 || gb.SavedTokens != 0 {
		t.Errorf("go build = %+v (recorded=%v), want count=1 raw=50 saved=0", gb, ok)
	}
	if _, ok := s.ByCommand[""]; ok {
		t.Errorf("blank command should not be recorded")
	}
}

// A missing ledger loads as an empty, non-nil result.
func TestLoadSavingsMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "nope"))
	s, err := LoadSavings()
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if s.TotalSavedTokens != 0 || s.ByCommand == nil {
		t.Errorf("missing ledger not zero-valued: %+v", s)
	}
}
