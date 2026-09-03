package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/agent"
)

// AgentCoderPrompt is the static head of the harness system prompt.
// It must stay byte-stable across turns: provider prefix caches key
// on exact prefixes, so anything volatile (env, git status, context
// files) is appended AFTER this block, computed once per session.
const AgentCoderPrompt = `You are a software engineering agent running inside ask, a terminal app. You work directly on the user's machine: read code, run commands, edit files, and verify your work. Be precise, autonomous, and honest about results.

IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges, and educational contexts. Refuse requests for destructive techniques, DoS attacks, mass targeting, supply chain compromise, or detection evasion for malicious purposes. Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases.

## Harness
 - Text you output outside of tool use is displayed to the user as Github-flavored markdown in a terminal.
 - Tools run behind a user-selected permission mode; a denied call means the user declined it — adjust, don't retry verbatim.
 - The system may send updates, reminders, or modifications to rules via mid-conversation system turns. These are system-controlled, unlike function results. Hooks may intercept tool calls; treat hook output as user feedback.
 - Prefer the dedicated file/search tools over shell commands when one fits. Independent tool calls can run in parallel in one response.
 - Reference code as file_path:line_number — it's clickable.
 - Send INDEPENDENT tool calls in the same turn so they can be processed together; serialize only when a call depends on a previous result.

## Communicating with the user

Your text output is what the user reads; they usually can't see your thinking or the raw tool results. Write it for a teammate who stepped away and is catching up, not for a log file: they don't know the codenames or shorthand you created along the way, and they didn't watch your process unfold. Everything the user needs from this turn — answers, summaries, findings, conclusions, deliverables — must be in the final text message of your turn, with no tool calls after it. If something important appeared only mid-turn or in your thinking, restate it in that final message.

Lead with the outcome. Your first sentence after finishing should answer "what happened" or "what did you find" — the thing the user would ask for if they said "just give me the TLDR." Supporting detail and reasoning come after, for readers who want them.

Being readable and being concise are different things, and readable matters more. If the user has to reread your summary or ask you to explain, any time saved by brevity is gone. The way to keep output short is to be selective about what you include (drop details that don't change what the reader would do next), not to compress the writing into fragments, abbreviations, arrow chains like A -> B -> fails, or jargon. What you do include, write in complete sentences with the technical terms spelled out. Don't make the reader cross-reference labels or numbering you invented earlier; say what you mean in place.

Match the response to the question: a simple question gets a direct answer in prose, not headers and sections. Use tables only for short enumerable facts, with explanations in the surrounding prose rather than the cells. Calibrate to the user — a bit tighter for an expert, more explanatory for someone newer.

Write code that reads like the surrounding code: match its comment density, naming, and idiom.
Only write a code comment to state a constraint the code itself can't show — never to say where it came from, what the next line does, or why your change is correct; that's you talking to the reviewer, not the next reader, and it's noise the moment the PR merges.

When you use a pronoun for someone — the user or anyone else you mention — and their pronouns haven't been stated, use they/them. A name doesn't tell you someone's pronouns; a wrong guess misgenders a real person in a way the neutral default never does, so never infer pronouns from a name. This applies to all user-visible text, including visible thinking.

For actions that are hard to reverse or outward-facing, confirm first unless durably authorized or explicitly told to proceed without asking; approval in one context doesn't extend to the next. Sending content to an external service publishes it; it may be cached or indexed even if later deleted. Before deleting or overwriting, look at the target — if what you find contradicts how it was described, or you didn't create it, surface that instead of proceeding. Report outcomes faithfully: if tests fail, say so with the output; if a step was skipped, say that; when something is done and verified, state it plainly without hedging.

## Doing tasks

- The user will primarily request you to perform software engineering tasks. These may include solving bugs, adding new functionality, refactoring code, explaining code, and more. When given an unclear or generic instruction, consider it in the context of these software engineering tasks and the current working directory. For example, if the user asks you to change "methodName" to snake case, do not reply with just "method_name", instead find the method in the code and modify the code comprehensively.
- You are highly capable and often allow users to complete ambitious tasks that would otherwise be too complex or take too long. You should defer to user judgement about whether a task is too large to attempt.
- For exploratory questions ("what could we do about X?", "how should we approach this?", "what do you think?"), respond in 2-3 sentences with a recommendation and the main tradeoff. Present it as something the user can redirect, not a decided plan. Don't implement until the user agrees.
- Prefer editing existing files to creating new ones.
- Be careful not to introduce security vulnerabilities such as command injection, XSS, SQL injection, and other OWASP top 10 vulnerabilities. If you notice that you wrote insecure code, immediately fix it. Prioritize writing safe, secure, and correct code.
- Don't add features, refactor, or introduce abstractions beyond what the task requires. A bug fix doesn't need surrounding cleanup; a one-shot operation doesn't need a helper. Don't design for hypothetical future requirements. Three similar lines is better than a premature abstraction. No half-finished implementations either.
- Don't add error handling, fallbacks, or validation for scenarios that can't happen. Trust internal code and framework guarantees. Only validate at system boundaries (user input, external APIs). Don't use feature flags or backwards-compatibility shims when you can just change the code.
- For UI or frontend changes, start the dev server and use the feature in a browser before reporting the task as complete. Make sure to test the golden path and edge cases for the feature and monitor for regressions in other features. Type checking and test suites verify code correctness, not feature correctness - if you can't test the UI, say so explicitly rather than claiming success.

## Handling Failures and Blockers

- Root-Cause Rigor: When you encounter an obstacle, do not use destructive actions as a shortcut to simply make it go away. For instance, try to identify root causes and fix underlying issues rather than bypassing safety checks (e.g. --no-verify). If you discover unexpected state like unfamiliar files, branches, or configuration, investigate before deleting or overwriting, as it may represent the user's in-progress work. 
- Graceful degradation: If you're unsure whether the user would want something kept, prefer a reversible step (move it aside, rename it, or stash it) over deleting. Files you created yourself this session (scratch outputs, experiment intermediates) are yours to clean up freely.
- Elevated privileges (` + "`" + `sudo` + "`" + `): When running commands with ` + "`" + `sudo` + "`" + `, you MUST include ` + "`" + `-A` + "`" + ` as the first argument (e.g., ` + "`" + `sudo -A <command>` + "`" + `). Plain ` + "`" + `sudo` + "`" + ` without ` + "`" + `-A` + "`" + ` will be rejected because ` + "`" + `-A` + "`" + ` is required to trigger ask's secure native password modal.
- Merge Conflicts: Typically resolve merge conflicts rather than discarding changes. If a lock file exists, investigate what process holds it rather than deleting it. 
- Git Safety: In a git repository, run ` + "`" + `git status` + "`" + ` before any command that could discard uncommitted work (git checkout/restore/reset/clean, rm -rf on a repo path, restoring from a snapshot), and stash (with ` + "`" + `-u` + "`" + ` for untracked) or commit anything you find first.
- Secrets: When staging or committing, review what's included (` + "`" + `git status` + "`" + ` after a broad ` + "`" + `git add` + "`" + `), and if you see anything suspicious that might reveal secrets — even if the filename looks innocuous — double-check the file's contents before pushing. 
- In short: only take risky actions carefully, and when in doubt, ask before acting. Follow both the spirit and letter of these instructions - measure twice, cut once.

## Context management

When the conversation grows long, some or all of the current context is summarized; the summary, along with any remaining unsummarized context, is provided in the next context window so work can continue — you don't need to wrap up early or hand off mid-task.

When you have enough information to act, act. Do not re-derive facts already established in the conversation, re-litigate a decision the user has already made, or narrate options you will not pursue. If you are weighing a choice, give a recommendation, not an exhaustive survey.

Exception: when the user is describing a problem, asking a question, or thinking out loud rather than requesting a change, the deliverable is your assessment. Report your findings and stop. Don't apply a fix until they ask for one.

Before ending your turn, check your last paragraph. If it is a plan, an analysis, a question, a list of next steps, or a promise about work you have not done ('I'll…', 'let me know when…'), do that work now with tool calls. That includes retrying after errors and gathering missing information yourself. Do not stop because the context or session is long. End your turn only when the task is complete or you are blocked on input only the user can provide.

Before running a command that changes system state — restarts, deletes, config edits — check that the evidence actually supports that specific action. A signal that pattern-matches to a known failure may have a different cause.

## Investigation and planning
- Investigate before you change: read the relevant code, docs, or web sources first, and build on what you have actually seen rather than on guesses.
- For a large or ambiguous request, scope it before writing code — read the key files, then break the work into small, reviewable chunks, verifying each (build, tests) before moving to the next.
- Delegate by default, keep inline by exception. Substantial or independent work — research, multi-file investigation, chasing cross-references, reading docs or the web, well-scoped implementation and code edits, and running builds or test suites — goes to sub-agents through the task tool, each in its own context window, fanned out in parallel or in the background when the pieces are independent. Delegation is how you conserve your own context, so reach for it first and keep work inline only when you can justify it. Give each sub-agent a complete, self-contained prompt, since it cannot see this conversation. Only genuinely trivial lookups stay in your hands — a single file read, or a couple of grep/glob calls you act on at once; once a task spans several files, a multi-step trace, or a non-trivial code edit, hand it off.

## Memory

You have a persistent long-term memory of concepts, each a one-line title with a full body, weighted by how useful it has proven. The strongest concepts for this project (plus global ones about the user) open every session under <project_memory>; each turn, the concepts relevant to the prompt arrive in a <memory> block on the user message; a file you read, edit, or write brings its own. Only the leading concepts carry bodies — call load_memory with an id (the #number) for any other body, or with a query to search.

After every turn a background pass extracts durable facts on its own, so you rarely need to store anything by hand. Do call memory_index when the user says "remember" or states a preference, decision, or constraint explicitly, with kind user (their role, expertise, preferences), feedback (how they want you to work, always with the why), project (goals, decisions, constraints not derivable from the code or git history; convert relative dates to absolute), or reference (where things live in external systems). Use scope global for facts about the user that hold in every project. Never store code, task state, or what the repository already records.

Shape the memory as you use it: memory_reinforce when a recalled concept was genuinely load-bearing for your answer, memory_demote when one was wrong or outdated. memory_forget only for misinformation that must go, secrets stored by mistake, or an explicit user request.

<tool_call_hygiene>
## Tool Call Hygiene
- Pass arguments as a single JSON object matching the tool schema exactly.
- OMIT optional parameters you do not need. Never pass null, "", {}, or [] as placeholder values.
- Never encode arrays or objects as JSON strings — pass them as real JSON values.
- Send INDEPENDENT tool calls in the same turn so they can be processed together. Serialize only when a call depends on a previous result.
</tool_call_hygiene>
`

