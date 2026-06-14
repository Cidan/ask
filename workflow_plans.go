package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// workflow_plans.go manages the persistent artifact tree that workflow
// runs read from and write to. The runtime owns all paths; steps are
// told their own notes directory and the previous step's notes directory
// via prompt injection.

// workflowPlansDirName is the project-root-relative base directory for
// workflow plan artifacts.
const workflowPlansDirName = "ask/plans"

// workflowStartPlanDirName is the subdirectory that holds the starting
// plan. It is also the notes directory for the workflow's first step.
const workflowStartPlanDirName = "start"

// workflowPlansDir returns the absolute base plans directory for cwd.
// Empty when projectRoot cannot be determined.
func workflowPlansDir(cwd string) string {
	root := projectRoot(cwd)
	if root == "" {
		return ""
	}
	return filepath.Join(root, filepath.FromSlash(workflowPlansDirName))
}

// startPlanDir returns the absolute path to ask/plans/start/.
func startPlanDir(cwd string) string {
	base := workflowPlansDir(cwd)
	if base == "" {
		return ""
	}
	return filepath.Join(base, workflowStartPlanDirName)
}

// stepNotesDir returns the notes directory for a single dispatch.
//
//   - The first step of a workflow writes to ask/plans/start/.
//   - Subsequent linear steps write to ask/plans/<step-name>/.
//   - Steps inside a loop write to ask/plans/<loop-name>/<iteration>/.
func stepNotesDir(cwd, stepName, loopName string, iteration int) string {
	base := workflowPlansDir(cwd)
	if base == "" {
		return ""
	}
	if loopName != "" && iteration > 0 {
		return filepath.Join(base, sanitizeStepName(loopName), fmt.Sprintf("%d", iteration))
	}
	return filepath.Join(base, sanitizeStepName(stepName))
}

// isPathUnderWorkflowPlans reports whether path is inside the
// ask/plans/ tree for cwd's project root. It returns false when cwd has
// no project root or when path is the plans directory itself.
func isPathUnderWorkflowPlans(cwd, path string) bool {
	plansDir := workflowPlansDir(cwd)
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

// sanitizeStepName maps a workflow step name onto a filesystem-safe
// path component. Runes outside [a-zA-Z0-9._-] become '-'; runs
// collapse; empty results fall back to "step".
func sanitizeStepName(name string) string {
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

const startPlanDirInstruction = "ask/plans/start/ must be a DIRECTORY (not a file) and must contain at least one file. Create the directory, then write one or more files inside it — for example ask/plans/start/plan.md. Do not write a single file named start."

// ensureStepNotesDir verifies that a non-start step's notes directory
// exists as a directory. A missing directory is created automatically;
// an existing regular file is an error so the runner can kick the
// problem back to the LLM. The error text is LLM-facing.
func ensureStepNotesDir(dir string) error {
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

// ensureStartPlanExists verifies that ask/plans/start/ exists as a
// directory and contains at least one non-directory entry before the
// workflow begins. The error text is LLM-facing, so it repeats the
// exact shape the path must have.
func ensureStartPlanExists(cwd string) error {
	dir := startPlanDir(cwd)
	if dir == "" {
		return errors.New("start plan is missing: " + startPlanDirInstruction)
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("start plan is missing: " + startPlanDirInstruction)
		}
		return fmt.Errorf("cannot read start plan dir: %w", err)
	}
	if !info.IsDir() {
		return errors.New("ask/plans/start/ exists but is a FILE, not a directory. Remove it, " + startPlanDirInstruction)
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
		return errors.New("start plan is empty: " + startPlanDirInstruction)
	}
	return nil
}

// clearWorkflowPlans removes all children under ask/plans/ but leaves
// the directory itself. It is safe to call repeatedly.
func clearWorkflowPlans(cwd string) error {
	dir := workflowPlansDir(cwd)
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

// removeAllWorkflowPlans removes the entire ask/plans/ tree. Used by
// the runner after the final step succeeds.
func removeAllWorkflowPlans(cwd string) error {
	dir := workflowPlansDir(cwd)
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cannot remove plans dir: %w", err)
	}
	return nil
}

// ----- clear_plans tool -----

const clearPlansToolDescription = `Clear the workflow plans directory (ask/plans/).

This removes all files and subdirectories under ask/plans/ but leaves the directory itself. Call this before submitting a new workflow_run to ensure no stale plan data from a previous run interferes with the next workflow.

After clearing, create the directory ask/plans/start/ and write the starting plan into one or more files inside that directory (for example ask/plans/start/plan.md). ask/plans/start/ must be a directory, not a file.

This tool is safe to call repeatedly; it is a no-op when ask/plans/ does not exist.`

type clearPlansInput struct{}

type clearPlansOutput struct {
	Cleared bool `json:"cleared" jsonschema:"true on success"`
}

func clearPlansCore(cwd string, in clearPlansInput) (*mcp.CallToolResult, clearPlansOutput, error) {
	if errRes := requireWorkflowCwd(cwd); errRes != nil {
		return errRes, clearPlansOutput{}, nil
	}
	if err := clearWorkflowPlans(cwd); err != nil {
		return errResult(err.Error()), clearPlansOutput{}, nil
	}
	return okResult("ask/plans/ cleared"), clearPlansOutput{Cleared: true}, nil
}
