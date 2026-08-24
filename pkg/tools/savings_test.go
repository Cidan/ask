package tools

import (
	"path/filepath"
	"testing"
)

// RecordSavings accumulates raw and saved tokens per command under an
// isolated config dir; LoadSavings reads them back.
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
	// Zero/negative savings are not recorded.
	if err := RecordSavings("ls", 50, 0); err != nil {
		t.Fatalf("record 4: %v", err)
	}

	s, err := LoadSavings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.TotalSavedTokens != 1400 || s.TotalRawTokens != 1700 {
		t.Errorf("totals: raw=%d saved=%d, want raw=1700 saved=1400", s.TotalRawTokens, s.TotalSavedTokens)
	}
	gt := s.ByCommand["go test"]
	if gt.Count != 2 || gt.RawTokens != 1500 || gt.SavedTokens != 1300 {
		t.Errorf("go test = %+v, want count=2 raw=1500 saved=1300", gt)
	}
	if _, ok := s.ByCommand["ls"]; ok {
		t.Errorf("zero-saving command should not be recorded")
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
