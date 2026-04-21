package main

import (
	"image/color"

	lipgloss "charm.land/lipgloss/v2"
)

type theme struct {
	name string

	accent      color.Color
	accentAlt   color.Color
	prompt      color.Color
	promptDot   color.Color
	success     color.Color
	errorFG     color.Color
	warn        color.Color
	dim         color.Color
	muted       color.Color
	inverseFG   color.Color
	darkFG      color.Color
	rowHL       color.Color
	scrollTrack color.Color
	tabActive   color.Color

	// background drives tea.View.BackgroundColor (OSC 11). Nil leaves the
	// terminal's own background untouched — used by the default theme.
	background color.Color
	// foreground drives tea.View.ForegroundColor (OSC 10). Set it whenever a
	// theme forces background against the grain of the terminal's default fg
	// (e.g. light-background themes where the terminal default is still light).
	foreground color.Color
}

var themeRegistry = []theme{
	defaultTheme(),
	draculaTheme(),
	nordTheme(),
	gruvboxDarkTheme(),
	tokyoNightTheme(),
	catppuccinLatteTheme(),
	catppuccinFrappeTheme(),
	catppuccinMacchiatoTheme(),
	catppuccinMochaTheme(),
	rosePineTheme(),
	fighterTheme(),
	hackerTheme(),
	amberTheme(),
	loveTheme(),
}

func defaultTheme() theme {
	return theme{
		name:        "default",
		accent:      lipgloss.Color("13"),
		accentAlt:   lipgloss.Color("14"),
		prompt:      lipgloss.Color("12"),
		promptDot:   lipgloss.Color("6"),
		success:     lipgloss.Color("10"),
		errorFG:     lipgloss.Color("9"),
		warn:        lipgloss.Color("11"),
		dim:         lipgloss.Color("8"),
		muted:       lipgloss.Color("7"),
		inverseFG:   lipgloss.Color("15"),
		darkFG:      lipgloss.Color("0"),
		rowHL:       lipgloss.Color("237"),
		scrollTrack: lipgloss.Color("0"),
		tabActive:   lipgloss.Color("5"),
		background:  nil,
	}
}

func draculaTheme() theme {
	return theme{
		name:        "dracula",
		accent:      lipgloss.Color("#FF79C6"),
		accentAlt:   lipgloss.Color("#8BE9FD"),
		prompt:      lipgloss.Color("#BD93F9"),
		promptDot:   lipgloss.Color("#8BE9FD"),
		success:     lipgloss.Color("#50FA7B"),
		errorFG:     lipgloss.Color("#FF5555"),
		warn:        lipgloss.Color("#F1FA8C"),
		dim:         lipgloss.Color("#6272A4"),
		muted:       lipgloss.Color("#BFBFBF"),
		inverseFG:   lipgloss.Color("#F8F8F2"),
		darkFG:      lipgloss.Color("#282A36"),
		rowHL:       lipgloss.Color("#44475A"),
		scrollTrack: lipgloss.Color("#282A36"),
		tabActive:   lipgloss.Color("#BD93F9"),
		background:  lipgloss.Color("#282A36"),
	}
}

func nordTheme() theme {
	return theme{
		name:        "nord",
		accent:      lipgloss.Color("#B48EAD"),
		accentAlt:   lipgloss.Color("#88C0D0"),
		prompt:      lipgloss.Color("#81A1C1"),
		promptDot:   lipgloss.Color("#8FBCBB"),
		success:     lipgloss.Color("#A3BE8C"),
		errorFG:     lipgloss.Color("#BF616A"),
		warn:        lipgloss.Color("#EBCB8B"),
		dim:         lipgloss.Color("#4C566A"),
		muted:       lipgloss.Color("#D8DEE9"),
		inverseFG:   lipgloss.Color("#ECEFF4"),
		darkFG:      lipgloss.Color("#2E3440"),
		rowHL:       lipgloss.Color("#3B4252"),
		scrollTrack: lipgloss.Color("#2E3440"),
		tabActive:   lipgloss.Color("#5E81AC"),
		background:  lipgloss.Color("#2E3440"),
	}
}

