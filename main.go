package main

import (
	"fmt"
	"os"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

const cursorBlinkSpeed = 650 * time.Millisecond

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

	vp := viewport.New()
	vp.Style = lipgloss.NewStyle().PaddingTop(1)
	vp.FillHeight = true
	vp.SoftWrap = true
	vp.MouseWheelEnabled = true

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
		viewport:           vp,
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
	if uc, err := readUsageCache(); err == nil {
		m.usageCache = uc
	}
	m.refreshPrompt()
	return m, nil
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "_hook" {
		if err := runHookSubcommand(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ask _hook:", err)
		}
		return
	}
	cfg, _ := loadConfig()
	_ = saveConfig(cfg)
	if cfg.UI.Worktree != nil && *cfg.UI.Worktree {
		ensureWorktreeGitignore()
	}
	pruneWorktrees()
	first, err := newTab(1, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ask: mcp:", err)
		os.Exit(1)
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
