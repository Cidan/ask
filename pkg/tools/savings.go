package tools

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type CommandSavings struct {
	Count       int `json:"count"`
	RawTokens   int `json:"rawTokens"`
	SavedTokens int `json:"savedTokens"`
}

type TokenSavings struct {
	TotalRawTokens   int                       `json:"totalRawTokens"`
	TotalSavedTokens int                       `json:"totalSavedTokens"`
	ByCommand        map[string]CommandSavings `json:"byCommand"`
}

// SavingsPath returns the token-savings ledger path
// (~/.config/ask/savings.json).
func SavingsPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "ask", "savings.json"), nil
}

// LoadSavings reads the recorded token savings. A missing ledger is a
// zero-value result, not an error; malformed content is an error so a
// caller never mistakes corruption for zero gains.
func LoadSavings() (TokenSavings, error) {
	empty := TokenSavings{ByCommand: map[string]CommandSavings{}}
	path, err := SavingsPath()
	if err != nil {
		return empty, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return TokenSavings{}, err
	}
	var s TokenSavings
	if err := json.Unmarshal(b, &s); err != nil {
		return TokenSavings{}, err
	}
	if s.ByCommand == nil {
		s.ByCommand = map[string]CommandSavings{}
	}
	return s, nil
}

// RecordSavings increments the run count plus the raw and saved token
// counts for a base command under a file lock. rawTokens is the untouched
// output's estimate, savedTokens the reduction; the percentage saved is
// derived from the two. A zero saving is still recorded — a real command
// that compressed to nothing this run is a coverage data point, so the
// overlay reflects every modeled command, not only the ones that won.
// Callers gate out trivial/pager commands (see IsTrivialCommand) before
// recording, and clamp keeps a defensive negative from underflowing the
// totals.
func RecordSavings(baseCommand string, rawTokens, savedTokens int) error {
	if baseCommand == "" {
		return nil
	}
	if savedTokens < 0 {
		savedTokens = 0
	}
	if rawTokens < 0 {
		rawTokens = 0
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	askDir := filepath.Join(configDir, "ask")
	if err := os.MkdirAll(askDir, 0755); err != nil {
		return err
	}

	filePath := filepath.Join(askDir, "savings.json")

	f, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := lockFile(f); err != nil {
		return err
	}
	defer unlockFile(f)

	var savings TokenSavings
	savings.ByCommand = make(map[string]CommandSavings)

	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}

	if len(b) > 0 {
		if err := json.Unmarshal(b, &savings); err != nil {
			savings = TokenSavings{
				ByCommand: make(map[string]CommandSavings),
			}
		}
	}

	if savings.ByCommand == nil {
		savings.ByCommand = make(map[string]CommandSavings)
	}

	savings.TotalRawTokens += rawTokens
	savings.TotalSavedTokens += savedTokens
	cmdStat := savings.ByCommand[baseCommand]
	cmdStat.Count++
	cmdStat.RawTokens += rawTokens
	cmdStat.SavedTokens += savedTokens
	savings.ByCommand[baseCommand] = cmdStat

	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(&savings); err != nil {
		return err
	}

	return nil
}
