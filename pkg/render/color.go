package render

import (
	"image/color"
	"strconv"
	"strings"
)

var transparent = color.RGBA{0, 0, 0, 0}

var namedColors = map[string]color.RGBA{
	"transparent": {0, 0, 0, 0},
	"black":       {0, 0, 0, 255},
	"white":       {255, 255, 255, 255},
	"red":         {255, 0, 0, 255},
	"green":       {0, 128, 0, 255},
	"blue":        {0, 0, 255, 255},
	"gray":        {128, 128, 128, 255},
	"grey":        {128, 128, 128, 255},
	"silver":      {192, 192, 192, 255},
	"maroon":      {128, 0, 0, 255},
	"navy":        {0, 0, 128, 255},
	"olive":       {128, 128, 0, 255},
	"teal":        {0, 128, 128, 255},
	"purple":      {128, 0, 128, 255},
	"orange":      {255, 165, 0, 255},
	"yellow":      {255, 255, 0, 255},
	"lime":        {0, 255, 0, 255},
	"aqua":        {0, 255, 255, 255},
	"cyan":        {0, 255, 255, 255},
	"fuchsia":     {255, 0, 255, 255},
	"magenta":     {255, 0, 255, 255},
	"pink":        {255, 192, 203, 255},
	"brown":       {165, 42, 42, 255},
	"gold":        {255, 215, 0, 255},
	"indigo":      {75, 0, 130, 255},
	"violet":      {238, 130, 238, 255},
	"crimson":     {220, 20, 60, 255},
	"coral":       {255, 127, 80, 255},
	"salmon":      {250, 128, 114, 255},
	"khaki":       {240, 230, 140, 255},
	"lightgray":   {211, 211, 211, 255},
	"lightgrey":   {211, 211, 211, 255},
	"darkgray":    {169, 169, 169, 255},
	"darkgrey":    {169, 169, 169, 255},
	"lightblue":   {173, 216, 230, 255},
	"lightgreen":  {144, 238, 144, 255},
	"darkblue":    {0, 0, 139, 255},
	"darkgreen":   {0, 100, 0, 255},
	"darkred":     {139, 0, 0, 255},
	"whitesmoke":  {245, 245, 245, 255},
	"slategray":   {112, 128, 144, 255},
	"slategrey":   {112, 128, 144, 255},
}

// parseColor parses a CSS color. ok is false when the value is not a
// recognizable color (the caller decides the fallback and whether to warn).
func parseColor(s string) (col color.RGBA, ok bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return transparent, false
	}
	if c, found := namedColors[s]; found {
		return c, true
	}
	if strings.HasPrefix(s, "#") {
		return parseHexColor(s[1:])
	}
	if strings.HasPrefix(s, "rgb") {
		return parseRGBColor(s)
	}
	return transparent, false
}

func parseHexColor(h string) (color.RGBA, bool) {
	switch len(h) {
	case 3: // rgb
		r, ok1 := hexNibble(h[0])
		g, ok2 := hexNibble(h[1])
		b, ok3 := hexNibble(h[2])
		if ok1 && ok2 && ok3 {
			return color.RGBA{r * 17, g * 17, b * 17, 255}, true
		}
	case 4: // rgba
		r, ok1 := hexNibble(h[0])
		g, ok2 := hexNibble(h[1])
		b, ok3 := hexNibble(h[2])
		a, ok4 := hexNibble(h[3])
		if ok1 && ok2 && ok3 && ok4 {
			return color.RGBA{r * 17, g * 17, b * 17, a * 17}, true
		}
	case 6: // rrggbb
		r, ok1 := hexByte(h[0:2])
		g, ok2 := hexByte(h[2:4])
		b, ok3 := hexByte(h[4:6])
		if ok1 && ok2 && ok3 {
			return color.RGBA{r, g, b, 255}, true
		}
	case 8: // rrggbbaa
		r, ok1 := hexByte(h[0:2])
		g, ok2 := hexByte(h[2:4])
		b, ok3 := hexByte(h[4:6])
		a, ok4 := hexByte(h[6:8])
		if ok1 && ok2 && ok3 && ok4 {
			return color.RGBA{r, g, b, a}, true
		}
	}
	return transparent, false
}

func hexNibble(c byte) (uint8, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

func hexByte(s string) (uint8, bool) {
	hi, ok1 := hexNibble(s[0])
	lo, ok2 := hexNibble(s[1])
	if ok1 && ok2 {
		return hi*16 + lo, true
	}
	return 0, false
}

func parseRGBColor(s string) (color.RGBA, bool) {
	open := strings.IndexByte(s, '(')
	close := strings.IndexByte(s, ')')
	if open < 0 || close < 0 || close < open {
		return transparent, false
	}
	inner := s[open+1 : close]
	inner = strings.ReplaceAll(inner, "/", ",")
	fields := strings.FieldsFunc(inner, func(r rune) bool { return r == ',' || r == ' ' })
	if len(fields) < 3 {
		return transparent, false
	}
	r, ok1 := parseColorChannel(fields[0])
	g, ok2 := parseColorChannel(fields[1])
	b, ok3 := parseColorChannel(fields[2])
	if !ok1 || !ok2 || !ok3 {
		return transparent, false
	}
	a := uint8(255)
	if len(fields) >= 4 {
		af, err := strconv.ParseFloat(strings.TrimSpace(fields[3]), 64)
		if err != nil {
			return transparent, false
		}
		if strings.HasSuffix(fields[3], "%") {
			af = af / 100
		}
		a = clampByte(af * 255)
	}
	return color.RGBA{r, g, b, a}, true
}

func parseColorChannel(f string) (uint8, bool) {
	f = strings.TrimSpace(f)
	if strings.HasSuffix(f, "%") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(f, "%"), 64)
		if err != nil {
			return 0, false
		}
		return clampByte(v / 100 * 255), true
	}
	v, err := strconv.ParseFloat(f, 64)
	if err != nil {
		return 0, false
	}
	return clampByte(v), true
}

func clampByte(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}
