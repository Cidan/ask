package main

import (
	"image"
	"strconv"
	"strings"

	glamour "charm.land/glamour/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/Cidan/ask/pkg/providers"
	uv "github.com/charmbracelet/ultraviolet"
)

// Overlay geometry. The box is the terminal minus a fixed margin on each
// side; small terminals collapse the margin to one cell so the list keeps
// its rows. These numbers are expected to be tuned.
const (
	modelPickerMarginX     = 4
	modelPickerMarginY     = 2
	modelPickerSmallWidth  = 100
	modelPickerSmallHeight = 30
	modelPickerListPercent = 38
	modelPickerMinListW    = 24
	modelPickerMinDetailW  = 20
	modelPickerScrollStep  = 5
	modelPickerFactLabelW  = 14
)

func modelPickerMargins(width, height int) (mx, my int) {
	mx, my = modelPickerMarginX, modelPickerMarginY
	if width < modelPickerSmallWidth {
		mx = 1
	}
	if height < modelPickerSmallHeight {
		my = 1
	}
	return mx, my
}

// modelPickerGeometry splits a width×height terminal into the box and its
// two columns: listW + " │ " + detailW == innerW.
func modelPickerGeometry(width, height int) (boxW, boxH, listW, detailW, innerH int) {
	mx, my := modelPickerMargins(width, height)
	boxW = width - 2*mx
	boxH = height - 2*my
	innerW := boxW - modelPickerBoxStyle.GetHorizontalFrameSize()
	innerH = boxH - modelPickerBoxStyle.GetVerticalFrameSize()
	listW = innerW * modelPickerListPercent / 100
	if listW < modelPickerMinListW {
		listW = modelPickerMinListW
	}
	if innerW-listW-3 < modelPickerMinDetailW {
		listW = innerW - 3 - modelPickerMinDetailW
	}
	if listW < 8 {
		listW = 8
	}
	detailW = innerW - listW - 3
	if detailW < 1 {
		detailW = 1
	}
	return boxW, boxH, listW, detailW, innerH
}

// modelPickerOverlay renders the picker sized for a width×height terminal,
// or "" when the picker is not open. The app layer composites it over the
// whole frame (tabs.go) — it is not part of the tab body.
func (m model) modelPickerOverlay(width, height int) string {
	s := m.modelPicker
	if s == nil || m.mode != modeModelPicker || width < 24 || height < 8 {
		return ""
	}
	_, _, listW, detailW, innerH := modelPickerGeometry(width, height)
	left := m.renderModelPickerList(s, listW, innerH)
	right := m.renderModelPickerDetail(s, detailW, innerH)
	div := modelPickerDividerStyle.Render("│")
	lines := make([]string, innerH)
	for i := range lines {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lines[i] = padRight(clipText(l, listW), listW) + " " + div + " " + padRight(clipText(r, detailW), detailW)
	}
	return modelPickerBoxStyle.Render(strings.Join(lines, "\n"))
}

// drawOverlayCentered paints overlay over base at the centre of a w×h frame.
func drawOverlayCentered(base, overlay string, w, h int) string {
	canvas := uv.NewScreenBuffer(w, h)
	uv.NewStyledString(base).Draw(canvas, image.Rect(0, 0, w, h))
	ow, oh := lipgloss.Width(overlay), lipgloss.Height(overlay)
	x, y := (w-ow)/2, (h-oh)/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	uv.NewStyledString(overlay).Draw(canvas, image.Rect(x, y, x+ow, y+oh))
	return canvas.Render()
}

// modelPickerWindow returns the [start, end) slice of n rows that keeps
// cursor visible in a size-row viewport.
func modelPickerWindow(n, cursor, size int) (int, int) {
	if size < 1 {
		size = 1
	}
	start := 0
	if cursor >= size {
		start = cursor - size + 1
	}
	if start > n-size {
		start = n - size
	}
	if start < 0 {
		start = 0
	}
	end := start + size
	if end > n {
		end = n
	}
	return start, end
}

func (m model) renderModelPickerList(s *modelPickerState, w, h int) []string {
	lines := make([]string, 0, h)
	lines = append(lines, configPromptStyle.Render("> ")+filterPromptLine(s.query, "Type to search"))
	lines = append(lines, "")

	rowsH := h - 4
	if rowsH < 1 {
		rowsH = 1
	}
	rows := s.rows()
	if len(rows) == 0 {
		lines = append(lines, dimStyle.Render("  (no matches)"))
	}
	start, end := modelPickerWindow(len(rows), s.cursor, rowsH)
	for i := start; i < end; i++ {
		lines = append(lines, m.renderModelPickerRow(rows[i], w, i == s.cursor))
	}
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	if len(lines) > h-1 {
		lines = lines[:h-1]
	}
	help := "↑↓ choose · enter select · esc"
	if s.loading {
		help = "loading models… · " + help
	}
	lines = append(lines, themePickerHelpStyle.Render(clipText(help, w)))
	return lines
}

