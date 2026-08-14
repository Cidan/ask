package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cidan/ask/pkg/config"
)

const (
	PlansDirName       = "ask/plans"
	StartPlanDirName   = "start"
	StartPlanDirInstruction = "ask/plans/start/ must be a DIRECTORY (not a file) and must contain at least one file. Create the directory, then write one or more files inside it — for example ask/plans/start/plan.md. Do not write a single file named start."
)

// PlansDir returns the absolute base plans directory for cwd.
func PlansDir(cwd string) string {
	if cwd == "" {
		return ""
	}
	root := config.ProjectRoot(cwd)
	if root == "" {
		root = cwd
	}
	return filepath.Join(root, filepath.FromSlash(PlansDirName))
}

// StartPlanDir returns the absolute path to ask/plans/start/.
func StartPlanDir(cwd string) string {
	base := PlansDir(cwd)
	if base == "" {
		return ""
	}
	return filepath.Join(base, StartPlanDirName)
}

// StepNotesDir returns the notes directory for a workflow step or loop iteration.
func StepNotesDir(cwd, stepName, loopName string, iteration int) string {
	base := PlansDir(cwd)
	if base == "" {
		return ""
	}
	if loopName != "" && iteration > 0 {
		return filepath.Join(base, SanitizeStepName(loopName), fmt.Sprintf("%d", iteration))
	}
	return filepath.Join(base, SanitizeStepName(stepName))
}

// IsPathUnderWorkflowPlans reports whether path is inside the ask/plans/ tree for cwd.
func IsPathUnderWorkflowPlans(cwd, path string) bool {
	plansDir := PlansDir(cwd)
	if plansDir == "" {
		return false
	}
	rel, err := filepath.Rel(plansDir, path)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// SanitizeStepName maps a workflow step name onto a filesystem-safe path component.
func SanitizeStepName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
			lastDash = r == '-'
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	stem := strings.Trim(b.String(), "-.")
	if stem == "" {
		stem = "step"
	}
	return stem
}

// EnsureStepNotesDir verifies that a notes directory exists.
func EnsureStepNotesDir(dir string) error {
	if dir == "" {
		return errors.New("notes directory path is empty")
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if mkerr := os.MkdirAll(dir, 0o755); mkerr != nil {
				return fmt.Errorf("cannot create notes directory %s: %w", dir, mkerr)
			}
			return nil
		}
		return fmt.Errorf("cannot read notes directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists but is a FILE, not a directory. Remove it, then create it as a directory and write your notes files inside it", dir)
	}
	return nil
}

// EnsureStartPlanExists verifies that ask/plans/start/ exists and contains at least one file.
func EnsureStartPlanExists(cwd string) error {
	dir := StartPlanDir(cwd)
	if dir == "" {
		return errors.New("start plan is missing: " + StartPlanDirInstruction)
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("start plan is missing: " + StartPlanDirInstruction)
		}
		return fmt.Errorf("cannot read start plan dir: %w", err)
	}
	if !info.IsDir() {
		return errors.New("ask/plans/start/ exists but is a FILE, not a directory. Remove it, " + StartPlanDirInstruction)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("cannot list start plan dir: %w", err)
	}
	hasFile := false
	for _, e := range entries {
		if !e.IsDir() {
			hasFile = true
			break
		}
	}
	if !hasFile {
		return errors.New("start plan is empty: " + StartPlanDirInstruction)
	}
	return nil
}

// ClearWorkflowPlans removes all files and subdirectories under ask/plans/.
func ClearWorkflowPlans(cwd string) error {
	dir := PlansDir(cwd)
	if dir == "" {
		return errors.New("no project root to locate ask/plans/")
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot read plans dir: %w", err)
	}
	if !info.IsDir() {
		return errors.New("ask/plans exists but is not a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("cannot list plans dir: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("cannot remove %s: %w", e.Name(), err)
		}
	}
	return nil
}

// RemoveAllWorkflowPlans removes the entire ask/plans/ tree.
func RemoveAllWorkflowPlans(cwd string) error {
	dir := PlansDir(cwd)
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cannot remove plans dir: %w", err)
	}
	return nil
}
