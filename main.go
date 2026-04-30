package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

const cursorBlinkSpeed = 650 * time.Millisecond

// usagePluginDir is the --plugin-dir value we pass to every claude
// subprocess, set once at startup by main() after extracting the
// embedded ask-usage plugin. Empty when extraction failed, in which
// case claudeCLIArgs omits --plugin-dir entirely and the chip just
// goes without 5h/wk segments.
var usagePluginDir string

func applyCursorBlink(ta *textarea.Model, enabled bool) {
	s := ta.Styles()
	s.Cursor.Blink = enabled
	s.Cursor.BlinkSpeed = cursorBlinkSpeed
	ta.SetStyles(s)
}

// applyInputTheme clears the textarea bubble's hardcoded CursorLine background
// (ansi 0 / 255) so the focused row inherits the theme's background instead of
// flashing a dark band across the input.
func applyInputTheme(ta *textarea.Model) {
	s := ta.Styles()
	s.Focused.CursorLine = lipgloss.NewStyle()
	s.Blurred.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(s)
}

func newTab(id int, cfg askConfig) (*model, error) {
	themeName := cfg.UI.Theme
	if themeName == "" {
		themeName = "default"
	}
	applyTheme(themeByName(themeName))

	ta := textarea.New()
	ta.Placeholder = "ask anything (try /resume)"
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.EndOfBufferCharacter = ' '
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.DynamicHeight = true
	ta.MinHeight = 3
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
	)
	ta.SetHeight(3)
	ta.Focus()

	cursorBlink := cfg.UI.CursorBlink == nil || *cfg.UI.CursorBlink
	applyCursorBlink(&ta, cursorBlink)
	applyInputTheme(&ta)

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	renderer := newRenderer(100)

	vp := newChatView()
	vp.style = lipgloss.NewStyle().PaddingTop(1)
	vp.mouseWheelEnabled = true

	cwd, _ := os.Getwd()

	provider := providerByID(cfg.Provider)
	if provider == nil {
		return nil, fmt.Errorf("no provider registered")
	}
	settings := provider.LoadSettings()

	// MCP bridge is started unconditionally so hot-swapping the
	// provider in-tab (Ctrl+B) doesn't have to spin up a new listener.
	// Providers that don't consume the bridge (codex) just ignore
	// mcpPort; the cost is a single idle loopback goroutine.
	bridge, err := newMCPBridge(id)
	if err != nil {
		return nil, err
	}
	mcpPort := bridge.port

	m := &model{
		id:                 id,
		cwd:                cwd,
		mcpBridge:          bridge,
		mcpPort:            mcpPort,
		provider:           provider,
		mode:               modeInput,
		input:              ta,
		chat:               vp,
		spinner:            sp,
		renderer:           renderer,
		width:              100,
		height:             30,
		providerSlashCmds:  settings.SlashCommands,
		providerModel:      settings.Model,
		providerEffort:     settings.Effort,
		ollamaHost:         cfg.Claude.Ollama.Host,
		ollamaModel:        cfg.Claude.Ollama.Model,
		themeName:          themeName,
		quietMode:          cfg.UI.QuietMode == nil || *cfg.UI.QuietMode,
		cursorBlink:        cursorBlink,
		renderDiffs:        cfg.UI.RenderDiffs == nil || *cfg.UI.RenderDiffs,
		toolOutputMode:     parseToolOutputMode(cfg.UI.ToolOutput),
		skipAllPermissions: cfg.UI.SkipAllPermissions != nil && *cfg.UI.SkipAllPermissions,
		worktree:           cfg.UI.Worktree != nil && *cfg.UI.Worktree,
		historyIdx:         -1,
		shellOutIdx:        -1,
		shellHistoryIdx:    -1,
		fc:                 &frameCache{},
	}
	// 80 cells gives a Neo4j error (e.g. "create database 'ask_tests':
	// connectivity: ...") room to wrap across a few lines instead of
	// being truncated with an ellipsis the user can't expand. The
	// toast still tail-truncates past defaultToastMaxHeight rows so a
	// runaway message can't take over the chat viewport.
	m.toast = NewToastModel(80, 3*time.Second)
	m.toast.applyTheme(activeTheme)
	if uc, err := readUsageCache(); err == nil {
		m.usageCache = uc
	}
	// Hook handlers tenant memmy ops on the per-tab cwd. Push the
	// initial cwd into the bridge here so SessionStart fires (which
	// happen before any user input could trigger another sync) get
	// the right project tuple.
	m.mcpBridge.setCwd(m.cwd)
	m.refreshPrompt()
	return m, nil
}

