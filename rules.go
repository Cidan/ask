package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"charm.land/fantasy"
)

// rules.go implements the Claude Code .claude/rules/ standard
// (docs.claude.com/en/docs/claude-code/memory → "Organize rules with
// .claude/rules/"). Markdown rule files live under .claude/rules/
// (discovered recursively) at the project and the user (~/.claude)
// scope. A rule may carry a YAML `paths` list in its frontmatter:
//
//   - No `paths` → EAGER. Loaded into the system prompt at session
//     start, same priority as .claude/CLAUDE.md.
//   - With `paths` → CONDITIONAL (JIT). The body is injected into the
//     tool result the moment the model READS a file whose project-root
//     relative path matches one of the globs — context exactly when
//     needed, never spent otherwise.
//
// User-scope rules load before project-scope so project rules win on a
// same-relative-path clash, matching the standard's precedence and the
// project-wins convention skills/subagents already use.

// askRule is one parsed .claude/rules/*.md file.
type askRule struct {
	// Path is the absolute path to the rule file (used as the stable
	// dedup key and shown in the injected header / prompt block).
	Path string
	// Rel is the rule file's label in prompts — its path relative to
	// the scope root, falling back to the base name.
	Rel string
	// Paths is the compiled glob list from `paths` frontmatter. Empty
	// means the rule is eager (unconditional).
	Paths []string
	// Body is the markdown instruction text (frontmatter stripped).
	Body string
}

// eager reports whether the rule loads unconditionally at session
// start (no `paths` scoping).
func (r askRule) eager() bool { return len(r.Paths) == 0 }

// matches reports whether rel (a project-root-relative, slash-
// separated path) is covered by any of the rule's globs. Always false
// for eager rules — they are not path-triggered.
func (r askRule) matches(rel string) bool {
	for _, pat := range r.Paths {
		if agentGlobMatch(pat, rel) {
			return true
		}
	}
	return false
}

// ruleScope pairs a discovery root with its rules directory. The root
// is what relative labels and (for the project scope) glob matching
// are measured against.
type ruleScope struct {
	root string // scope root (project root or user home)
	dir  string // the .claude/rules directory under root
}

// ruleSearchScopes returns the rule scopes in precedence order: user
// first (lower priority), then project (higher priority) so project
// rules override user rules on a same-relative-path clash.
func ruleSearchScopes(cwd string) []ruleScope {
	var scopes []ruleScope
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		scopes = append(scopes, ruleScope{root: home, dir: filepath.Join(home, ".claude", "rules")})
	}
	root := projectRoot(cwd)
	if root == "" {
		root = cwd
	}
	if root != "" {
		scopes = append(scopes, ruleScope{root: root, dir: filepath.Join(root, ".claude", "rules")})
	}
	return scopes
}

// ruleFileCap bounds one rule file's injected body, mirroring the
// context-file cap so a runaway rule can't dominate the prompt.
const ruleFileCap = 48_000

// discoverRules walks every scope's .claude/rules dir recursively for
// *.md files, parses each, and returns them keyed by scope-relative
// label so project rules supersede user rules with the same label.
// Symlinked dirs are followed (filepath.WalkDir resolves the entry
// type); a visited-real-path set breaks symlink cycles. Malformed
// files are skipped with a debug note, never fatal.
func discoverRules(cwd string) []askRule {
	byRel := map[string]askRule{}
	for _, scope := range ruleSearchScopes(cwd) {
		seen := map[string]bool{}
		walkRulesDir(scope, scope.dir, seen, byRel)
	}
	rels := make([]string, 0, len(byRel))
	for rel := range byRel {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	out := make([]askRule, 0, len(byRel))
	for _, rel := range rels {
		out = append(out, byRel[rel])
	}
	return out
}

// walkRulesDir recursively collects *.md rules under dir, following
// symlinks while guarding against cycles via the realpath set `seen`.
func walkRulesDir(scope ruleScope, dir string, seen map[string]bool, byRel map[string]askRule) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return // missing or broken link — nothing to load
	}
	if seen[real] {
		return // cycle / already visited
	}
	seen[real] = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		info, err := os.Stat(full) // Stat follows symlinks → dir-vs-file by target
		if err != nil {
			continue
		}
		if info.IsDir() {
			walkRulesDir(scope, full, seen, byRel)
			continue
		}
		if !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		rule, ok := parseRuleFile(full, scope)
		if !ok {
			continue
		}
		byRel[rule.Rel] = rule
	}
}