func gruvboxDarkTheme() theme {
	return theme{
		name:        "gruvbox",
		accent:      lipgloss.Color("#D3869B"),
		accentAlt:   lipgloss.Color("#83A598"),
		prompt:      lipgloss.Color("#83A598"),
		promptDot:   lipgloss.Color("#689D6A"),
		success:     lipgloss.Color("#B8BB26"),
		errorFG:     lipgloss.Color("#FB4934"),
		warn:        lipgloss.Color("#FABD2F"),
		dim:         lipgloss.Color("#928374"),
		muted:       lipgloss.Color("#BDAE93"),
		inverseFG:   lipgloss.Color("#FBF1C7"),
		darkFG:      lipgloss.Color("#282828"),
		rowHL:       lipgloss.Color("#3C3836"),
		scrollTrack: lipgloss.Color("#1D2021"),
		tabActive:   lipgloss.Color("#8F3F71"),
		background:  lipgloss.Color("#282828"),
	}
}

func tokyoNightTheme() theme {
	return theme{
		name:        "tokyo night",
		accent:      lipgloss.Color("#BB9AF7"),
		accentAlt:   lipgloss.Color("#7DCFFF"),
		prompt:      lipgloss.Color("#7AA2F7"),
		promptDot:   lipgloss.Color("#2AC3DE"),
		success:     lipgloss.Color("#9ECE6A"),
		errorFG:     lipgloss.Color("#F7768E"),
		warn:        lipgloss.Color("#E0AF68"),
		dim:         lipgloss.Color("#565F89"),
		muted:       lipgloss.Color("#A9B1D6"),
		inverseFG:   lipgloss.Color("#C0CAF5"),
		darkFG:      lipgloss.Color("#1A1B26"),
		rowHL:       lipgloss.Color("#292E42"),
		scrollTrack: lipgloss.Color("#1A1B26"),
		tabActive:   lipgloss.Color("#7AA2F7"),
		background:  lipgloss.Color("#1A1B26"),
	}
}

// Catppuccin: four official flavors with a consistent role mapping.
// Pink→accent, Sky→accentAlt, Blue→prompt, Teal→promptDot, Green→success,
// Red→errorFG, Yellow→warn, Overlay0→dim, Subtext1→muted, Mauve→tabActive,
// Surface0→rowHL, Base→background+scrollTrack. inverseFG/darkFG flip between
// the three dark flavors (Text on colored bg, Base on warn) and Latte (Base on
// colored bg, Text on warn) so contrast stays legible in both directions.

func catppuccinLatteTheme() theme {
	return theme{
		name:        "latte",
		accent:      lipgloss.Color("#EA76CB"),
		accentAlt:   lipgloss.Color("#04A5E5"),
		prompt:      lipgloss.Color("#1E66F5"),
		promptDot:   lipgloss.Color("#179299"),
		success:     lipgloss.Color("#40A02B"),
		errorFG:     lipgloss.Color("#D20F39"),
		warn:        lipgloss.Color("#DF8E1D"),
		dim:         lipgloss.Color("#9CA0B0"),
		muted:       lipgloss.Color("#5C5F77"),
		inverseFG:   lipgloss.Color("#EFF1F5"),
		darkFG:      lipgloss.Color("#4C4F69"),
		rowHL:       lipgloss.Color("#CCD0DA"),
		scrollTrack: lipgloss.Color("#E6E9EF"),
		tabActive:   lipgloss.Color("#8839EF"),
		background:  lipgloss.Color("#EFF1F5"),
		foreground:  lipgloss.Color("#4C4F69"),
	}
}

func catppuccinFrappeTheme() theme {
	return theme{
		name:        "frappé",
		accent:      lipgloss.Color("#F4B8E4"),
		accentAlt:   lipgloss.Color("#99D1DB"),
		prompt:      lipgloss.Color("#8CAAEE"),
		promptDot:   lipgloss.Color("#81C8BE"),
		success:     lipgloss.Color("#A6D189"),
		errorFG:     lipgloss.Color("#E78284"),
		warn:        lipgloss.Color("#E5C890"),
		dim:         lipgloss.Color("#737994"),
		muted:       lipgloss.Color("#B5BFE2"),
		inverseFG:   lipgloss.Color("#C6D0F5"),
		darkFG:      lipgloss.Color("#303446"),
		rowHL:       lipgloss.Color("#414559"),
		scrollTrack: lipgloss.Color("#303446"),
		tabActive:   lipgloss.Color("#CA9EE6"),
		background:  lipgloss.Color("#303446"),
	}
}

