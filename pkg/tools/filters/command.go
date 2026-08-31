package filters

import (
	"path/filepath"
	"regexp"
	"strings"
)

// MaxLines caps universal (post-filter) output middle-out. Semantic
// filters do the heavy compression; this is only the safety net for
// commands with no dedicated filter, so it stays generous — the point is
// to bound pathological output, not to drop signal.
const MaxLines = 1000

var ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// StripANSI removes CSI escape sequences (colors, cursor moves). It is the
// first thing every filter sees, so downstream matching is on plain text.
func StripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiEscapeRegex.ReplaceAllString(s, "")
}

// SplitLines splits on newlines without allocating a final empty element
// for a trailing newline — callers re-add the terminator via JoinLines.
func SplitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// JoinLines rejoins lines with newlines, restoring a trailing newline when
// the original output had one.
func JoinLines(lines []string, endedNL bool) string {
	out := strings.Join(lines, "\n")
	if out != "" && endedNL {
		out += "\n"
	}
	return out
}

// TrimTrailingSpace strips trailing spaces/tabs/CR from every line.
func TrimTrailingSpace(lines []string) []string {
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return lines
}

// SqueezeBlanks collapses runs of blank lines into a single blank line.
func SqueezeBlanks(lines []string) []string {
	out := lines[:0]
	prevBlank := false
	for _, l := range lines {
		blank := l == ""
		if blank && prevBlank {
			continue
		}
		out = append(out, l)
		prevBlank = blank
	}
	return out
}

// TrimBlankEdges drops leading and trailing blank lines.
func TrimBlankEdges(lines []string) []string {
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// DedupConsecutive collapses runs of identical lines into one line tagged
// with a `(×N)` multiplier. Only applied to the universal fallback —
// semantic-filter output carries meaningful repeats and is left alone.
func DedupConsecutive(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		run := j - i
		if run > 1 && lines[i] != "" {
			out = append(out, lines[i]+" (x"+itoa(run)+")")
		} else {
			for range run {
				out = append(out, lines[i])
			}
		}
		i = j
	}
	return out
}

// CapLines truncates middle-out to MaxLines, leaving a marker naming how
// many lines were dropped. Head and tail are kept because that is where a
// command's setup and verdict live.
func CapLines(lines []string) []string {
	if len(lines) <= MaxLines {
		return lines
	}
	half := MaxLines / 2
	out := make([]string, 0, MaxLines+1)
	out = append(out, lines[:half]...)
	out = append(out, "... "+itoa(len(lines)-MaxLines)+" lines truncated to save tokens ...")
	out = append(out, lines[len(lines)-half:]...)
	return out
}

// itoa is strconv.Itoa without the import churn for a hot, tiny path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// shellSplit tokenizes a command line into its top-level stages — the
// segments separated by pipes (| and ||), statement separators (; and &&),
// and newlines — each stage a list of words. It honors single and double
// quotes, backslash escapes, and $(...) / `...` substitutions, none of
// which split a stage or a word. This is deliberately not a full shell
// parser: it exists so command identification never splits on an operator
// that lives inside a quoted string, the source of garbage ledger keys
// like `")"`, `esac`, or `done'`.
func shellSplit(command string) [][]string {
	var (
		stages  [][]string
		stage   []string
		word    strings.Builder
		hasWord bool
	)
	flushWord := func() {
		if hasWord {
			stage = append(stage, word.String())
			word.Reset()
			hasWord = false
		}
	}
	flushStage := func() {
		flushWord()
		if len(stage) > 0 {
			stages = append(stages, stage)
			stage = nil
		}
	}

	r := []rune(command)
	n := len(r)
	for i := 0; i < n; {
		c := r[i]
		switch {
		case c == '\\':
			if i+1 < n {
				word.WriteRune(r[i+1])
				hasWord = true
				i += 2
			} else {
				i++
			}
		case c == '\'':
			i++
			for i < n && r[i] != '\'' {
				word.WriteRune(r[i])
				i++
			}
			if i < n {
				i++
			}
			hasWord = true
		case c == '"':
			i++
			for i < n && r[i] != '"' {
				if r[i] == '\\' && i+1 < n {
					word.WriteRune(r[i+1])
					i += 2
					continue
				}
				word.WriteRune(r[i])
				i++
			}
			if i < n {
				i++
			}
			hasWord = true
		case c == '`':
			word.WriteRune('`')
			i++
			for i < n && r[i] != '`' {
				word.WriteRune(r[i])
				i++
			}
			if i < n {
				word.WriteRune('`')
				i++
			}
			hasWord = true
		case c == '$' && i+1 < n && r[i+1] == '(':
			depth := 1
			word.WriteString("$(")
			i += 2
			for i < n && depth > 0 {
				switch r[i] {
				case '(':
					depth++
				case ')':
					depth--
				}
				word.WriteRune(r[i])
				i++
			}
			hasWord = true
		case c == '|':
			flushStage()
			i++
			if i < n && r[i] == '|' {
				i++
			}
		case c == ';':
			flushStage()
			i++
		case c == '\n':
			flushStage()
			i++
		case c == '&' && i+1 < n && r[i+1] == '&':
			flushStage()
			i += 2
		case c == ' ' || c == '\t' || c == '\r':
			flushWord()
			i++
		default:
			word.WriteRune(c)
			hasWord = true
			i++
		}
	}
	flushStage()
	return stages
}

