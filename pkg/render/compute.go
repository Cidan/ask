package render

import (
	"image/color"
	"strconv"
	"strings"
)

func (e *engine) computeFromDeclared(d map[string]string, parent *computed) *computed {
	c := defaultComputed()
	// inherited properties
	c.color = parent.color
	c.fontFamily = parent.fontFamily
	c.fontSize = parent.fontSize
	c.fontWeight = parent.fontWeight
	c.italic = parent.italic
	c.lineHeight = parent.lineHeight
	c.textAlign = parent.textAlign

	// font-size first so em on other props (and defaults) resolve against
	// the parent size; em here is also relative to the parent size.
	if v, ok := d["font-size"]; ok {
		c.fontSize = e.parseFontSize(v, parent.fontSize)
	}
	if v, ok := d["color"]; ok {
		if col, ok := parseColor(v); ok {
			c.color = col
		} else {
			e.warn("unknown color: " + strings.TrimSpace(v))
			c.color = color.RGBA{0, 0, 0, 255}
		}
	}
	if v, ok := d["font-family"]; ok {
		c.fontFamily = v
		if _, serif, _ := mapFamily(v); serif {
			e.warn("serif not bundled, using sans")
		}
	}
	if v, ok := d["font-weight"]; ok {
		c.fontWeight = e.parseFontWeight(v)
	}
	if v, ok := d["font-style"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "normal":
			c.italic = false
		case "italic", "oblique":
			c.italic = true
		default:
			e.warn("unsupported font-style: " + strings.TrimSpace(v))
		}
	}
	if v, ok := d["line-height"]; ok {
		c.lineHeight = e.parseLineHeight(v)
	}
	if v, ok := d["text-align"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "left", "center", "right":
			c.textAlign = strings.ToLower(strings.TrimSpace(v))
		default:
			e.warn("unsupported text-align: " + strings.TrimSpace(v))
		}
	}

	if v, ok := d["display"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "block", "flex", "inline", "inline-block", "none":
			c.display = strings.ToLower(strings.TrimSpace(v))
		default:
			e.warn("unsupported display: " + strings.TrimSpace(v))
			c.display = "block"
		}
	}
	if v, ok := d["box-sizing"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "content-box", "border-box":
			c.boxSizing = strings.ToLower(strings.TrimSpace(v))
		default:
			e.warn("unsupported box-sizing: " + strings.TrimSpace(v))
		}
	}

	c.width = e.parseLenProp(d, "width", c.width)
	c.height = e.parseLenProp(d, "height", c.height)
	c.minWidth = e.parseLenProp(d, "min-width", c.minWidth)
	c.maxWidth = e.parseLenProp(d, "max-width", c.maxWidth)

	e.applyBoxLen(d, "margin", &c.margin)
	e.applyBoxLen(d, "padding", &c.padding)

	e.applyBorders(d, c)

	if v, ok := d["border-radius"]; ok {
		if r, ok := parsePxNumber(strings.Fields(v)[0]); ok {
			c.borderRadius = r
		}
	}

	e.applyBackground(d, c)
	e.applyFlex(d, c)

	return c
}

func (e *engine) parseFontSize(v string, parentSize float64) float64 {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "small":
		return 13
	case "medium":
		return 16
	case "large":
		return 20
	case "x-small":
		return 10
	case "x-large":
		return 24
	}
	if strings.HasSuffix(v, "em") {
		if n, err := strconv.ParseFloat(strings.TrimSuffix(v, "em"), 64); err == nil {
			return n * parentSize
		}
	}
	if strings.HasSuffix(v, "rem") {
		if n, err := strconv.ParseFloat(strings.TrimSuffix(v, "rem"), 64); err == nil {
			return n * 16
		}
	}
	if n, ok := parsePxNumber(v); ok {
		return n
	}
	e.warn("unsupported font-size: " + v)
	return parentSize
}

func (e *engine) parseFontWeight(v string) int {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "normal":
		return 400
	case "bold":
		return 700
	case "bolder":
		return 700
	case "lighter":
		return 300
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 1000 {
		return n
	}
	e.warn("unsupported font-weight: " + v)
	return 400
}

func (e *engine) parseLineHeight(v string) lineHeight {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "normal" {
		return lineHeight{mult: true, v: 1.3}
	}
	if strings.HasSuffix(v, "px") {
		if n, ok := parsePxNumber(v); ok {
			return lineHeight{mult: false, v: n}
		}
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return lineHeight{mult: true, v: n}
	}
	e.warn("unsupported line-height: " + v)
	return lineHeight{mult: true, v: 1.3}
}

func (e *engine) parseLenProp(d map[string]string, name string, def length) length {
	v, ok := d[name]
	if !ok {
		return def
	}
	l, ok := parseLength(v)
	if !ok {
		e.warn("unsupported " + name + " value: " + strings.TrimSpace(v))
		return def
	}
	return l
}