func catppuccinMacchiatoTheme() theme {
	return theme{
		name:        "macchiato",
		accent:      lipgloss.Color("#F5BDE6"),
		accentAlt:   lipgloss.Color("#91D7E3"),
		prompt:      lipgloss.Color("#8AADF4"),
		promptDot:   lipgloss.Color("#8BD5CA"),
		success:     lipgloss.Color("#A6DA95"),
		errorFG:     lipgloss.Color("#ED8796"),
		warn:        lipgloss.Color("#EED49F"),
		dim:         lipgloss.Color("#6E738D"),
		muted:       lipgloss.Color("#B8C0E0"),
		inverseFG:   lipgloss.Color("#CAD3F5"),
		darkFG:      lipgloss.Color("#24273A"),
		rowHL:       lipgloss.Color("#363A4F"),
		scrollTrack: lipgloss.Color("#24273A"),
		tabActive:   lipgloss.Color("#C6A0F6"),
		background:  lipgloss.Color("#24273A"),
	}
}

func catppuccinMochaTheme() theme {
	return theme{
		name:        "mocha",
		accent:      lipgloss.Color("#F5C2E7"),
		accentAlt:   lipgloss.Color("#89DCEB"),
		prompt:      lipgloss.Color("#89B4FA"),
		promptDot:   lipgloss.Color("#94E2D5"),
		success:     lipgloss.Color("#A6E3A1"),
		errorFG:     lipgloss.Color("#F38BA8"),
		warn:        lipgloss.Color("#F9E2AF"),
		dim:         lipgloss.Color("#6C7086"),
		muted:       lipgloss.Color("#BAC2DE"),
		inverseFG:   lipgloss.Color("#CDD6F4"),
		darkFG:      lipgloss.Color("#1E1E2E"),
		rowHL:       lipgloss.Color("#313244"),
		scrollTrack: lipgloss.Color("#1E1E2E"),
		tabActive:   lipgloss.Color("#CBA6F7"),
		background:  lipgloss.Color("#1E1E2E"),
	}
}

func rosePineTheme() theme {
	return theme{
		name:        "rose pine",
		accent:      lipgloss.Color("#EBBCBA"),
		accentAlt:   lipgloss.Color("#9CCFD8"),
		prompt:      lipgloss.Color("#31748F"),
		promptDot:   lipgloss.Color("#F6C177"),
		success:     lipgloss.Color("#9CCFD8"),
		errorFG:     lipgloss.Color("#EB6F92"),
		warn:        lipgloss.Color("#F6C177"),
		dim:         lipgloss.Color("#6E6A86"),
		muted:       lipgloss.Color("#908CAA"),
		inverseFG:   lipgloss.Color("#E0DEF4"),
		darkFG:      lipgloss.Color("#191724"),
		rowHL:       lipgloss.Color("#26233A"),
		scrollTrack: lipgloss.Color("#1F1D2E"),
		tabActive:   lipgloss.Color("#C4A7E7"),
		background:  lipgloss.Color("#191724"),
	}
}

// fighterTheme uses the Monokai Pro (Octagon) palette — softer than classic
// Monokai so borders and accents read as muted pastels rather than neon. The
// name nods to the Octagon filter (MMA cage).
func fighterTheme() theme {
	return theme{
		name:        "fighter",
		accent:      lipgloss.Color("#AB9DF2"),
		accentAlt:   lipgloss.Color("#78DCE8"),
		prompt:      lipgloss.Color("#AB9DF2"),
		promptDot:   lipgloss.Color("#A9DC76"),
		success:     lipgloss.Color("#A9DC76"),
		errorFG:     lipgloss.Color("#FF6188"),
		warn:        lipgloss.Color("#FFD866"),
		dim:         lipgloss.Color("#727072"),
		muted:       lipgloss.Color("#C1C0C0"),
		inverseFG:   lipgloss.Color("#FCFCFA"),
		darkFG:      lipgloss.Color("#2D2A2E"),
		rowHL:       lipgloss.Color("#403E41"),
		scrollTrack: lipgloss.Color("#2D2A2E"),
		tabActive:   lipgloss.Color("#AB9DF2"),
		background:  lipgloss.Color("#2D2A2E"),
	}
}