// printHelp writes the user-facing CLI usage block. Kept as a shared
// helper so `ask --help` (stdout, exit 0) and the `ask resume` arity
// error path (stderr, exit 2) print the exact same text.
func printHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  ask                start a new ask TUI session in the current directory
  ask resume <vid>   resume the virtual session with id <vid> — chdirs to
                     the workspace recorded for that session, then opens
                     the TUI already attached to it
  ask --help         show this help

Virtual session ids look like vs-<hex> and are listed by /resume inside
the TUI. Quitting ask prints the active tab's id so it can be passed to
`+"`"+`ask resume`+"`"+` later.
`)
}

// resumeLookup resolves vsID against ~/.config/ask/sessions.json and
// returns the matching VS id and the recorded workspace path. Pure: no
// side effects — main is responsible for the os.Chdir, which keeps
// tests self-contained (chdirs from a test process pollute every test
// that follows because the cleanup ordering against t.TempDir teardown
// is fragile when the cwd points inside a doomed tempdir).
func resumeLookup(vsID string) (id, workspace string, err error) {
	if vsID == "" {
		return "", "", fmt.Errorf("missing virtual session id")
	}
	store, err := loadVirtualSessions()
	if err != nil {
		return "", "", err
	}
	vs := store.findByID(vsID)
	if vs == nil {
		return "", "", fmt.Errorf("virtual session %q not found", vsID)
	}
	if vs.Workspace == "" {
		return "", "", fmt.Errorf("virtual session %q has no workspace recorded", vsID)
	}
	if _, err := os.Stat(vs.Workspace); err != nil {
		return "", "", fmt.Errorf("workspace %s: %w", vs.Workspace, err)
	}
	return vs.ID, vs.Workspace, nil
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "_hook" {
		if err := runHookSubcommand(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ask _hook:", err)
		}
		return
	}
	var startupResumeVID string
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--help", "-h", "help":
			printHelp(os.Stdout)
			return
		case "resume":
			if len(os.Args) < 3 {
				fmt.Fprintln(os.Stderr, "ask resume: missing virtual session id")
				fmt.Fprintln(os.Stderr)
				printHelp(os.Stderr)
				os.Exit(2)
			}
			vid, ws, err := resumeLookup(os.Args[2])
			if err != nil {
				fmt.Fprintln(os.Stderr, "ask resume:", err)
				os.Exit(1)
			}
			if err := os.Chdir(ws); err != nil {
				fmt.Fprintln(os.Stderr, "ask resume: chdir", ws+":", err)
				os.Exit(1)
			}
			startupResumeVID = vid
		}
	}
	cfg, _ := loadConfig()
	_ = saveConfig(cfg)
	if cfg.UI.Worktree != nil && *cfg.UI.Worktree {
		ensureWorktreeGitignore()
	}
	pruneWorktrees()
	// Memory is opt-in. When the user has it persisted as on, bring the
	// service up before any tab is constructed so consumers (which the
	// integration plan adds in later slices) see a ready singleton from
	// turn one. A failure here is non-fatal — the persisted flag stays
	// "on" so a subsequent /config toggle can retry — but we log it so
	// silent breakage is at least diagnosable via ASK_DEBUG=1.
	if memoryConfigEnabled(cfg) {
		if err := openMemoryService(cfg); err != nil {
			// errMemoryNoKey is the expected case when the user has
			// flipped Enabled but not yet pasted a key. Log it but
			// don't print a noisy stderr line — the picker already
			// surfaces "off (open failed)" so the user can paste.
			if !errors.Is(err, errMemoryNoKey) {
				fmt.Fprintln(os.Stderr, "ask: memory:", err)
			}
			debugLog("memory open at startup: %v", err)
		}
	}
	if dir, err := extractUsagePlugin(); err != nil {
		debugLog("usage plugin extract: %v", err)
	} else {
		usagePluginDir = dir
	}
	first, err := newTab(1, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ask: mcp:", err)
		os.Exit(1)
	}
	if startupResumeVID != "" {
		first.virtualSessionID = startupResumeVID
	}
	a := newApp(first)
	p := tea.NewProgram(a, tea.WithFPS(120))
	setTeaProgram(p)
	final, err := p.Run()
	if fa, ok := final.(app); ok {
		fa.shutdown()
	}
	pruneWorktrees()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ask:", err)
		os.Exit(1)
	}
}