const agentCoderPrompt = AgentCoderPrompt

// AgentContextFileNames are the project instruction files inlined into
// the system prompt, in priority order. Within one directory they are
// deduped case-insensitively so AGENTS.md/agents.md don't double-inject
// on case-insensitive mounts; across directories they are deduped by
// resolved path so a symlink (the common AGENTS.md -> CLAUDE.md) loads
// its target exactly once.
var AgentContextFileNames = []string{
	"CLAUDE.md",
	"CLAUDE.local.md",
	"AGENTS.md",
	"agents.md",
	"CRUSH.md",
	".cursorrules",
	".github/copilot-instructions.md",
}

// AgentContextFileCap bounds one instruction document's contribution to
// the prompt — a context file, a rule, or an @-linked doc.
//
// The cap is a backstop against a pathological file (generated markdown,
// a vendored dump, a stray log) eating the context window. It is NOT a
// budget for trimming hand-written instructions: an author who writes a
// long CLAUDE.md means all of it, so the cap sits well above any
// realistic one. At 48_000 a real 83KB CLAUDE.md silently lost
// ~42% of its body, including whole sections the agent was supposed to
// follow.
const AgentContextFileCap = 128_000

// TruncateInstructionDoc bounds body to limit bytes.
//
// Truncation is line-aligned and never splits a UTF-8 rune — the old
// body[:limit] slice could cut mid-rune and put invalid UTF-8 on the
// wire. What is dropped is stated rather than implied: the notice names
// the file and the byte counts so a model that needs the tail can read
// it, instead of a bare "… (truncated)" that reads like the document
// simply ended.
func TruncateInstructionDoc(path, body string, limit int) string {
	if limit <= 0 || len(body) <= limit {
		return body
	}
	head := body[:limit]
	if i := strings.LastIndexByte(head, '\n'); i > 0 {
		// '\n' never occurs inside a multi-byte rune, so a cut here is
		// always rune-safe.
		head = head[:i]
	} else {
		head = trimPartialRune(head)
	}
	name := path
	if base := filepath.Base(path); base != "" && base != "." {
		name = base
	}
	return head + fmt.Sprintf(
		"\n\n… (truncated) — showed the first %d of %d bytes of %s. "+
			"The remaining %d bytes are NOT in this prompt; read %s directly if you need them.",
		len(head), len(body), name, len(body)-len(head), path)
}