// hackerTheme is the Matrix: phosphor green on CRT black, with amber for
// warnings (that 1970s terminal tint) and a crimson red reserved for "access
// denied" errors. Everything else lives on the green ramp.
func hackerTheme() theme {
	return theme{
		name:        "hacker",
		accent:      lipgloss.Color("#3FDF5A"),
		accentAlt:   lipgloss.Color("#5DE880"),
		prompt:      lipgloss.Color("#3FDF5A"),
		promptDot:   lipgloss.Color("#1E8F35"),
		success:     lipgloss.Color("#3FDF5A"),
		errorFG:     lipgloss.Color("#E03048"),
		warn:        lipgloss.Color("#E0A500"),
		dim:         lipgloss.Color("#0E4A1A"),
		muted:       lipgloss.Color("#25A838"),
		inverseFG:   lipgloss.Color("#050805"),
		darkFG:      lipgloss.Color("#000000"),
		rowHL:       lipgloss.Color("#0A2510"),
		scrollTrack: lipgloss.Color("#050805"),
		tabActive:   lipgloss.Color("#5DE880"),
		background:  lipgloss.Color("#050805"),
		foreground:  lipgloss.Color("#3FDF5A"),
	}
}

// amberTheme is hacker's sibling: amber CRT phosphor on a warm near-black,
// the look of a 1970s DEC/IBM amber-screen terminal. Red is reserved for
// errors; everything else lives on the amber ramp.
func amberTheme() theme {
	return theme{
		name:        "amber",
		accent:      lipgloss.Color("#E0A44A"),
		accentAlt:   lipgloss.Color("#FFC857"),
		prompt:      lipgloss.Color("#E0A44A"),
		promptDot:   lipgloss.Color("#B37F2C"),
		success:     lipgloss.Color("#D4C04B"),
		errorFG:     lipgloss.Color("#D14545"),
		warn:        lipgloss.Color("#FFC857"),
		dim:         lipgloss.Color("#5A3E14"),
		muted:       lipgloss.Color("#B37F2C"),
		inverseFG:   lipgloss.Color("#0F0A00"),
		darkFG:      lipgloss.Color("#000000"),
		rowHL:       lipgloss.Color("#291B00"),
		scrollTrack: lipgloss.Color("#0F0A00"),
		tabActive:   lipgloss.Color("#FFC857"),
		background:  lipgloss.Color("#0F0A00"),
		foreground:  lipgloss.Color("#E0A44A"),
	}
}

// loveTheme mirrors Charm crush's default palette (charmtone). The accent and
// prompt are Charple, secondary accents lean on Bok/Dolly, and the base is
// Pepper/Charcoal so a side-by-side with crush feels at home.
func loveTheme() theme {
	return theme{
		name:        "love",
		accent:      lipgloss.Color("#8878FA"),
		accentAlt:   lipgloss.Color("#68FFD6"),
		prompt:      lipgloss.Color("#8878FA"),
		promptDot:   lipgloss.Color("#68FFD6"),
		success:     lipgloss.Color("#00FFB2"),
		errorFG:     lipgloss.Color("#FF577D"),
		warn:        lipgloss.Color("#F5EF34"),
		dim:         lipgloss.Color("#605F6B"),
		muted:       lipgloss.Color("#BFBCC8"),
		inverseFG:   lipgloss.Color("#FFFAF1"),
		darkFG:      lipgloss.Color("#201F26"),
		rowHL:       lipgloss.Color("#3A3943"),
		scrollTrack: lipgloss.Color("#201F26"),
		tabActive:   lipgloss.Color("#FF60FF"),
		background:  lipgloss.Color("#201F26"),
	}
}

func themeByName(name string) theme {
	for _, t := range themeRegistry {
		if t.name == name {
			return t
		}
	}
	return defaultTheme()
}