func (m model) renderModelPickerDetail(s *modelPickerState, w, h int) []string {
	var body []string
	hint := "pgup/pgdn scroll · ctrl+r refresh"
	switch {
	case s.keyEntry != nil:
		body = modelPickerKeyEntryLines(s.keyEntry, w)
		hint = "enter save & switch · esc back"
	case s.customEntry != nil:
		body = modelPickerCustomEntryLines(s.customEntry, w)
		hint = "enter select · esc back"
	default:
		rows := s.rows()
		if s.cursor >= 0 && s.cursor < len(rows) {
			switch r := rows[s.cursor]; r.kind {
			case modelPickerRowEntry:
				body = s.modelDetailLines(r.entry, w)
			case modelPickerRowCustom:
				body = []string{
					themePickerTitleStyle.Render(modelPickerCustomRowLabel),
					configKeyDimStyle.Render(clipText(r.entry.providerName, w)),
					"",
					dimStyle.Render(clipText("Type the exact model id to use with "+r.entry.providerName+".", w)),
				}
			}
		}
		if body == nil {
			body = []string{dimStyle.Render("(no matches)")}
		}
	}

	avail := h - 2
	if avail < 1 {
		avail = 1
	}
	maxScroll := len(body) - avail
	if maxScroll < 0 {
		maxScroll = 0
	}
	off := s.detailScroll
	if off > maxScroll {
		off = maxScroll
	}
	end := off + avail
	if end > len(body) {
		end = len(body)
	}
	lines := append([]string(nil), body[off:end]...)
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	if end < len(body) {
		hint = "↓ more · " + hint
	}
	lines = append(lines, themePickerHelpStyle.Render(clipText(hint, w)))
	return lines
}

type modelFact struct{ label, value string }

// modelDetailFacts turns metadata into the label/value rows of the detail
// pane; every row is omitted when its value is unknown.
func modelDetailFacts(meta providers.ModelMeta) []modelFact {
	var facts []modelFact
	if meta.ContextWindow > 0 {
		facts = append(facts, modelFact{"Context", groupDigits(meta.ContextWindow) + " tokens"})
	}
	if meta.MaxOutputTokens > 0 {
		facts = append(facts, modelFact{"Max output", groupDigits(meta.MaxOutputTokens) + " tokens"})
	}
	if p := meta.Pricing; p != nil {
		if p.InputPer1M == 0 && p.OutputPer1M == 0 {
			facts = append(facts, modelFact{"Pricing", "free"})
		} else {
			facts = append(facts,
				modelFact{"Input", formatUSDPer1M(p.InputPer1M)},
				modelFact{"Output", formatUSDPer1M(p.OutputPer1M)},
			)
			if p.CachedInputPer1M > 0 {
				facts = append(facts, modelFact{"Cached input", formatUSDPer1M(p.CachedInputPer1M)})
			}
			if p.CacheWritePer1M > 0 {
				facts = append(facts, modelFact{"Cache write", formatUSDPer1M(p.CacheWritePer1M)})
			}
		}
	}
	if len(meta.InputModalities) > 0 {
		facts = append(facts, modelFact{"Modalities", strings.Join(meta.InputModalities, " · ")})
	}
	switch {
	case len(meta.ReasoningLevels) > 0:
		facts = append(facts, modelFact{"Reasoning", strings.Join(meta.ReasoningLevels, " · ")})
	case meta.Reasoning:
		facts = append(facts, modelFact{"Reasoning", "yes"})
	}
	if meta.KnowledgeCutoff != "" {
		facts = append(facts, modelFact{"Knowledge", meta.KnowledgeCutoff})
	}
	if meta.ReleaseDate != "" {
		facts = append(facts, modelFact{"Released", meta.ReleaseDate})
	}
	if meta.Status != "" {
		facts = append(facts, modelFact{"Status", meta.Status})
	}
	return facts
}

// formatUSDPer1M renders a per-1M price with at least two and at most four
// decimals: 0.075 → "$0.075 / 1M", 15 → "$15.00 / 1M".
func formatUSDPer1M(v float64) string {
	s := strings.TrimRight(strconv.FormatFloat(v, 'f', 4, 64), "0")
	i := strings.IndexByte(s, '.')
	if i < 0 {
		s += ".00"
	} else if dec := len(s) - i - 1; dec < 2 {
		s += strings.Repeat("0", 2-dec)
	}
	return "$" + s + " / 1M"
}

func (s *modelPickerState) modelDetailLines(e modelPickerEntry, w int) []string {
	lines := []string{
		themePickerTitleStyle.Render(clipText(e.display, w)),
		configKeyDimStyle.Render(clipText(e.providerName+" · "+e.modelID, w)),
		"",
	}
	meta, ok := modelMetaFor(e.providerID, e.modelID)
	if !ok {
		return append(lines, dimStyle.Render("no information available"))
	}
	if meta.Description != "" {
		lines = append(lines, s.renderDescription(e, meta.Description, w)...)
		lines = append(lines, "")
	}
	for _, f := range modelDetailFacts(meta) {
		valueStyle := lipgloss.NewStyle()
		if f.label == "Status" {
			valueStyle = modelPickerStatusStyle
		}
		lines = append(lines,
			configKeyDimStyle.Render(padRight(f.label, modelPickerFactLabelW))+
				valueStyle.Render(clipText(f.value, w-modelPickerFactLabelW)))
	}
	return lines
}

