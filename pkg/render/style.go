package render

import (
	"image/color"
	"strconv"
	"strings"
)

type unit int

const (
	unitAuto unit = iota
	unitPx
	unitPercent
)

type length struct {
	u unit
	v float64
}

var autoLen = length{u: unitAuto}

// resolve turns a length into px. base is the reference for percentages;
// def is returned when the length is auto.
func (l length) resolve(base, def float64) float64 {
	switch l.u {
	case unitPx:
		return l.v
	case unitPercent:
		return base * l.v / 100
	default:
		return def
	}
}

func (l length) isAuto() bool { return l.u == unitAuto }

type edge struct {
	width float64
	color color.RGBA
	has   bool // a color/border was explicitly established for this side
}

type lineHeight struct {
	mult bool
	v    float64
}

func (lh lineHeight) px(fontSize float64) float64 {
	if lh.mult {
		return lh.v * fontSize
	}
	return lh.v
}

type computed struct {
	display string

	width     length
	height    length
	minWidth  length
	maxWidth  length
	margin    [4]length // top right bottom left
	padding   [4]length
	boxSizing string

	border       [4]edge
	borderRadius float64

	background    color.RGBA
	hasBackground bool

	color      color.RGBA
	fontFamily string
	fontSize   float64
	fontWeight int
	italic     bool
	lineHeight lineHeight
	textAlign  string

	flexDirection  string
	justifyContent string
	alignItems     string
	gap            float64
	flexGrow       float64
}

func (c *computed) bold() bool { return c.fontWeight >= 600 }

func (c *computed) fontKey() string {
	base, _, _ := mapFamily(c.fontFamily)
	return fontKeyFor(base, c.bold(), c.italic)
}

func defaultComputed() *computed {
	return &computed{
		display:        "block",
		width:          autoLen,
		height:         autoLen,
		minWidth:       autoLen,
		maxWidth:       autoLen,
		boxSizing:      "content-box",
		color:          color.RGBA{0, 0, 0, 255},
		fontFamily:     "sans-serif",
		fontSize:       16,
		fontWeight:     400,
		lineHeight:     lineHeight{mult: true, v: 1.3},
		textAlign:      "left",
		flexDirection:  "row",
		justifyContent: "flex-start",
		alignItems:     "stretch",
	}
}

func parseLength(s string) (length, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "":
		return autoLen, false
	case "auto":
		return autoLen, true
	case "0":
		return length{u: unitPx, v: 0}, true
	}
	if strings.HasSuffix(s, "%") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil {
			return autoLen, false
		}
		return length{u: unitPercent, v: v}, true
	}
	if strings.HasSuffix(s, "px") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(s, "px"), 64)
		if err != nil {
			return autoLen, false
		}
		return length{u: unitPx, v: v}, true
	}
	// bare number treated as px
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return length{u: unitPx, v: v}, true
	}
	return autoLen, false
}

// parsePxNumber reads a plain px or unitless number into px.
func parsePxNumber(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimSuffix(s, "px")
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// expandBox expands a 1-4 value shorthand into [top,right,bottom,left].
func expandBox(vals []string) [4]string {
	var out [4]string
	switch len(vals) {
	case 1:
		out = [4]string{vals[0], vals[0], vals[0], vals[0]}
	case 2:
		out = [4]string{vals[0], vals[1], vals[0], vals[1]}
	case 3:
		out = [4]string{vals[0], vals[1], vals[2], vals[1]}
	default:
		if len(vals) >= 4 {
			out = [4]string{vals[0], vals[1], vals[2], vals[3]}
		}
	}
	return out
}

func fields(s string) []string {
	return strings.Fields(s)
}
