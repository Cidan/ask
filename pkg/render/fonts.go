package render

import (
	"image/color"
	"math"
	"strings"
	"sync"

	"github.com/tdewolff/canvas"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gobolditalic"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/goregular"
)

// ptPerPx converts a CSS pixel size into the point size the canvas font
// engine expects. The canvas keeps its coordinate unit at one millimeter,
// and the renderer maps one CSS pixel to one millimeter, so a face at this
// point size has an em of exactly pxSize millimeters (= pxSize CSS px).
const ptPerPx = 72.0 / 25.4

var fontTTF = map[string][]byte{
	"sans":    goregular.TTF,
	"sans-b":  gobold.TTF,
	"sans-i":  goitalic.TTF,
	"sans-bi": gobolditalic.TTF,
	"mono":    gomono.TTF,
	"mono-b":  gomonobold.TTF,
	"medium":  gomedium.TTF,
}

var (
	fontMu       sync.Mutex
	fontFamilies = map[string]*canvas.FontFamily{}
	faceCache    = map[faceKey]*canvas.FontFace{}
)

type faceKey struct {
	key string
	px  int
	col color.RGBA
}

// mapFamily resolves a css font-family list to one of the bundled font
// bases: "sans", "mono", or "medium". serif indicates the caller should
// warn that serif is unavailable. ok is false when nothing matched.
func mapFamily(list string) (base string, serif bool, ok bool) {
	for _, raw := range strings.Split(list, ",") {
		name := strings.TrimSpace(strings.ToLower(raw))
		name = strings.Trim(name, "'\"")
		switch name {
		case "sans", "sans-serif", "ui-sans-serif", "system-ui", "go", "arial", "helvetica", "inter":
			return "sans", false, true
		case "mono", "monospace", "ui-monospace", "go mono", "courier", "consolas", "menlo":
			return "mono", false, true
		case "medium", "go medium":
			return "medium", false, true
		case "serif", "times", "times new roman", "georgia":
			return "sans", true, true
		}
	}
	return "sans", false, false
}

// fontKeyFor resolves the concrete bundled variant for a base family and
// bold/italic flags.
func fontKeyFor(base string, bold, italic bool) string {
	switch base {
	case "mono":
		if bold {
			return "mono-b"
		}
		return "mono"
	case "medium":
		return "medium"
	default:
		switch {
		case bold && italic:
			return "sans-bi"
		case bold:
			return "sans-b"
		case italic:
			return "sans-i"
		default:
			return "sans"
		}
	}
}

// fontFamily returns the canvas font family for a bundled variant, loading
// and caching it on first use. It falls back to "sans" when the requested
// key is unknown or fails to load. Callers must hold fontMu.
func fontFamily(key string) *canvas.FontFamily {
	if fam, ok := fontFamilies[key]; ok {
		return fam
	}
	ttf := fontTTF[key]
	if ttf == nil {
		if key == "sans" {
			return nil
		}
		return fontFamily("sans")
	}
	fam := canvas.NewFontFamily(key)
	if err := fam.LoadFont(ttf, 0, canvas.FontRegular); err != nil {
		if key == "sans" {
			return nil
		}
		return fontFamily("sans")
	}
	fontFamilies[key] = fam
	return fam
}

// getFace returns a cached measurement face for the given bundled variant
// at the given CSS pixel size. Its metrics and text widths are in CSS px.
func getFace(key string, pxSize float64) *canvas.FontFace {
	return getFaceColored(key, pxSize, color.RGBA{0, 0, 0, 255})
}

// getFaceColored returns a cached face with the given fill color baked in,
// as canvas renders text using the face's own fill paint.
func getFaceColored(key string, pxSize float64, col color.Color) *canvas.FontFace {
	px := int(math.Round(pxSize))
	if px < 1 {
		px = 1
	}
	r, g, b, a := col.RGBA()
	rgba := color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
	fk := faceKey{key: key, px: px, col: rgba}
	fontMu.Lock()
	defer fontMu.Unlock()
	if face, ok := faceCache[fk]; ok {
		return face
	}
	fam := fontFamily(key)
	if fam == nil {
		return nil
	}
	face := fam.Face(float64(px)*ptPerPx, rgba)
	faceCache[fk] = face
	return face
}

func faceAscent(f *canvas.FontFace) float64 {
	if f == nil {
		return 0
	}
	return f.Metrics().Ascent
}

func measureText(f *canvas.FontFace, s string) float64 {
	if f == nil {
		return 0
	}
	return f.TextWidth(s)
}
