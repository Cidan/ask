package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// agentCoderPrompt is the static head of the harness system prompt.
// It must stay byte-stable across turns: DeepSeek's prefix cache keys
// on exact prefixes, so anything volatile (env, git status, context
// files) is appended AFTER this block, computed once per session.
const agentCoderPrompt = `You are a software engineering agent running inside ask, a terminal app. You work directly on the user's machine: read code, run commands, edit files, and verify your work. Be precise, autonomous, and honest about results.

<critical_rules>
1. Read before you write: always read a file with the read tool before editing or overwriting it. The edit tool enforces this.
2. Edits are exact-match: old_string must byte-match the file, including indentation and whitespace. Copy text from read output (strip the line-number prefix and the tab after it).
3. Verify your work: after code changes, build and run the relevant tests. Report failures honestly with their output — never claim something works without checking.
4. NEVER commit, push, or alter git state unless the user explicitly asks.
5. Do not add code comments unless the logic genuinely cannot be understood without one.
6. Reference code as file_path:line_number so the user can jump to it.
7. Prefer doing the work now over describing what could be done later. You operate at machine speed; "later" is a human concept.
8. If a tool result contradicts your assumptions, stop and re-read the relevant files instead of guessing.
</critical_rules>

<tool_usage>
- Use read/glob/grep/ls for file inspection — not bash cat/find/grep. The dedicated tools are faster, capped, and tracked (editing requires a prior read through the read tool).
- Use bash for builds, tests, git inspection, and anything that genuinely needs a shell. Each call runs in a fresh shell: no cd persistence, prefer absolute paths.
- Long-running commands (dev servers, watch modes): pass run_in_background, then poll with job_output and stop with job_kill. Never leave a foreground command hanging.
- Use todos to plan multi-step work, and call it again at EVERY step transition — mark the finished item completed and the next one in_progress the moment it happens, never batched at the end. The user watches this list live; a list that still shows step 1 while you are on step 4 is a wrong status display.
- Use fetch to read documentation URLs the project or user points at.
- Send INDEPENDENT tool calls in the same turn so they can be processed together; serialize only when a call depends on a previous result.
</tool_usage>

<tool_call_hygiene>
- Pass arguments as a single JSON object matching the tool schema exactly.
- OMIT optional parameters you do not need. Never pass null, "", {}, or [] as placeholder values.
- Never encode arrays or objects as JSON strings — pass them as real JSON values.
- Call each tool by its exact listed name; do not invent tools like apply_patch or apply_diff.
</tool_call_hygiene>

<editing>
When an edit fails with "old_string not found":
1. Re-read the file at the relevant range — it may have changed, or your copy may differ in whitespace.
2. Rebuild old_string from the fresh read output exactly, including blank lines and indentation.
3. If the snippet appears multiple times, extend it with surrounding lines until unique, or use replace_all when renaming a symbol everywhere.
Never retry the identical failing edit. For whole-file rewrites use write instead of one giant edit.
</editing>

<communication>
- Be concise. Lead with the outcome; details after.
- Plain prose for answers; markdown headings only when structure genuinely helps.
- Report what you did, what you verified, and anything still broken. No filler, no "I hope this helps".
</communication>`

// agentContextFileNames are the project instruction files inlined into
// the system prompt, in priority order. Deduped case-insensitively so
// AGENTS.md/agents.md don't double-inject on case-insensitive mounts.
var agentContextFileNames = []string{
	"CLAUDE.md",
	"CLAUDE.local.md",
	"AGENTS.md",
	"agents.md",
	"CRUSH.md",
	".cursorrules",
	".github/copilot-instructions.md",
}

// agentContextFileCap bounds one context file's contribution.
const agentContextFileCap = 48_000

// agentGitStatus captures a one-shot git snapshot for the env block.
// Swappable in tests so prompt assembly stays subprocess-free there.
var agentGitStatus = func(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain=v1", "--branch").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 40 {
		lines = append(lines[:40], fmt.Sprintf("… (%d more entries)", len(lines)-40))
	}
	return strings.Join(lines, "\n")
}

// buildAgentSystemPrompt assembles the full system prompt for one
// agent session: static coder head, env snapshot, project context
// files, then the shared ask steering prompt (with its worktree
// pinning clause when args.Cwd is an ask-managed worktree). Called
// once per session — the result must be reused verbatim on every
// request so DeepSeek's automatic prefix caching can hit.
func buildAgentSystemPrompt(args ProviderSessionArgs) string {
	cwd := args.Cwd
	var b strings.Builder
	b.WriteString(agentCoderPrompt)

	b.WriteString("\n\n<env>\n")
	fmt.Fprintf(&b, "Working directory: %s\n", cwd)
	isRepo := false
	if _, err := os.Stat(filepath.Join(cwd, ".git")); err == nil {
		isRepo = true
	}
	fmt.Fprintf(&b, "Is a git repository: %v\n", isRepo)
	fmt.Fprintf(&b, "Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Today's date: %s\n", time.Now().Format("2006-01-02"))
	if isRepo {
		if status := agentGitStatus(cwd); status != "" {
			b.WriteString("Git status (snapshot at session start — may be outdated):\n")
			b.WriteString(status)
			b.WriteByte('\n')
		}
	}
	b.WriteString("</env>")

	if ctxFiles := agentContextFiles(cwd); ctxFiles != "" {
		b.WriteString("\n\n<project_instructions>\nThe project provides these instruction files. Follow them.\n")
		b.WriteString(ctxFiles)
		b.WriteString("</project_instructions>")
	}

	if mem := agentMemorySystemBlock(cwd); mem != "" {
		b.WriteString("\n\n<project_memory>\n")
		b.WriteString(mem)
		b.WriteString("\n</project_memory>")
	}

	if block := skillsPromptBlock(discoverSkills(cwd)); block != "" {
		b.WriteString("\n\n")
		b.WriteString(block)
	}
	if block := subagentsPromptBlock(discoverSubagents(cwd)); block != "" {
		b.WriteString("\n\n")
		b.WriteString(block)
	}

	b.WriteString("\n\n")
	b.WriteString(steeringPromptFor(args))
	return b.String()
}

// agentContextFiles inlines the project's instruction files as
// <file path="..."> blocks.
func agentContextFiles(cwd string) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, name := range agentContextFileNames {
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		path := filepath.Join(cwd, name)
		data, err := os.ReadFile(path)
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		seen[key] = true
		content := string(data)
		if len(content) > agentContextFileCap {
			content = content[:agentContextFileCap] + "\n… (truncated)"
		}
		fmt.Fprintf(&b, "<file path=%q>\n%s\n</file>\n", path, strings.TrimRight(content, "\n"))
	}
	return b.String()
}
