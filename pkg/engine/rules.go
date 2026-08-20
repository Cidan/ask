package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"
)

// Rule is one parsed .claude/rules/*.md file.
type Rule struct {
	// Path is the absolute path to the rule file.
	Path string
	// Rel is the rule file's label in prompts.
	Rel string
	// Paths is the compiled glob list from `paths` frontmatter. Empty means eager.
	Paths []string
	// Body is the markdown instruction text.
	Body string
}

func (r Rule) Eager() bool { return len(r.Paths) == 0 }

func (r Rule) Matches(rel string) bool {
	for _, pat := range r.Paths {
		if GlobMatch(pat, rel) {
			return true
		}
	}
	return false
}

type RuleScope struct {
	Root string
	Dir  string
}

func RuleSearchScopes(cwd string) []RuleScope {
	var scopes []RuleScope
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		scopes = append(scopes, RuleScope{Root: home, Dir: filepath.Join(home, ".claude", "rules")})
	}
	root := config.ProjectRoot(cwd)
	if root == "" {
		root = cwd
	}
	if root != "" {
		scopes = append(scopes, RuleScope{Root: root, Dir: filepath.Join(root, ".claude", "rules")})
	}
	return scopes
}

const ruleFileCap = 48_000

func DiscoverRules(cwd string) []Rule {
	byRel := map[string]Rule{}
	for _, scope := range RuleSearchScopes(cwd) {
		seen := map[string]bool{}
		walkRulesDir(scope, scope.Dir, seen, byRel)
	}
	rels := make([]string, 0, len(byRel))
	for rel := range byRel {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	out := make([]Rule, 0, len(byRel))
	for _, rel := range rels {
		out = append(out, byRel[rel])
	}
	return out
}

func walkRulesDir(scope RuleScope, dir string, seen map[string]bool, byRel map[string]Rule) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return
	}
	if seen[real] {
		return
	}
	seen[real] = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		info, err := os.Stat(full)
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
		rule, ok := ParseRuleFile(full, scope)
		if !ok {
			continue
		}
		byRel[rule.Rel] = rule
	}
}

func ParseRuleFile(path string, scope RuleScope) (Rule, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Rule{}, false
	}
	paths, body := ParseRuleFrontmatter(string(data))
	if strings.TrimSpace(body) == "" {
		return Rule{}, false
	}
	if len(body) > ruleFileCap {
		body = body[:ruleFileCap] + "\n… (truncated)"
	}
	rel := path
	if r, err := filepath.Rel(scope.Dir, path); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	} else {
		rel = filepath.Base(path)
	}
	return Rule{
		Path:  path,
		Rel:   filepath.ToSlash(rel),
		Paths: paths,
		Body:  strings.TrimRight(body, "\n"),
	}, true
}

func ParseRuleFrontmatter(s string) (paths []string, body string) {
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
	paths = ParsePathsField(fm)
	return paths, body
}

func ParsePathsField(fm string) []string {
	lines := strings.Split(fm, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		key, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(key) != "paths" {
			continue
		}
		if v := strings.TrimSpace(value); v != "" {
			return parseInlineList(v)
		}
		var out []string
		for j := i + 1; j < len(lines); j++ {
			item := strings.TrimRight(lines[j], "\r")
			trimmed := strings.TrimSpace(item)
			if trimmed == "" {
				continue
			}
			if !strings.HasPrefix(trimmed, "-") {
				break
			}
			glob := UnquoteYAML(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")))
			if glob != "" {
				out = append(out, glob)
			}
		}
		return out
	}
	return nil
}

func parseInlineList(v string) []string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
		var out []string
		for _, part := range strings.Split(inner, ",") {
			if g := UnquoteYAML(strings.TrimSpace(part)); g != "" {
				out = append(out, g)
			}
		}
		return out
	}
	if g := UnquoteYAML(v); g != "" {
		return []string{g}
	}
	return nil
}

