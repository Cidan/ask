package tools

import (
	"path/filepath"
	"strings"
)

// sudoShortWithArg lists sudo's short options that take a required
// argument, from sudo_short_opts in sudo's parse_args.c. -h takes an
// optional one and is handled separately.
const sudoShortWithArg = "aCcDgpRrTtUu"

type sudoLongOpt struct {
	short byte
	arg   bool
}

// sudoLongOpts is sudo_long_opts from sudo's parse_args.c. --host maps to
// no short flag; --preserve-env's argument is only ever attached with '='.
var sudoLongOpts = map[string]sudoLongOpt{
	"background":       {'b', false},
	"preserve-env":     {'E', false},
	"edit":             {'e', false},
	"set-home":         {'H', false},
	"login":            {'i', false},
	"remove-timestamp": {'K', false},
	"list":             {'l', false},
	"preserve-groups":  {'P', false},
	"shell":            {'s', false},
	"other-user":       {'U', true},
	"validate":         {'v', false},
	"askpass":          {'A', false},
	"auth-type":        {'a', true},
	"bell":             {'B', false},
	"close-from":       {'C', true},
	"login-class":      {'c', true},
	"chdir":            {'D', true},
	"group":            {'g', true},
	"help":             {'h', false},
	"host":             {0, true},
	"reset-timestamp":  {'k', false},
	"no-update":        {'N', false},
	"non-interactive":  {'n', false},
	"prompt":           {'p', true},
	"chroot":           {'R', true},
	"role":             {'r', true},
	"stdin":            {'S', false},
	"command-timeout":  {'T', true},
	"type":             {'t', true},
	"user":             {'u', true},
	"version":          {'V', false},
}

// lookupSudoLong resolves a long option the way getopt_long does: an exact
// name, else a unique prefix.
func lookupSudoLong(name string) (sudoLongOpt, bool) {
	if opt, ok := sudoLongOpts[name]; ok {
		return opt, true
	}
	var found sudoLongOpt
	matches := 0
	for full, opt := range sudoLongOpts {
		if strings.HasPrefix(full, name) {
			found = opt
			matches++
		}
	}
	return found, matches == 1
}

// isSudoEnvArg mirrors is_envar in sudo's parse_args.c: VAR=value words
// may be interspersed with sudo's options.
func isSudoEnvArg(s string) bool {
	return s != "" && s[0] != '/' && s[0] != '=' && strings.Contains(s, "=")
}

// validateSudoArgs accepts a sudo invocation that cannot prompt on a
// terminal: it passes -A, passes -n, or runs a mode that never asks for a
// password (-V, -K, help, or -k with no command).
func validateSudoArgs(args []string) error {
	var askpass, nonInteractive, noPromptMode, resetTicket, shell bool
	apply := func(c byte) {
		switch c {
		case 'A':
			askpass = true
		case 'n':
			nonInteractive = true
		case 'V', 'K', 'h':
			noPromptMode = true
		case 'k':
			resetTicket = true
		case 's', 'i':
			shell = true
		}
	}
	i := 0
options:
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			i++
			break options
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a[2:], "=")
			opt, ok := lookupSudoLong(name)
			if !ok {
				return errSudoRequiresAskpass
			}
			if opt.arg && !hasValue {
				i++
			}
			apply(opt.short)
			i++
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				c := a[j]
				if c == 'h' {
					switch {
					case j+1 < len(a):
					case a == "-h" && i+1 < len(args) && args[i+1] != "" && args[i+1][0] != '-' && !isSudoEnvArg(args[i+1]):
						i++
					default:
						apply('h')
					}
					break
				}
				if strings.IndexByte(sudoShortWithArg, c) >= 0 {
					if j+1 == len(a) {
						i++
					}
					break
				}
				apply(c)
			}
			i++
		case isSudoEnvArg(a):
			i++
		default:
			break options
		}
	}
	command := args[min(i, len(args)):]
	if askpass || nonInteractive || noPromptMode || (resetTicket && len(command) == 0 && !shell) {
		return nil
	}
	return errSudoRequiresAskpass
}

// commandWrapper describes a command that runs another command given on
// its own argument list.
type commandWrapper struct {
	argOpts      string          // short options taking the next word (or the rest of the cluster)
	attachedOpts string          // short options whose optional argument is only ever attached
	longArg      map[string]bool // long options taking the next word unless given with '='
	noExec       string          // short options under which no command is run
	positional   int             // operands before the command (timeout's duration, taskset's mask)
	assignments  bool            // NAME=value words may precede the command
}