// All themable style vars live here. applyTheme() populates them; every other
// file references them read-only.
var (
	selectedStyle    lipgloss.Style
	dimStyle         lipgloss.Style
	promptStyle      lipgloss.Style
	promptArrowStyle lipgloss.Style
	promptDotStyle   lipgloss.Style
	cwdStyle         lipgloss.Style
	errStyle         lipgloss.Style
	userBarStyle     lipgloss.Style
	outputStyle      lipgloss.Style
	thinkingStyle    lipgloss.Style
	chipStyle        lipgloss.Style
	scrollThumbStyle lipgloss.Style
	scrollTrackStyle lipgloss.Style
	thumbBorderStyle lipgloss.Style
	pathBoxStyle     lipgloss.Style

	todoBoxStyle       lipgloss.Style
	todoPendingStyle   lipgloss.Style
	todoProgressStyle  lipgloss.Style
	todoCompletedStyle lipgloss.Style

	diffPathStyle       lipgloss.Style
	diffHunkHeaderStyle lipgloss.Style
	diffAddStyle        lipgloss.Style
	diffDelStyle        lipgloss.Style
	diffContextStyle    lipgloss.Style

	askBoxStyle              lipgloss.Style
	askTabStyle              lipgloss.Style
	askTabActiveStyle        lipgloss.Style
	askPromptStyle           lipgloss.Style
	askOptionSelected        lipgloss.Style
	askOptionCursorFG        lipgloss.Style
	askOptionRowStyle        lipgloss.Style
	askHelpStyle             lipgloss.Style
	askConfirmKeyStyle       lipgloss.Style
	askSummaryDimStyle       lipgloss.Style
	askNoteLabelStyle        lipgloss.Style
	askPlaceholder           lipgloss.Style
	askCaretStyle            lipgloss.Style
	askConfirmBoxStyle       lipgloss.Style
	askConfirmBtnStyle       lipgloss.Style
	askConfirmBtnActiveStyle lipgloss.Style

	approvalBoxStyle          lipgloss.Style
	approvalBtnStyle          lipgloss.Style
	approvalDenyActiveStyle   lipgloss.Style
	approvalAllowActiveStyle  lipgloss.Style
	approvalAlwaysActiveStyle lipgloss.Style
	approvalTitleStyle        lipgloss.Style
	approvalToolStyle         lipgloss.Style
	approvalSummaryStyle      lipgloss.Style

	configBoxStyle         lipgloss.Style
	configTitleStyle       lipgloss.Style
	configPromptStyle      lipgloss.Style
	configPlaceholderStyle lipgloss.Style
	configCaretStyle       lipgloss.Style
	configSelectedRowStyle lipgloss.Style
	configKeyDimStyle      lipgloss.Style
	configHelpStyle        lipgloss.Style

	ollamaBoxStyle    lipgloss.Style
	ollamaTitleStyle  lipgloss.Style
	ollamaLabelStyle  lipgloss.Style
	ollamaActiveArrow lipgloss.Style
	ollamaErrStyle    lipgloss.Style
	ollamaHelpStyle   lipgloss.Style

	fileSymlinkStyle    lipgloss.Style
	fileExeStyle        lipgloss.Style
	diagramPreviewStyle lipgloss.Style

	themePickerBoxStyle   lipgloss.Style
	themePickerTitleStyle lipgloss.Style
	themePickerHelpStyle  lipgloss.Style
	themePickerRowStyle   lipgloss.Style

	themeBackground color.Color
	themeForeground color.Color
	activeTheme     theme
)