// parseRuleFile reads one rule file, strips and parses its optional
// frontmatter for a `paths` list, and returns the askRule. ok=false
// for an unreadable or empty-bodied file.
func parseRuleFile(path string, scope ruleScope) (askRule, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		debugLog("rule %s skipped: %v", path, err)
		return askRule{}, false
	}
	paths, body := parseRuleFrontmatter(string(data))
	if strings.TrimSpace(body) == "" {
		debugLog("rule %s skipped: empty body", path)
		return askRule{}, false
	}
	if len(body) > ruleFileCap {
		body = body[:ruleFileCap] + "\n… (truncated)"
	}
	rel := path
	if r, err := filepath.Rel(scope.dir, path); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	} else {
		rel = filepath.Base(path)
	}
	return askRule{
		Path:  path,
		Rel:   filepath.ToSlash(rel),
		Paths: paths,
		Body:  strings.TrimRight(body, "\n"),
	}, true
}

// parseRuleFrontmatter splits a rule file into its `paths` glob list
// and its body. It understands the YAML `paths` field in both the
// block-list form
//
//	paths:
//	  - "src/**/*.ts"
//	  - "lib/**/*.ts"
//
// and the inline-list form `paths: ["a", "b"]`. A file with no `---`
// frontmatter, or frontmatter without a `paths` field, yields a nil
// glob list (an eager rule) and the full text as the body. Brace
// patterns like {ts,tsx} stay intact — agentGlobMatch expands them at
// match time.
func parseRuleFrontmatter(s string) (paths []string, body string) {
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return nil, s
	}
	rest := s[strings.Index(s, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, s
	}
	fm := rest[:end]
	body = rest[end+len("\n---"):]
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	paths = parsePathsField(fm)
	return paths, body
}

// parsePathsField extracts the glob list from a rule's YAML
// frontmatter. It scans for a `paths:` key, accepts an inline list on
// the same line (`paths: [a, b]`), and otherwise consumes the
// following indented `- item` block lines.
func parsePathsField(fm string) []string {
	lines := strings.Split(fm, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		key, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(key) != "paths" {
			continue
		}
		// Inline form: paths: ["a", "b"] or paths: a
		if v := strings.TrimSpace(value); v != "" {
			return parseInlineList(v)
		}
		// Block form: subsequent `  - item` lines.
		var out []string
		for j := i + 1; j < len(lines); j++ {
			item := strings.TrimRight(lines[j], "\r")
			trimmed := strings.TrimSpace(item)
			if trimmed == "" {
				continue
			}
			if !strings.HasPrefix(trimmed, "-") {
				break // next key — list ended
			}
			glob := unquoteYAML(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")))
			if glob != "" {
				out = append(out, glob)
			}
		}
		return out
	}
	return nil
}

// parseInlineList parses `["a", "b"]`, `[a, b]`, or a bare scalar into
// a glob slice.
func parseInlineList(v string) []string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
		var out []string
		for _, part := range strings.Split(inner, ",") {
			if g := unquoteYAML(strings.TrimSpace(part)); g != "" {
				out = append(out, g)
			}
		}
		return out
	}
	if g := unquoteYAML(v); g != "" {
		return []string{g}
	}
	return nil
}

// rulesPromptBlock renders the eager (unconditional) rules into the
// <project_rules> system-prompt block. Path-scoped rules are omitted —
// they load JIT through ruleAwareTool. Returns "" when no eager rules
// exist so buildAgentSystemPrompt skips the block entirely.
func rulesPromptBlock(rules []askRule) string {
	var eager []askRule
	for _, r := range rules {
		if r.eager() {
			eager = append(eager, r)
		}
	}
	if len(eager) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<project_rules>\nThese project rules apply to all work in this session. Follow them.\n")
	for _, r := range eager {
		fmt.Fprintf(&b, "<rule path=%q>\n%s\n</rule>\n", r.Path, r.Body)
	}
	b.WriteString("</project_rules>")
	return b.String()
}

// contextAwareTool decorates the read, glob, grep, and ls tools.
// It injects context files (CLAUDE.md, etc.) by walking upwards from the target directory,
// and for the read tool, it also injects JIT path-scoped rules. Each injection happens
// at most once per session (tracked on `firedRules` and `seenCtx`).
type contextAwareTool struct {
	fantasy.AgentTool
	cwd        string
	root       string
	rules      []askRule
	mu         *sync.Mutex
	firedRules map[string]bool // rule.Path → already injected this session
	seenCtx    map[string]bool // dir path → already injected this session
}