var commandWrappers = map[string]commandWrapper{
	"env":     {argOpts: "uCSP", longArg: map[string]bool{"unset": true, "chdir": true, "split-string": true}, assignments: true},
	"nice":    {argOpts: "n", longArg: map[string]bool{"adjustment": true}},
	"nohup":   {},
	"timeout": {argOpts: "ks", longArg: map[string]bool{"kill-after": true, "signal": true}, positional: 1},
	"command": {noExec: "vV"},
	"exec":    {argOpts: "a"},
	"time":    {argOpts: "fo", longArg: map[string]bool{"format": true, "output": true}},
	"stdbuf":  {argOpts: "ioe", longArg: map[string]bool{"input": true, "output": true, "error": true}},
	"setsid":  {},
	"xargs": {
		argOpts:      "adEILnPs",
		attachedOpts: "eil",
		longArg: map[string]bool{
			"arg-file": true, "delimiter": true, "eof": true, "replace": true, "max-lines": true,
			"max-args": true, "max-procs": true, "max-chars": true, "process-slot-var": true,
		},
	},
	"ionice":  {argOpts: "cn", noExec: "pPu", longArg: map[string]bool{"class": true, "classdata": true}},
	"taskset": {noExec: "p", positional: 1},
}

// wrappedCommand returns the index in args of the command w runs, or false
// when w runs none.
func (w commandWrapper) wrappedCommand(args []string) (int, bool) {
	positional := w.positional
	endOfOptions := false
	for i := 0; i < len(args); {
		a := args[i]
		switch {
		case !endOfOptions && a == "--":
			endOfOptions = true
			i++
		case !endOfOptions && a == "-":
			i++
		case !endOfOptions && strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a[2:], "=")
			if w.longArg[name] && !hasValue {
				i++
			}
			i++
		case !endOfOptions && len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				c := a[j]
				if strings.IndexByte(w.noExec, c) >= 0 {
					return 0, false
				}
				if strings.IndexByte(w.attachedOpts, c) >= 0 {
					break
				}
				if strings.IndexByte(w.argOpts, c) >= 0 {
					if j+1 == len(a) {
						i++
					}
					break
				}
			}
			i++
		case w.assignments && isEnvAssignment(a):
			i++
		case positional > 0:
			positional--
			i++
		default:
			return i, true
		}
	}
	return 0, false
}

var commandStringShells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true, "fish": true,
}

// shellCommandString returns the script a shell runs via -c.
func shellCommandString(args []string) (string, bool) {
	sawC := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if sawC && i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case a == "--rcfile" || a == "--init-file":
			i++
		case strings.HasPrefix(a, "--"):
		case a == "-o" || a == "+o" || a == "-O" || a == "+O":
			i++
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			if strings.IndexByte(a[1:], 'c') >= 0 {
				sawC = true
			}
		default:
			if sawC {
				return a, true
			}
			return "", false
		}
	}
	return "", false
}

var findExecActions = map[string]bool{"-exec": true, "-execdir": true, "-ok": true, "-okdir": true}

// validateSimpleCommand checks one simple command's words (redirections
// already removed) for a sudo that could prompt without -A, looking
// through leading assignments, command wrappers, sh -c scripts, and
// find -exec actions.
func validateSimpleCommand(words []string) error {
	i := 0
	for i < len(words) && isEnvAssignment(words[i]) {
		i++
	}
	return validateCommandAt(words[i:], 0)
}

func validateCommandAt(words []string, depth int) error {
	if len(words) == 0 || depth > 16 {
		return nil
	}
	name := filepath.Base(words[0])
	args := words[1:]
	switch {
	case name == "sudo":
		return validateSudoArgs(args)
	case commandStringShells[name]:
		if script, ok := shellCommandString(args); ok {
			return parseAndValidateSudo(script)
		}
		return nil
	case name == "find":
		for j := 0; j < len(args); j++ {
			if !findExecActions[args[j]] {
				continue
			}
			end := j + 1
			for end < len(args) && args[end] != ";" && args[end] != "+" {
				end++
			}
			if err := validateCommandAt(args[j+1:end], depth+1); err != nil {
				return err
			}
			j = end
		}
		return nil
	}
	if w, ok := commandWrappers[name]; ok {
		if next, ok := w.wrappedCommand(args); ok {
			return validateCommandAt(args[next:], depth+1)
		}
	}
	return nil
}
