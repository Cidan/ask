package main

import (
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// chatView is the chat-history viewport. It replaces bubbles' viewport.Model
// with a state container that drives the lazy rendering pipeline in view.go.
//
// chatView itself is dumb: it owns scroll position (yOffset), the on-screen
// dimensions (width, height) and the wrapping style; the wrap cache lives on
// each historyEntry. layoutChat() mutates these fields after each frame's
// pre-render pass, and chatViewContent() consumes them to produce the visible
// window.
//
// The reason the rendering is not folded inside this type is that glamour
// rendering of histResponse entries requires the model's *glamour.TermRenderer
// (which is theme-/width-bound on the model), so making chatView own the
// pipeline would either require an upward dependency on *model or a callback
// pyramid. Keeping chatView as state + math leaves rendering inside view.go
// where the existing helpers already live.
type chatView struct {
	yOffset int
	width   int
	height  int
	style   lipgloss.Style

	mouseWheelEnabled bool
	mouseWheelDelta   int

	// totalLines is populated by layoutChat() each frame from the per-entry
	// cache. AtBottom/MaxYOffset/ScrollPercent all read it; nothing inside
	// chatView updates it.
	totalLines int
}

func newChatView() chatView {
	return chatView{
		mouseWheelEnabled: true,
		mouseWheelDelta:   3,
	}
}

func (c chatView) Width() int  { return c.width }
func (c chatView) Height() int { return c.height }

// SetWidth/SetHeight are intentionally tolerant of negative input — callers in
// view.go clamp to a minimum of 1 before assigning, and tests sometimes pass
// 0 to assert behaviour at a degenerate size.
func (c *chatView) SetWidth(w int)  { c.width = w }
func (c *chatView) SetHeight(h int) { c.height = h }

func (c chatView) YOffset() int        { return c.yOffset }
func (c chatView) TotalLineCount() int { return c.totalLines }

// VisibleLineCount returns the number of content rows actually filled by
// entries (i.e. not blank padding rows). Used by scrollbarChars() to compute
// the thumb size.
func (c chatView) VisibleLineCount() int {
	h := c.contentHeight()
	if h <= 0 {
		return 0
	}
	rem := c.totalLines - c.yOffset
	if rem <= 0 {
		return 0
	}
	return min(rem, h)
}

// MaxYOffset is the largest valid scroll offset. Mirrors bubbles' behaviour
// of allowing the bottom row of content to sit flush with the viewport bottom.
func (c chatView) MaxYOffset() int {
	return max(0, c.totalLines-c.contentHeight())
}

func (c chatView) AtBottom() bool { return c.yOffset >= c.MaxYOffset() }
func (c chatView) AtTop() bool    { return c.yOffset <= 0 }

// ScrollPercent returns the scroll position as a fraction in [0, 1]. Returns 1
// when the entire content fits in the viewport (matches bubbles' semantics so
// the scrollbar stays at the bottom, not flickers to top).
func (c chatView) ScrollPercent() float64 {
	maxY := c.MaxYOffset()
	if maxY <= 0 {
		return 1.0
	}
	return max(0, min(1, float64(c.yOffset)/float64(maxY)))
}

// SetYOffset clamps to [0, MaxYOffset]. Always use this rather than poking
// c.yOffset directly so streaming-while-scrolled doesn't push the offset past
// the now-larger total and produce blank rows at the bottom.
func (c *chatView) SetYOffset(n int) {
	c.yOffset = max(0, min(c.MaxYOffset(), n))
}

func (c *chatView) GotoBottom() { c.SetYOffset(c.MaxYOffset()) }
func (c *chatView) GotoTop()    { c.yOffset = 0 }

// ScrollUp / ScrollDown move by n lines, clamped at the edges. n=0 is a
// no-op so callers can pass viewport.Height()/2 unconditionally.
func (c *chatView) ScrollUp(n int) {
	if n <= 0 || c.AtTop() {
		return
	}
	c.SetYOffset(c.yOffset - n)
}

func (c *chatView) ScrollDown(n int) {
	if n <= 0 || c.AtBottom() {
		return
	}
	c.SetYOffset(c.yOffset + n)
}

// contentHeight is the inner height after the style's vertical frame
// (PaddingTop, PaddingBottom, MarginTop, MarginBottom, BorderTopSize,
// BorderBottomSize). Visible-line math always uses this rather than c.height.
func (c chatView) contentHeight() int {
	return max(0, c.height-c.style.GetVerticalFrameSize())
}

// contentWidth is the inner width after the style's horizontal frame. Used by
// the wrap cache to know where to break long lines.
func (c chatView) contentWidth() int {
	return max(0, c.width-c.style.GetHorizontalFrameSize())
}

// Update handles mouse-wheel scroll inside the chat history. Anything else is
// ignored — keyboard scrolling (PgUp/PgDn) is dispatched from update.go so it
// can interact with shell mode and the slash-cmd menu first.
func (c chatView) Update(msg tea.Msg) (chatView, tea.Cmd) {
	if !c.mouseWheelEnabled {
		return c, nil
	}
	wheel, ok := msg.(tea.MouseWheelMsg)
	if !ok {
		return c, nil
	}
	switch wheel.Button {
	case tea.MouseWheelDown:
		c.ScrollDown(c.mouseWheelDelta)
	case tea.MouseWheelUp:
		c.ScrollUp(c.mouseWheelDelta)
	}
	return c, nil
}