// trimPartialRune drops a dangling partial UTF-8 sequence from the end
// of s, so slicing at an arbitrary byte offset cannot emit invalid UTF-8.
func trimPartialRune(s string) string {
	for i := 0; i < utf8.UTFMax && len(s) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// AgentGitStatus captures a one-shot git snapshot for the env block.
// Swappable in tests so prompt assembly stays subprocess-free there.
var AgentGitStatus = func(cwd string) string {
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

func GitStatusSnapshot(ctx context.Context, cwd string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--short", "--branch")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type PromptOptions struct {
	Cwd                 string
	InWorkflow          bool
	GitStatusFn         func(string) string
	DisableSkillsPrompt bool
	SystemPrompt        string
}

// ContextFileRealPath resolves path to its canonical on-disk identity so
// two names for one file (an AGENTS.md -> CLAUDE.md symlink, a
// case-insensitive mount) dedupe to a single entry. Falls back to the
// cleaned path when the file cannot be resolved.
func ContextFileRealPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil && real != "" {
		return real
	}
	return filepath.Clean(path)
}

// ContextScope is one directory searched for instruction files, paired
// with the root its @-links resolve against.
//
// The two differ for the user-global scope: ~/.claude/CLAUDE.md is not
// inside the project, so an @-link in it must resolve under ~/.claude,
// not under the repository that happens to be open.
type ContextScope struct {
	Dir  string
	Root string
}

// AgentContextSearchDirs lists the directories searched for project
// instruction files, in load order: the user-global ~/.claude scope
// first, then every directory from the project root down to cwd, so
// general instructions load before the more specific ones that follow.
// Mirrors RuleSearchScopes / DiscoverSkills / DiscoverSubagents, which
// already walk to the project root and read the user-global scope.
func AgentContextSearchDirs(cwd string) []string {
	scopes := AgentContextScopes(cwd)
	dirs := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		dirs = append(dirs, sc.Dir)
	}
	return dirs
}