// secondaryPrograms are pagers, text transformers, and trivial builtins:
// programs that consume or reshape another command's output, or produce
// none worth compressing. They are never chosen as a pipeline's primary
// command, and a command whose primary program is one of them is treated
// as trivial (IsTrivial) — not a savings opportunity worth recording.
var secondaryPrograms = map[string]bool{
	// pagers / text filters / transformers
	"head": true, "tail": true, "cat": true, "tac": true, "less": true,
	"more": true, "most": true, "grep": true, "egrep": true, "fgrep": true,
	"rg": true, "ag": true, "ack": true, "wc": true, "sort": true,
	"uniq": true, "cut": true, "tr": true, "tee": true, "sed": true,
	"awk": true, "gawk": true, "mawk": true, "jq": true, "yq": true,
	"column": true, "nl": true, "fold": true, "rev": true, "paste": true,
	"comm": true, "join": true, "xxd": true, "hexdump": true, "od": true,
	"strings": true, "base64": true, "expand": true, "unexpand": true,
	// navigation / no-op / tiny builtins / file plumbing
	"cd": true, "pushd": true, "popd": true, "ls": true, "echo": true,
	"printf": true, "pwd": true, "dirname": true, "basename": true,
	"realpath": true, "readlink": true, "true": true, "false": true,
	":": true, "test": true, "[": true, "touch": true, "mkdir": true,
	"rm": true, "cp": true, "mv": true, "ln": true, "chmod": true,
	"chown": true, "sleep": true, "export": true, "set": true,
	"unset": true, "which": true, "type": true, "find": true, "fd": true,
	"locate": true,
}

// leadingKeywords prefix a real command without changing which program does
// the work: shell keywords that introduce a command, and command wrappers
// whose own flags are boolean or attached.
var leadingKeywords = map[string]bool{
	"do": true, "then": true, "else": true, "elif": true,
	"if": true, "while": true, "until": true, "!": true,
	"time": true, "nohup": true, "exec": true,
	"command": true, "builtin": true,
}

// skipStageKeywords open or close a shell control block and are never a
// command themselves — a stage that starts with one contributes no program.
var skipStageKeywords = map[string]bool{
	"for": true, "case": true, "select": true, "in": true,
	"done": true, "fi": true, "esac": true, "end": true,
	"{": true, "}": true, ";;": true,
}

// sudoValueFlags / envValueFlags name the wrapper options that consume the
// following token as their argument, so the wrapped command is found.
var sudoValueFlags = map[string]bool{
	"-u": true, "--user": true, "-g": true, "--group": true,
	"-p": true, "--prompt": true, "-C": true, "--close-from": true,
	"-U": true, "-r": true, "--role": true, "-t": true, "--type": true,
}

var envValueFlags = map[string]bool{"-u": true, "--unset": true}

// gitValueFlags / goValueFlags name the global options that take the next
// token as a value, so the real subcommand is found past them
// (`git -C dir status`, `git -c k=v log`, `go -C dir test`).
var gitValueFlags = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true, "--super-prefix": true,
	"--config-env": true,
}

var goValueFlags = map[string]bool{"-C": true}

// subcommandTools are programs whose ledger key includes the subcommand
// (git → "git diff", go → "go test", kubectl → "kubectl get"), so the
// savings overlay can group by action.
var subcommandTools = map[string]bool{
	"go": true, "git": true, "npm": true, "yarn": true, "pnpm": true,
	"bun": true, "cargo": true, "pip": true, "uv": true, "docker": true,
	"kubectl": true, "helm": true, "terraform": true, "tofu": true,
	"bazel": true, "dotnet": true, "gradle": true, "poetry": true,
	"bundle": true,
}