func applyTheme(t theme) {
	activeTheme = t
	themeBackground = t.background
	themeForeground = t.foreground
	selectedStyle = lipgloss.NewStyle().Foreground(t.accent).Bold(true)
	dimStyle = lipgloss.NewStyle().Foreground(t.dim)
	promptStyle = lipgloss.NewStyle().Foreground(t.prompt)
	promptArrowStyle = lipgloss.NewStyle().Foreground(t.accentAlt)
	promptDotStyle = lipgloss.NewStyle().Foreground(t.promptDot)
	cwdStyle = lipgloss.NewStyle().Foreground(t.prompt)
	errStyle = lipgloss.NewStyle().Foreground(t.errorFG)
	userBarStyle = lipgloss.NewStyle().
		MarginLeft(3).
		Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(t.accent).
		PaddingLeft(1)
	outputStyle = lipgloss.NewStyle().MarginLeft(5)
	thinkingStyle = lipgloss.NewStyle().MarginLeft(3)
	chipStyle = lipgloss.NewStyle().MarginLeft(3).Foreground(t.accent).Bold(true)
	scrollThumbStyle = lipgloss.NewStyle().Foreground(t.dim)
	scrollTrackStyle = lipgloss.NewStyle().Foreground(t.scrollTrack)
	thumbBorderStyle = lipgloss.NewStyle().Foreground(t.accentAlt)
	pathBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.accent).
		Padding(0, 1)

	todoBoxStyle = lipgloss.NewStyle().
		MarginLeft(3).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.promptDot).
		Padding(0, 1)
	todoPendingStyle = lipgloss.NewStyle().Foreground(t.dim)
	todoProgressStyle = lipgloss.NewStyle().Foreground(t.accentAlt).Bold(true)
	todoCompletedStyle = lipgloss.NewStyle().Foreground(t.dim).Strikethrough(true)

	diffPathStyle = lipgloss.NewStyle().Foreground(t.accentAlt).Bold(true)
	diffHunkHeaderStyle = lipgloss.NewStyle().Foreground(t.dim)
	diffAddStyle = lipgloss.NewStyle().Foreground(t.success)
	diffDelStyle = lipgloss.NewStyle().Foreground(t.errorFG)
	diffContextStyle = lipgloss.NewStyle().Foreground(t.dim)

	askBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.accent).
		Padding(1, 2)
	askTabStyle = lipgloss.NewStyle().Padding(0, 2).Foreground(t.dim)
	askTabActiveStyle = lipgloss.NewStyle().Padding(0, 2).Foreground(t.inverseFG).Background(t.tabActive).Bold(true)
	askPromptStyle = lipgloss.NewStyle().Foreground(t.accentAlt).Bold(true)
	askOptionSelected = lipgloss.NewStyle().Foreground(t.success)
	askOptionCursorFG = lipgloss.NewStyle().Foreground(t.dim)
	askOptionRowStyle = lipgloss.NewStyle().Background(t.rowHL)
	askHelpStyle = lipgloss.NewStyle().Foreground(t.dim)
	askConfirmKeyStyle = lipgloss.NewStyle().Foreground(t.accentAlt)
	askSummaryDimStyle = lipgloss.NewStyle().Foreground(t.dim)
	askNoteLabelStyle = lipgloss.NewStyle().Foreground(t.warn)
	askPlaceholder = lipgloss.NewStyle().Foreground(t.dim).Italic(true)
	askCaretStyle = lipgloss.NewStyle().Foreground(t.accent)
	askConfirmBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.errorFG).
		Padding(1, 2)
	askConfirmBtnStyle = lipgloss.NewStyle().Padding(0, 3).Foreground(t.dim)
	askConfirmBtnActiveStyle = lipgloss.NewStyle().Padding(0, 3).Foreground(t.inverseFG).Background(t.errorFG).Bold(true)

	approvalBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.warn).
		Padding(1, 2)
	approvalBtnStyle = lipgloss.NewStyle().Padding(0, 2).Foreground(t.dim)
	approvalDenyActiveStyle = lipgloss.NewStyle().Padding(0, 2).Foreground(t.inverseFG).Background(t.errorFG).Bold(true)
	approvalAllowActiveStyle = lipgloss.NewStyle().Padding(0, 2).Foreground(t.inverseFG).Background(t.success).Bold(true)
	approvalAlwaysActiveStyle = lipgloss.NewStyle().Padding(0, 2).Foreground(t.darkFG).Background(t.warn).Bold(true)
	approvalTitleStyle = lipgloss.NewStyle().Foreground(t.warn).Bold(true)
	approvalToolStyle = lipgloss.NewStyle().Foreground(t.accentAlt).Bold(true)
	approvalSummaryStyle = lipgloss.NewStyle().Foreground(t.muted)

	configBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.accent).
		Padding(0, 1)
	configTitleStyle = lipgloss.NewStyle().Foreground(t.accent).Bold(true)
	configPromptStyle = lipgloss.NewStyle().Foreground(t.accentAlt)
	configPlaceholderStyle = lipgloss.NewStyle().Foreground(t.dim)
	configCaretStyle = lipgloss.NewStyle().Foreground(t.accent)
	configSelectedRowStyle = lipgloss.NewStyle().Foreground(t.darkFG).Background(t.accent).Bold(true)
	configKeyDimStyle = lipgloss.NewStyle().Foreground(t.dim)
	configHelpStyle = lipgloss.NewStyle().Foreground(t.dim)

	ollamaBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.accentAlt).
		Padding(1, 2)
	ollamaTitleStyle = lipgloss.NewStyle().Foreground(t.accentAlt).Bold(true)
	ollamaLabelStyle = lipgloss.NewStyle().Foreground(t.warn)
	ollamaActiveArrow = lipgloss.NewStyle().Foreground(t.accent)
	ollamaErrStyle = lipgloss.NewStyle().Foreground(t.errorFG)
	ollamaHelpStyle = lipgloss.NewStyle().Foreground(t.dim)

	fileSymlinkStyle = lipgloss.NewStyle().Foreground(t.accentAlt)
	fileExeStyle = lipgloss.NewStyle().Foreground(t.success)
	diagramPreviewStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.accentAlt).
		Padding(0, 1)

	themePickerBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.accent).
		Padding(1, 2)
	themePickerTitleStyle = lipgloss.NewStyle().Foreground(t.accent).Bold(true)
	themePickerHelpStyle = lipgloss.NewStyle().Foreground(t.dim)
	themePickerRowStyle = lipgloss.NewStyle().Foreground(t.darkFG).Background(t.accent).Bold(true)
}

func init() {
	applyTheme(defaultTheme())
}