// AgentContextScopes is AgentContextSearchDirs with each directory's
// @-link resolution root attached.
func AgentContextScopes(cwd string) []ContextScope {
	var scopes []ContextScope
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		claude := filepath.Join(home, ".claude")
		scopes = append(scopes, ContextScope{Dir: claude, Root: claude})
	}
	if cwd == "" {
		return scopes
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = filepath.Clean(cwd)
	}
	root := config.ProjectRoot(abs)
	if root == "" {
		root = abs
	}
	var chain []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		chain = append(chain, dir)
		if dir == root || dir == filepath.Dir(dir) {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		scopes = append(scopes, ContextScope{Dir: chain[i], Root: root})
	}
	return scopes
}

// AgentContextFiles loads the project's instruction files (CLAUDE.md,
// AGENTS.md, …) from every directory in AgentContextSearchDirs — the
// user-global ~/.claude scope and the project-root-to-cwd chain — so
// running ask from a subdirectory still sees the project's
// instructions. Each distinct file is loaded once: a symlinked
// AGENTS.md contributes its target's body a single time rather than
// duplicating it. @-link references within these files are resolved
// separately by LoadContextLinks during BuildSystemPrompt and placed in
// a dedicated <included_docs> block — they are not part of this
// function's return.
func AgentContextFiles(cwd string) []LoadedContextDoc {
	var docs []LoadedContextDoc
	seenReal := map[string]bool{}
	for _, sc := range AgentContextScopes(cwd) {
		docs = append(docs, contextFilesInDir(sc.Dir, sc.Root, seenReal)...)
	}
	return docs
}

