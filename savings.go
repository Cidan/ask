package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

type CommandSavings struct {
	Count       int `json:"count"`
	SavedTokens int `json:"savedTokens"`
}

type TokenSavings struct {
	TotalSavedTokens int                       `json:"totalSavedTokens"`
	ByCommand        map[string]CommandSavings `json:"byCommand"`
}

// RecordSavings safely increments the saved token count for the given base command.
// It uses OS-level file locking to ensure safe concurrent access from multiple processes.
func RecordSavings(baseCommand string, tokensSaved int) error {
	if tokensSaved <= 0 {
		return nil
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

	// Open the file for read and write, create if it doesn't exist.
	f, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Acquire an exclusive lock on the file descriptor
	if err := lockFile(f); err != nil {
		return err
	}
	// Release the lock when done
	defer unlockFile(f)

	var savings TokenSavings
	savings.ByCommand = make(map[string]CommandSavings)

	// Read existing content
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}

	if len(b) > 0 {
		if err := json.Unmarshal(b, &savings); err != nil {
			// If JSON is invalid or file is corrupted, we just start fresh
			savings = TokenSavings{
				ByCommand: make(map[string]CommandSavings),
			}
		}
	}

	if savings.ByCommand == nil {
		savings.ByCommand = make(map[string]CommandSavings)
	}

	savings.TotalSavedTokens += tokensSaved
	cmdStat := savings.ByCommand[baseCommand]
	cmdStat.Count++
	cmdStat.SavedTokens += tokensSaved
	savings.ByCommand[baseCommand] = cmdStat

	// Truncate and write updated content
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