// stripLeadingNoise removes tokens that prefix a real command without
// changing which program does the work: environment assignments, shell
// keywords that introduce a command (if/while/then/do/…), and command
// wrappers (sudo/env/time/…) together with their own flags.
func stripLeadingNoise(fields []string) []string {
	for len(fields) > 0 {
		f := fields[0]
		switch {
		case isEnvAssignment(f):
			fields = fields[1:]
		case leadingKeywords[f]:
			fields = fields[1:]
		case progOf(f) == "sudo":
			fields = stripWrapperFlags(fields[1:], sudoValueFlags)
		case progOf(f) == "env":
			fields = stripWrapperFlags(fields[1:], envValueFlags)
		default:
			return fields
		}
	}
	return fields
}

// stripWrapperFlags drops a wrapper's own leading flags and env
// assignments, consuming the argument of any flag named in valueFlags.
func stripWrapperFlags(fields []string, valueFlags map[string]bool) []string {
	for len(fields) > 0 {
		f := fields[0]
		switch {
		case isEnvAssignment(f):
			fields = fields[1:]
		case strings.HasPrefix(f, "-"):
			if valueFlags[f] && len(fields) >= 2 {
				fields = fields[2:]
			} else {
				fields = fields[1:]
			}
		default:
			return fields
		}
	}
	return fields
}

// subcommandAfter returns the first non-flag token after the program
// (fields[0]), skipping global options. valueFlags names options that
// consume the following token as their argument (git's -C/-c, go's -C); a
// `--opt=value` flag is self-contained.
func subcommandAfter(fields []string, valueFlags map[string]bool) (string, bool) {
	for i := 1; i < len(fields); {
		f := fields[i]
		if !strings.HasPrefix(f, "-") {
			return f, true
		}
		if strings.Contains(f, "=") {
			i++
			continue
		}
		if valueFlags[f] {
			i += 2
			continue
		}
		i++
	}
	return "", false
}

// BaseCommand identifies the primary command of a possibly-chained shell
// line and returns its fields (program plus arguments, with wrapper,
// keyword, and env-assignment noise stripped). It is the single source of
// truth for command identification: the filter registry dispatches on it
// and the savings ledger keys on it (via LedgerKey), so the two always
// agree.
//
// Selection across pipeline/statement stages: the last stage a registered
// filter claims wins (so `go test ./... | tail` is a go-test command, and
// `cmake .. && make` is a make command); failing that, the last stage
// whose program is not a pager/transformer (so `kubectl get pods | head`
// is a kubectl command); failing that, the last stage.
func BaseCommand(command string) []string {
	var candidates [][]string
	for _, st := range shellSplit(command) {
		fs := stripLeadingNoise(st)
		if len(fs) == 0 || skipStageKeywords[fs[0]] {
			continue
		}
		candidates = append(candidates, fs)
	}
	if len(candidates) == 0 {
		return nil
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		if match(candidates[i]) != nil {
			return candidates[i]
		}
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		if !secondaryPrograms[progOf(candidates[i][0])] {
			return candidates[i]
		}
	}
	return candidates[len(candidates)-1]
}

// LedgerKey is the savings-ledger key for a command: the primary program
// (directory prefix stripped), plus its subcommand for subcommand-style
// tools. Empty when there is no command.
func LedgerKey(command string) string {
	fields := BaseCommand(command)
	if len(fields) == 0 {
		return ""
	}
	prog := progOf(fields[0])
	if !subcommandTools[prog] {
		return prog
	}
	var valueFlags map[string]bool
	switch prog {
	case "git":
		valueFlags = gitValueFlags
	case "go":
		valueFlags = goValueFlags
	}
	if sub, ok := subcommandAfter(fields, valueFlags); ok {
		return prog + " " + sub
	}
	return prog
}

// IsTrivial reports whether a command is not a meaningful savings
// opportunity: its primary program is a pager, text transformer, or a tiny
// builtin (cat, grep, head, cd, echo, …), or there is no command at all.
// The ledger skips these so it reflects real build/test/tooling commands
// rather than file reads and shell plumbing.
func IsTrivial(command string) bool {
	fields := BaseCommand(command)
	if len(fields) == 0 {
		return true
	}
	return secondaryPrograms[progOf(fields[0])]
}

// isEnvAssignment reports whether f is a leading `NAME=value` shell
// assignment rather than the program itself.
func isEnvAssignment(f string) bool {
	idx := strings.IndexByte(f, '=')
	if idx <= 0 || strings.Contains(f[:idx], "/") {
		return false
	}
	for i, c := range f[:idx] {
		alpha := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i == 0 && !alpha {
			return false
		}
		if i > 0 && !alpha && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// progOf returns the executable name of a field with any directory prefix
// stripped (/usr/bin/git → git).
func progOf(field string) string { return filepath.Base(field) }