func (e *engine) applyBoxLen(d map[string]string, prefix string, out *[4]length) {
	if v, ok := d[prefix]; ok {
		vals := expandBox(fields(v))
		for i, s := range vals {
			if l, ok := parseLength(s); ok {
				out[i] = l
			}
		}
	}
	sides := [4]string{"-top", "-right", "-bottom", "-left"}
	for i, suf := range sides {
		if v, ok := d[prefix+suf]; ok {
			if l, ok := parseLength(v); ok {
				out[i] = l
			}
		}
	}
}

func (e *engine) applyBorders(d map[string]string, c *computed) {
	if v, ok := d["border"]; ok {
		w, col, hasCol := e.parseBorderShorthand(v)
		for i := range c.border {
			c.border[i].width = w
			if hasCol {
				c.border[i].color = col
				c.border[i].has = true
			}
		}
	}
	sides := map[string]int{"border-top": 0, "border-right": 1, "border-bottom": 2, "border-left": 3}
	for name, i := range sides {
		if v, ok := d[name]; ok {
			w, col, hasCol := e.parseBorderShorthand(v)
			c.border[i].width = w
			if hasCol {
				c.border[i].color = col
				c.border[i].has = true
			}
		}
	}
	if v, ok := d["border-width"]; ok {
		vals := expandBox(fields(v))
		for i, s := range vals {
			if w, ok := parsePxNumber(s); ok {
				c.border[i].width = w
			}
		}
	}
	if v, ok := d["border-style"]; ok {
		e.checkBorderStyle(v)
	}
	if v, ok := d["border-color"]; ok {
		vals := expandBox(fields(v))
		for i, s := range vals {
			if col, ok := parseColor(s); ok {
				c.border[i].color = col
				c.border[i].has = true
			} else {
				e.warn("unknown color: " + strings.TrimSpace(s))
			}
		}
	}
}

func (e *engine) parseBorderShorthand(v string) (width float64, col color.RGBA, hasColor bool) {
	for _, tok := range fields(v) {
		lt := strings.ToLower(tok)
		if w, ok := parsePxNumber(tok); ok {
			width = w
			continue
		}
		if isBorderStyle(lt) {
			e.checkBorderStyle(lt)
			continue
		}
		if c, ok := parseColor(tok); ok {
			col = c
			hasColor = true
			continue
		}
		e.warn("unsupported border value: " + tok)
	}
	return width, col, hasColor
}

var borderStyles = map[string]bool{
	"solid": true, "none": true, "hidden": true, "dotted": true, "dashed": true,
	"double": true, "groove": true, "ridge": true, "inset": true, "outset": true,
}

func isBorderStyle(s string) bool { return borderStyles[s] }

func (e *engine) checkBorderStyle(v string) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v != "solid" && v != "none" && v != "hidden" && borderStyles[v] {
		e.warn("border-style " + v + " rendered as solid")
	}
}

func (e *engine) applyBackground(d map[string]string, c *computed) {
	if v, ok := d["background-color"]; ok {
		if col, ok := parseColor(v); ok {
			c.background = col
			c.hasBackground = true
		} else {
			e.warn("unknown color: " + strings.TrimSpace(v))
		}
	}
	if v, ok := d["background"]; ok {
		lv := strings.ToLower(v)
		if strings.Contains(lv, "gradient") || strings.Contains(lv, "url(") {
			e.warn("unsupported background value: images/gradients not supported")
		}
		for _, tok := range fields(v) {
			if col, ok := parseColor(tok); ok {
				c.background = col
				c.hasBackground = true
				break
			}
		}
	}
}

func (e *engine) applyFlex(d map[string]string, c *computed) {
	if v, ok := d["flex-direction"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "row", "column":
			c.flexDirection = strings.ToLower(strings.TrimSpace(v))
		default:
			e.warn("unsupported flex-direction: " + strings.TrimSpace(v))
		}
	}
	if v, ok := d["justify-content"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "flex-start", "center", "flex-end", "space-between", "space-around":
			c.justifyContent = strings.ToLower(strings.TrimSpace(v))
		default:
			e.warn("unsupported justify-content: " + strings.TrimSpace(v))
		}
	}
	if v, ok := d["align-items"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "stretch", "flex-start", "center", "flex-end":
			c.alignItems = strings.ToLower(strings.TrimSpace(v))
		default:
			e.warn("unsupported align-items: " + strings.TrimSpace(v))
		}
	}
	if v, ok := d["gap"]; ok {
		if g, ok := parsePxNumber(strings.Fields(v)[0]); ok {
			c.gap = g
		}
	}
	if v, ok := d["flex-grow"]; ok {
		if g, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			c.flexGrow = g
		}
	}
	if v, ok := d["flex"]; ok {
		e.applyFlexShorthand(v, c)
	}
}

func (e *engine) applyFlexShorthand(v string, c *computed) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "none":
		c.flexGrow = 0
		return
	case "auto":
		c.flexGrow = 1
		e.warn("unsupported flex value: " + v)
		return
	}
	parts := fields(v)
	if len(parts) == 0 {
		return
	}
	if g, err := strconv.ParseFloat(parts[0], 64); err == nil {
		c.flexGrow = g
		if len(parts) > 1 {
			e.warn("complex flex shorthand simplified to flex-grow")
		}
		return
	}
	e.warn("unsupported flex value: " + v)
}
