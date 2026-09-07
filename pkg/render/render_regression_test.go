package render

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

// A flex container lays each item out twice — once to measure its natural
// size at the container origin, once to place it. Text runs must not survive
// the measurement pass, or every flex item's text is painted a second time
// piled at the container's left edge. This guards that regression: the text
// of the right item must never land on the plain green left box.
func TestFlexTextNotDuplicatedOverSibling(t *testing.T) {
	html := `<div class="row"><div class="left"></div><div class="right">AAAAAAAAAAAA</div></div>`
	css := `.row{display:flex;flex-direction:row}
.left{width:200px;height:100px;background:#00ff00}
.right{flex-grow:1;height:100px;background:#ffffff;color:#000000;font-size:40px}`

	res, err := Render(html, css, Options{Width: 800, Height: 120, Scale: 1})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(res.PNG))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	dark := func(r *image.Rectangle) int {
		n := 0
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				cr, cg, cb, _ := img.At(x, y).RGBA()
				if cr>>8 < 60 && cg>>8 < 60 && cb>>8 < 60 {
					n++
				}
			}
		}
		return n
	}

	// The left box spans x:[0,200), y:[0,100). Its interior must be pure green —
	// zero (near-)black pixels, i.e. no text bled onto it.
	leftInterior := image.Rect(5, 5, 195, 95)
	if got := dark(&leftInterior); got > 0 {
		t.Errorf("text bled onto the left box: %d dark pixels over the green sibling (want 0)", got)
	}

	// Sanity: the text really did render (once) in the right item.
	rightArea := image.Rect(205, 5, 795, 95)
	if got := dark(&rightArea); got == 0 {
		t.Errorf("expected the right item's text to render, found 0 dark pixels")
	}
}

// An auto-width flex item (no explicit width, no flex-grow) must size to its
// max-content width, not collapse to 0. When it collapsed, every word wrapped
// onto its own line. Here a fixed sidebar sits beside an auto-width content
// column whose single line of text fits with room to spare; that text must
// stay on one line — a single horizontal band of dark pixels, not four.
func TestFlexAutoWidthItemDoesNotCollapse(t *testing.T) {
	html := `<div class="row"><div class="side"></div>` +
		`<div class="content"><div class="line">one two three four</div></div></div>`
	css := `.row{display:flex;flex-direction:row}
.side{width:200px;height:200px;background:#dddddd}
.content{background:#ffffff}
.line{color:#000000;font-size:30px}`

	res, err := Render(html, css, Options{Width: 1000, Height: 200, Scale: 1})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(res.PNG))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Count horizontal bands of rows containing dark text pixels. Scan only to
	// the right of the sidebar so its grey fill never counts.
	b := img.Bounds()
	bands, inBand := 0, false
	for y := b.Min.Y; y < b.Max.Y; y++ {
		rowDark := false
		for x := 210; x < b.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			if cr>>8 < 60 && cg>>8 < 60 && cb>>8 < 60 {
				rowDark = true
				break
			}
		}
		if rowDark && !inBand {
			bands++
		}
		inBand = rowDark
	}
	if bands != 1 {
		t.Fatalf("auto-width flex item collapsed: text spans %d line bands, want 1 (each word wrapped)", bands)
	}
}
