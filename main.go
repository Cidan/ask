package main

import (
	"fmt"
	"os"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

func initialModel(cfg askConfig) model {
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

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	renderer := newRenderer(100)

	vp := viewport.New()
	vp.Style = lipgloss.NewStyle().PaddingTop(1)
	vp.FillHeight = true
	vp.SoftWrap = true
	vp.MouseWheelEnabled = true

	m := model{
		mode:            modeInput,
		input:           ta,
		viewport:        vp,
		spinner:         sp,
		renderer:        renderer,
		width:           100,
		height:          30,
		claudeSlashCmds: cfg.Claude.SlashCommands,
	}
	m.refreshPrompt()
	return m
}

func main() {
	bridge, err := newMCPBridge()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ask: mcp:", err)
		os.Exit(1)
	}
	cfg, _ := loadConfig()
	_ = saveConfig(cfg)
	m := initialModel(cfg)
	m.mcpPort = bridge.port
	p := tea.NewProgram(m)
	bridge.start(p)
	final, err := p.Run()
	if m, ok := final.(model); ok {
		m.killProc()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ask:", err)
		os.Exit(1)
	}
}