func RulesPromptBlock(rules []Rule) string {
	var eager []Rule
	for _, r := range rules {
		if r.Eager() {
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

type ContextAwareTool struct {
	Inner      Tool
	cwd        string
	root       string
	rules      []Rule
	mu         *sync.Mutex
	firedRules map[string]bool
	seenCtx    map[string]bool
}

func (ct *ContextAwareTool) Name() string           { return ct.Inner.Name() }
func (ct *ContextAwareTool) Description() string    { return ct.Inner.Description() }
func (ct *ContextAwareTool) IsLongRunning() bool    { return ct.Inner.IsLongRunning() }
func (ct *ContextAwareTool) Info() ToolInfo         { return ExtractToolInfo(ct.Inner) }
func (ct *ContextAwareTool) Declaration() *genai.FunctionDeclaration {
	if dp, ok := ct.Inner.(interface{ Declaration() *genai.FunctionDeclaration }); ok {
		return dp.Declaration()
	}
	return nil
}

func WrapContextAwareTools(tools []Tool, cwd string, rules []Rule) []Tool {
	var scoped []Rule
	for _, r := range rules {
		if !r.Eager() {
			scoped = append(scoped, r)
		}
	}
	root := config.ProjectRoot(cwd)
	if root == "" {
		root = cwd
	}
	mu := &sync.Mutex{}
	firedRules := map[string]bool{}
	seenCtx := map[string]bool{}
	out := make([]Tool, len(tools))
	for i, t := range tools {
		name := t.Name()
		if name == "read" || name == "glob" || name == "grep" || name == "ls" {
			out[i] = &ContextAwareTool{
				Inner:      t,
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

func (ct *ContextAwareTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	resp, err := RunADKTool(ctx, ct.Inner, args)
	if err != nil {
		return resp, err
	}
	if isErr, _ := resp["is_error"].(bool); isErr {
		return resp, err
	}

	argsMap, _ := args.(map[string]any)
	if argsMap == nil {
		if raw, err := json.Marshal(args); err == nil {
			_ = json.Unmarshal(raw, &argsMap)
		}
	}

	var targetPath string
	name := ct.Inner.Name()
	if name == "read" {
		if p, ok := argsMap["file_path"].(string); ok {
			targetPath = p
		}
	} else {
		if p, ok := argsMap["path"].(string); ok && p != "" {
			targetPath = p
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
			for _, name := range AgentContextFileNames {
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
					if len(content) > AgentContextFileCap {
						content = content[:AgentContextFileCap] + "\n… (truncated)"
					}

					relP, err := filepath.Rel(ct.root, p)
					if err != nil || strings.HasPrefix(relP, "..") {
						relP = filepath.Base(p)
					} else {
						relP = filepath.ToSlash(relP)
					}

					dirAdd = append(dirAdd, fmt.Sprintf("## Project instructions from %s\n\n%s", relP, strings.TrimRight(content, "\n")))
				}
			}
		}

		ctxAdd = append(dirAdd, ctxAdd...)

		if dir == ct.root || dir == filepath.Dir(dir) {
			break
		}
		dir = filepath.Dir(dir)
	}

	var add []string
	add = append(add, ctxAdd...)

	if name == "read" {
		rel := ct.RelPath(targetPath)
		if rel != "" {
			for _, r := range ct.rules {
				ct.mu.Lock()
				fired := ct.firedRules[r.Path]
				ct.mu.Unlock()
				if fired || !r.Matches(rel) {
					continue
				}
				ct.mu.Lock()
				ct.firedRules[r.Path] = true
				ct.mu.Unlock()

				add = append(add, fmt.Sprintf("## Rule for %s (%s)\n\n%s", rel, r.Rel, r.Body))
				if linked := RuleLinkedDocs(ct.root, r.Body); len(linked) > 0 {
					for _, d := range linked {
						add = append(add, fmt.Sprintf("### Included from %s\n\n%s", d.Path, d.Body))
					}
				}
			}
		}
	}

	if len(add) > 0 {
		block := strings.Join(add, "\n\n")
		if resp == nil {
			resp = make(map[string]any)
		}
		if s, ok := resp["result"].(string); ok && s != "" {
			resp["result"] = s + "\n\n" + block
		} else {
			resp["result"] = block
		}
	}

	return resp, err
}

func (ct *ContextAwareTool) RelPath(p string) string {
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

func extractFilePath(input string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		return ""
	}
	if p, ok := m["file_path"].(string); ok {
		return p
	}
	if p, ok := m["path"].(string); ok {
		return p
	}
	return ""
}