// wrapContextAwareTools decorates the read, glob, grep, and ls tools with JIT rule
// injection and directory-aware context file injection.
func wrapContextAwareTools(tools []fantasy.AgentTool, cwd string, rules []askRule) []fantasy.AgentTool {
	var scoped []askRule
	for _, r := range rules {
		if !r.eager() {
			scoped = append(scoped, r)
		}
	}
	root := projectRoot(cwd)
	if root == "" {
		root = cwd
	}
	mu := &sync.Mutex{}
	firedRules := map[string]bool{}
	seenCtx := map[string]bool{}
	out := make([]fantasy.AgentTool, len(tools))
	for i, t := range tools {
		name := t.Info().Name
		if name == "read" || name == "glob" || name == "grep" || name == "ls" {
			out[i] = &contextAwareTool{
				AgentTool:  t,
				cwd:        cwd,
				root:       root,
				rules:      scoped,
				mu:         mu,
				firedRules: firedRules,
				seenCtx:    seenCtx,
			}
		} else {
			out[i] = t
		}
	}
	return out
}

func (ct *contextAwareTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := ct.AgentTool.Run(ctx, call)
	if err != nil || resp.IsError || resp.Type != "text" {
		return resp, err
	}

	var targetPath string
	name := ct.Info().Name
	if name == "read" {
		targetPath = fileToolPath(call.Input)
	} else {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(call.Input), &parsed); err == nil {
			if p, ok := parsed["path"].(string); ok && p != "" {
				targetPath = p
			}
		}
		if targetPath == "" {
			targetPath = ct.cwd
		}
	}

	if targetPath == "" {
		return resp, err
	}

	absPath := targetPath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(ct.cwd, absPath)
	}
	absPath = filepath.Clean(absPath)

	var ctxAdd []string

	// 1. Directory Upward Walk for context files
	dir := absPath
	info, errStat := os.Stat(absPath)
	if errStat == nil && !info.IsDir() {
		dir = filepath.Dir(absPath)
	} else if errStat != nil {
		dir = filepath.Dir(absPath)
	}

	for {
		ct.mu.Lock()
		seen := ct.seenCtx[dir]
		ct.seenCtx[dir] = true
		ct.mu.Unlock()

		var dirAdd []string
		if !seen {
			seenFile := map[string]bool{}
			for _, name := range agentContextFileNames {
				key := strings.ToLower(name)
				if seenFile[key] {
					continue
				}
				p := filepath.Join(dir, name)
				if data, err := os.ReadFile(p); err == nil {
					seenFile[key] = true
					content := string(data)
					if len(strings.TrimSpace(content)) == 0 {
						continue
					}
					if len(content) > agentContextFileCap {
						content = content[:agentContextFileCap] + "\n… (truncated)"
					}
					
					relP, err := filepath.Rel(ct.root, p)
					if err != nil || strings.HasPrefix(relP, "..") {
						relP = filepath.Base(p) // fallback
					} else {
						relP = filepath.ToSlash(relP)
					}
					
					dirAdd = append(dirAdd, fmt.Sprintf("## Project instructions from %s\n\n%s", relP, strings.TrimRight(content, "\n")))
				}
			}
		}

		// Prepend the current directory's instructions so that the innermost
		// instructions appear at the bottom (closest to the output).
		ctxAdd = append(dirAdd, ctxAdd...)

		if dir == ct.root || dir == filepath.Dir(dir) {
			break
		}
		dir = filepath.Dir(dir)
	}

	var add []string
	add = append(add, ctxAdd...)

	// 2. JIT Rules logic ONLY for 'read'
	if name == "read" {
		rel := ct.relPath(targetPath)
		if rel != "" {
			for _, r := range ct.rules {
				ct.mu.Lock()
				fired := ct.firedRules[r.Path]
				ct.mu.Unlock()
				if fired || !r.matches(rel) {
					continue
				}
				ct.mu.Lock()
				ct.firedRules[r.Path] = true
				ct.mu.Unlock()

				add = append(add, fmt.Sprintf("## Rule for %s (%s)\n\n%s", rel, r.Rel, r.Body))
				if linked := ruleLinkedDocs(ct.root, r.Body); len(linked) > 0 {
					for _, d := range linked {
						add = append(add, fmt.Sprintf("### Included from %s\n\n%s", d.Path, d.Body))
					}
				}
			}
		}
	}

	if len(add) > 0 {
		resp.Content = resp.Content + "\n\n" + strings.Join(add, "\n\n")
	}

	return resp, err
}

// relPath turns a tool's file_path argument into a clean, slash-
// separated path relative to the project root for glob matching.
// Returns "" when the path resolves outside the project root (rules
// can't scope files they don't cover).
func (ct *contextAwareTool) relPath(p string) string {
	if p == "" {
		return ""
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(ct.cwd, abs)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(ct.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}