func (s *modelPickerState) renderDescription(e modelPickerEntry, md string, w int) []string {
	key := e.providerID + "\x00" + e.modelID + "\x00" + strconv.Itoa(w)
	if s.descCache == nil {
		s.descCache = map[string]string{}
	}
	out, ok := s.descCache[key]
	if !ok {
		out = renderModelDescription(md, w)
		s.descCache[key] = out
	}
	return strings.Split(out, "\n")
}

// renderModelDescription runs the description through glamour at the pane
// width (provider descriptions carry markdown — links, emphasis), falling
// back to a plain word wrap if the renderer cannot be built.
func renderModelDescription(md string, w int) string {
	if w < 10 {
		w = 10
	}
	style := buildGlamourStyle(activeTheme)
	zero := uint(0)
	style.Document.Margin = &zero
	if r, err := glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(w)); err == nil {
		if out, err := r.Render(md); err == nil {
			return strings.Trim(out, "\n")
		}
	}
	return strings.Join(wordWrap(md, w), "\n")
}

func modelPickerKeyEntryLines(ke *modelPickerKeyEntry, w int) []string {
	lines := []string{themePickerTitleStyle.Render(clipText(ke.title(), w)), ""}
	help := ke.title() + " is not configured. Paste or type one to continue — it is stored in ~/.config/ask/ask.json (0600)."
	if ke.field.EnvKey != "" {
		help += " $" + ke.field.EnvKey + " also works."
	}
	for _, l := range wordWrap(help, w) {
		lines = append(lines, configHelpStyle.Render(l))
	}
	return append(lines,
		"",
		dimStyle.Render(clipText("model: "+ke.pending.display, w)),
		configPromptStyle.Render("> ")+ke.draft+configCaretStyle.Render("▏"),
	)
}

func modelPickerCustomEntryLines(ce *modelPickerCustomEntry, w int) []string {
	lines := []string{themePickerTitleStyle.Render(clipText("Custom "+ce.providerName+" model", w)), ""}
	for _, l := range wordWrap("Type the exact model id to use with "+ce.providerName+".", w) {
		lines = append(lines, configHelpStyle.Render(l))
	}
	return append(lines, "", configPromptStyle.Render("> ")+ce.draft+configCaretStyle.Render("▏"))
}

func modelPickerRowPlain(r modelPickerRow, m model) string {
	switch r.kind {
	case modelPickerRowHeader:
		t := r.title
		if r.note != "" {
			t += "  (" + r.note + ")"
		}
		return t
	case modelPickerRowEntry:
		line := "  " + r.entry.display
		if r.recent {
			line += " · " + r.entry.providerName
		}
		if m.modelPickerEntryIsCurrent(r.entry) {
			line += " ✓"
		}
		return line
	case modelPickerRowCustom:
		return "  " + modelPickerCustomRowLabel
	}
	return ""
}

func (m model) modelPickerEntryIsCurrent(e modelPickerEntry) bool {
	if m.provider == nil || m.provider.ID() != e.providerID {
		return false
	}
	if m.providerModel != "" {
		return strings.EqualFold(m.providerModel, e.modelID)
	}
	opts := m.provider.ModelPicker().Options
	return len(opts) > 0 && strings.EqualFold(opts[0], e.modelID)
}

func (m model) renderModelPickerRow(r modelPickerRow, width int, selected bool) string {
	switch r.kind {
	case modelPickerRowBlank:
		return ""
	case modelPickerRowHeader:
		line := configKeyDimStyle.Bold(true).Render(clipText(r.title, width))
		if r.note != "" {
			line += "  " + errStyle.Render(clipText("("+r.note+")", width-lipgloss.Width(r.title)-2))
		}
		return line
	}

	plain := modelPickerRowPlain(r, m)
	if selected {
		line := clipText("▸ "+strings.TrimPrefix(plain, "  "), width)
		if pad := width - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		return themePickerRowStyle.Render(line)
	}
	if r.kind == modelPickerRowCustom {
		return dimStyle.Render(plain)
	}
	if r.recent {
		return clipText("  "+r.entry.display+dimStyle.Render(" · "+r.entry.providerName)+
			modelPickerCurrentMark(m, r.entry), width)
	}
	return clipText("  "+r.entry.display+modelPickerCurrentMark(m, r.entry), width)
}

func modelPickerCurrentMark(m model, e modelPickerEntry) string {
	if m.modelPickerEntryIsCurrent(e) {
		return promptStyle.Render(" ✓")
	}
	return ""
}
