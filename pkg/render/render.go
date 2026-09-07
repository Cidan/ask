// Package render rasterizes a compact subset of HTML and CSS to a PNG
// entirely in-process: no browser, no subprocess, no cgo. It is intended
// for generating design previews that a model can inspect as an image.
package render

import (
	"bytes"
	"image/color"
	"image/png"
	"math"
	"sort"
	"strings"

	"github.com/tdewolff/canvas"
	"github.com/tdewolff/canvas/renderers/rasterizer"
	"golang.org/x/net/html"
)

type Options struct {
	Width      int    // CSS px viewport width; if <=0 default 1200
	Height     int    // CSS px viewport height; if <=0 grow to fit content
	Scale      int    // device pixel ratio; if <=0 default 2; clamp to 1..4
	Background string // page background if body/html sets none; default "#ffffff"
}

type Result struct {
	PNG      []byte
	Width    int
	Height   int
	Warnings []string
}

type engine struct {
	scale    int
	warnings map[string]struct{}
}

func (e *engine) warn(msg string) {
	e.warnings[msg] = struct{}{}
}

func (e *engine) sortedWarnings() []string {
	if len(e.warnings) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.warnings))
	for w := range e.warnings {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

func Render(htmlSrc, cssSrc string, opts Options) (Result, error) {
	e := &engine{warnings: map[string]struct{}{}}

	width := opts.Width
	if width <= 0 {
		width = 1200
	}
	scale := opts.Scale
	if scale <= 0 {
		scale = 2
	}
	if scale < 1 {
		scale = 1
	}
	if scale > 4 {
		scale = 4
	}
	e.scale = scale

	doc, err := html.Parse(strings.NewReader(htmlSrc))
	if err != nil {
		e.warn("html parse error, rendered best-effort")
		doc, _ = html.Parse(strings.NewReader(""))
	}

	var styleText strings.Builder
	collectStyleText(doc, &styleText)
	sheet, cerr := parseCSS(cssSrc + "\n" + styleText.String())
	if cerr != nil {
		e.warn("css parse error, rendered best-effort")
		if sheet == nil {
			if s2, e2 := parseCSS(cssSrc); e2 == nil {
				sheet = s2
			}
		}
	}
	rules := e.buildRules(sheet)

	htmlNode := findElement(doc, "html")
	var root *box
	if htmlNode != nil {
		root = e.buildBox(htmlNode, nil, rules, defaultComputed())
	}
	if root == nil {
		root = &box{tag: "html", disp: "block", style: defaultComputed()}
	}

	bw := e.computeBlockWidth(root, float64(width))
	e.layoutBlock(root, root.ml, root.mt, bw, -1)

	usedW := width
	usedH := opts.Height
	if usedH <= 0 {
		usedH = int(math.Ceil(root.y + root.h + root.mb))
	}
	if usedH < 1 {
		usedH = 1
	}
	if usedW < 1 {
		usedW = 1
	}

	pageBg := color.RGBA{255, 255, 255, 255}
	if opts.Background != "" {
		if c, ok := parseColor(opts.Background); ok {
			pageBg = c
		}
	}
	if root.style.hasBackground {
		pageBg = root.style.background
	}
	if body := findChildBox(root, "body"); body != nil && body.style.hasBackground {
		pageBg = body.style.background
	}

	// Draw in CSS-px canvas coordinates with a top-left origin (y down); the
	// rasterizer applies the device scale so output px = CSS px * scale.
	c := canvas.New(float64(usedW), float64(usedH))
	ctx := canvas.NewContext(c)
	ctx.SetCoordSystem(canvas.CartesianIV)

	ctx.SetFillColor(pageBg)
	ctx.DrawPath(0, 0, canvas.Rectangle(float64(usedW), float64(usedH)))

	e.paint(ctx, root)

	img := rasterizer.Draw(c, canvas.DPMM(float64(scale)), canvas.DefaultColorSpace)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return Result{}, err
	}
	return Result{
		PNG:      buf.Bytes(),
		Width:    img.Bounds().Dx(),
		Height:   img.Bounds().Dy(),
		Warnings: e.sortedWarnings(),
	}, nil
}

func findElement(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && strings.ToLower(n.Data) == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElement(c, tag); found != nil {
			return found
		}
	}
	return nil
}

func findChildBox(b *box, tag string) *box {
	for _, c := range b.children {
		if c.tag == tag {
			return c
		}
	}
	return nil
}