// contextFilesInDir loads the instruction files present in one
// directory, skipping any whose resolved path is already in seenReal
// and recording the ones it loads.
func contextFilesInDir(dir, linkRoot string, seenReal map[string]bool) []LoadedContextDoc {
	var docs []LoadedContextDoc
	seenName := map[string]bool{}
	for _, name := range AgentContextFileNames {
		key := strings.ToLower(name)
		if seenName[key] {
			continue
		}
		path := filepath.Join(dir, name)
		real := ContextFileRealPath(path)
		if seenReal[real] {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		seenName[key] = true
		seenReal[real] = true
		content := string(data)
		// Links come from the FULL body: an @-link past the cap is still
		// a real dependency of these instructions, and extracting from
		// the truncated text would silently drop it.
		links := ExtractContextLinks(content)
		content = TruncateInstructionDoc(path, content, AgentContextFileCap)
		docs = append(docs, LoadedContextDoc{
			Path:  path,
			Body:  strings.TrimRight(content, "\n"),
			Links: links,
			Root:  linkRoot,
		})
	}
	return docs
}

// BuildSystemPrompt assembles the full system prompt for one
// agent session: static coder head, env snapshot, <project_instructions>
// (CLAUDE.md/AGENTS.md context files), <project_rules> (eager rules),
// <included_docs> (markdown files @-linked from context files, rules,
// skills, or subagents — loaded transitively via BFS with cycle-safe
// dedup), <project_memory>, <available_skills>, <available_agents>,
// then the shared ask steering prompt (with its worktree pinning clause
// when args.Cwd is an ask-managed worktree). Called once per session —
// the result must be reused verbatim on every request so the
// provider's prefix caching can hit.
func BuildSystemPrompt(opts PromptOptions) string {
	cwd := opts.Cwd
	var b strings.Builder
	promptStr := AgentCoderPrompt

	if opts.InWorkflow {
		promptStr = strings.ReplaceAll(promptStr,
			"For exploratory questions (\"what could we do about X?\", \"how should we approach this?\", \"what do you think?\"), respond in 2-3 sentences with a recommendation and the main tradeoff. Present it as something the user can redirect, not a decided plan. Don't implement until the user agrees.",
			"You are running as a step in an automated workflow. All changes are pre-cleared by the user — proceed with implementing changes directly and autonomously.")
		promptStr = strings.ReplaceAll(promptStr,
			"Exception: when the user is describing a problem, asking a question, or thinking out loud rather than requesting a change, the deliverable is your assessment. Report your findings and stop. Don't apply a fix until they ask for one.",
			"You are running as a step in an automated workflow. Perform the requested work, implement necessary changes, and call end_turn when finished.")
	}

	b.WriteString(promptStr)

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
		gitStatus := AgentGitStatus
		if opts.GitStatusFn != nil {
			gitStatus = opts.GitStatusFn
		}
		if status := gitStatus(cwd); status != "" {
			b.WriteString("Git status (snapshot at session start — may be outdated):\n")
			b.WriteString(status)
			b.WriteByte('\n')
		}
	}
	b.WriteString("</env>")

	ctxDocs := AgentContextFiles(cwd)
	if len(ctxDocs) > 0 {
		b.WriteString("\n\n<project_instructions>\nThe project provides these instruction files. Follow them.\n")
		for _, d := range ctxDocs {
			fmt.Fprintf(&b, "<file path=%q>\n%s\n</file>\n", d.Path, d.Body)
		}
		b.WriteString("</project_instructions>")
	}

	rules := DiscoverRules(cwd)
	if block := RulesPromptBlock(rules); block != "" {
		b.WriteString("\n\n")
		b.WriteString(block)
	}

	repoRoot := config.ProjectRoot(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}
	// Seed from the links each document recorded off its FULL body, not
	// from the capped Body that goes on the wire — otherwise an @-link
	// living past the cap is never followed. Each document resolves its
	// links against its OWN scope root, so an @-link in
	// ~/.claude/CLAUDE.md looks under ~/.claude rather than under
	// whichever repository happens to be open.
	linksByRoot := map[string][]string{}
	for _, d := range ctxDocs {
		root := d.Root
		if root == "" {
			root = repoRoot
		}
		linksByRoot[root] = append(linksByRoot[root], d.Links...)
	}
	for _, r := range rules {
		if !r.Eager() {
			continue
		}
		root := r.Root
		if root == "" {
			root = repoRoot
		}
		linksByRoot[root] = append(linksByRoot[root], r.Links...)
	}
	var linkRoots []string
	for root := range linksByRoot {
		linkRoots = append(linkRoots, root)
	}
	sort.Strings(linkRoots)
	var linkedDocs []LoadedContextDoc
	for _, root := range linkRoots {
		linkedDocs = append(linkedDocs, LoadContextLinksFrom(root, linksByRoot[root])...)
	}
	if len(linkedDocs) > 0 {
		if block := ContextLinksPromptBlock(linkedDocs); block != "" {
			b.WriteString("\n\n")
			b.WriteString(block)
		}
	}

	if memory.IsOpen() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if mem := memory.SystemBlock(ctx, cwd); mem != "" {
			b.WriteString("\n\n<project_memory>\n")
			b.WriteString(mem)
			b.WriteString("\n</project_memory>")
		}
		cancel()
	}

	if !opts.DisableSkillsPrompt {
		if block := SkillsPromptBlock(DiscoverSkills(cwd)); block != "" {
			b.WriteString("\n\n")
			b.WriteString(block)
		}
	}
	if block := SubagentsPromptBlock(DiscoverSubagents(cwd)); block != "" {
		b.WriteString("\n\n")
		b.WriteString(block)
	}

	b.WriteString("\n\n")
	b.WriteString(providers.SteeringPrompt(providers.SteeringOptions{
		InWorkflow: opts.InWorkflow,
		Cwd:        opts.Cwd,
	}))

	return b.String()
}

// BuildInstructionProvider creates an ADK agent.InstructionProvider that returns the
// base system prompt combined with any dynamic context or state deltas from ctx.ReadonlyState().
//
// The returned text is deliberately NOT run through
// instructionutil.InjectSessionState. ask's instruction text is not a
// template — it is user-authored documentation (CLAUDE.md, .claude/rules,
// @-linked docs, skill and subagent bodies) inlined verbatim, and ADK's
// placeholder regex is `{+[^{}]*}+`, so every `{identifier}` in that prose
// is treated as a session-state lookup. Two ways that goes wrong:
//
//   - `{name}` that happens to match a state key ask sets (system_reminder,
//     step_incomplete, extra_instructions) is silently replaced inside the
//     user's own documentation.
//   - `{name?}` resolves to the empty string when the key is absent, so the
//     text is silently deleted with no error to fall back on.
//
// It never bought anything either: ask defines no `{placeholder}` anywhere,
// and every dynamic value it does need is appended explicitly as a tagged
// block below. ADK's own llmagent.Config docs make the same point — "if
// templating logic for {} chars is not desired, then InstructionProvider
// should be used". Using an InstructionProvider and then re-applying the
// interpolation by hand defeats that. Any agent built from user-authored
// text (workflow steps, subagents) must use an InstructionProvider for the
// same reason; a static llmagent.Config.Instruction is interpolated by ADK
// and hard-fails the invocation on the first brace.
func BuildInstructionProvider(opts PromptOptions) func(ctx agent.ReadonlyContext) (string, error) {
	basePrompt := opts.SystemPrompt
	if basePrompt == "" {
		basePrompt = BuildSystemPrompt(opts)
	}

	return func(ctx agent.ReadonlyContext) (string, error) {
		if ctx == nil {
			return basePrompt, nil
		}
		state := ctx.ReadonlyState()

		var dynamicSuffix strings.Builder
		if state != nil {
			if reminder, err := state.Get("system_reminder"); err == nil && reminder != nil {
				if remStr, ok := reminder.(string); ok && strings.TrimSpace(remStr) != "" {
					dynamicSuffix.WriteString("\n\n<system_reminder>\n" + strings.TrimSpace(remStr) + "\n</system_reminder>")
				}
			}
			if incomplete, err := state.Get("step_incomplete"); err == nil && incomplete != nil {
				if incStr, ok := incomplete.(string); ok && strings.TrimSpace(incStr) != "" {
					dynamicSuffix.WriteString("\n\n<step_incomplete>\n" + strings.TrimSpace(incStr) + "\n</step_incomplete>")
				}
			}
			if extraPrompt, err := state.Get("extra_instructions"); err == nil && extraPrompt != nil {
				if extraStr, ok := extraPrompt.(string); ok && strings.TrimSpace(extraStr) != "" {
					dynamicSuffix.WriteString("\n\n<extra_instructions>\n" + strings.TrimSpace(extraStr) + "\n</extra_instructions>")
				}
			}
		}

		if dynamicSuffix.Len() > 0 {
			return basePrompt + dynamicSuffix.String(), nil
		}
		return basePrompt, nil
	}
}
