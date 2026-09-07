package render

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func decode(t *testing.T, r Result) image.Image {
	t.Helper()
	if len(r.PNG) == 0 {
		t.Fatal("empty PNG")
	}
	img, err := png.Decode(bytes.NewReader(r.PNG))
	if err != nil {
		t.Fatalf("png decode: %v", err)
	}
	return img
}

func sampleAt(img image.Image, x, y int) color.RGBA {
	r, g, b, a := img.At(x, y).RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

func absDiff(a, b uint8) uint8 {
	if a > b {
		return a - b
	}
	return b - a
}

func approxEq(a, b color.RGBA, tol uint8) bool {
	return absDiff(a.R, b.R) <= tol &&
		absDiff(a.G, b.G) <= tol &&
		absDiff(a.B, b.B) <= tol
}

func mustRender(t *testing.T, h, c string, o Options) Result {
	t.Helper()
	r, err := Render(h, c, o)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	return r
}

var (
	red   = color.RGBA{255, 0, 0, 255}
	green = color.RGBA{0, 128, 0, 255}
	blue  = color.RGBA{0, 0, 255, 255}
	white = color.RGBA{255, 255, 255, 255}
	black = color.RGBA{0, 0, 0, 255}
)

func TestDimensionsAndScale(t *testing.T) {
	r := mustRender(t, "<body></body>", "", Options{Width: 400, Height: 200, Scale: 2})
	if r.Width != 800 || r.Height != 400 {
		t.Fatalf("got %dx%d, want 800x400", r.Width, r.Height)
	}
	img := decode(t, r)
	if img.Bounds().Dx() != 800 || img.Bounds().Dy() != 400 {
		t.Fatalf("decoded %v, want 800x400", img.Bounds())
	}
}

func TestBackgroundColor(t *testing.T) {
	r := mustRender(t, "<body></body>", "body{background:#ff0000}", Options{Width: 200, Height: 100, Scale: 1})
	img := decode(t, r)
	if got := sampleAt(img, 100, 50); !approxEq(got, red, 8) {
		t.Fatalf("center pixel %v, want red", got)
	}
}

func TestFlexSidebarSplit(t *testing.T) {
	h := `<body><div class="row"><div class="side"></div><div class="main"></div></div></body>`
	c := `.row{display:flex;height:400px}
	      .side{width:200px;background:#0000ff}
	      .main{flex-grow:1;background:#ffffff}`
	r := mustRender(t, h, c, Options{Width: 1000, Height: 400, Scale: 1})
	img := decode(t, r)
	if got := sampleAt(img, 100, 200); !approxEq(got, blue, 8) {
		t.Fatalf("left pixel %v, want blue", got)
	}
	if got := sampleAt(img, 600, 200); !approxEq(got, white, 8) {
		t.Fatalf("right pixel %v, want white", got)
	}
}

func TestPaddingAndMargin(t *testing.T) {
	h := `<body><div class="pad"><div class="inner"></div></div></body>`
	c := `.pad{padding:50px;background:#00ff00;width:300px}
	      .inner{height:100px;background:#ff0000}`
	r := mustRender(t, h, c, Options{Width: 400, Height: 200, Scale: 1, Background: "#ffffff"})
	img := decode(t, r)

	pureGreen := color.RGBA{0, 255, 0, 255}
	if got := sampleAt(img, 10, 100); !approxEq(got, pureGreen, 8) {
		t.Fatalf("left padding band %v, want green", got)
	}
	if got := sampleAt(img, 200, 100); !approxEq(got, red, 8) {
		t.Fatalf("inner %v, want red", got)
	}
	// child starts at x=50: just left is green, just inside is red.
	if got := sampleAt(img, 45, 100); !approxEq(got, pureGreen, 8) {
		t.Fatalf("pixel at x=45 %v, want green (padding)", got)
	}
	if got := sampleAt(img, 60, 100); !approxEq(got, red, 8) {
		t.Fatalf("pixel at x=60 %v, want red (child)", got)
	}
}

func TestBorder(t *testing.T) {
	h := `<body><div class="b"></div></body>`
	c := `.b{width:100px;height:100px;border:10px solid #000000;background:#ff8800}`
	r := mustRender(t, h, c, Options{Width: 200, Height: 200, Scale: 1})
	img := decode(t, r)

	orange := color.RGBA{0xff, 0x88, 0x00, 255}
	if got := sampleAt(img, 2, 60); !approxEq(got, black, 8) {
		t.Fatalf("left border %v, want black", got)
	}
	if got := sampleAt(img, 60, 2); !approxEq(got, black, 8) {
		t.Fatalf("top border %v, want black", got)
	}
	if got := sampleAt(img, 60, 60); !approxEq(got, orange, 8) {
		t.Fatalf("interior %v, want orange", got)
	}
}

func luminance(c color.RGBA) int {
	return (int(c.R)*299 + int(c.G)*587 + int(c.B)*114) / 1000
}

func TestTextPresenceAndColor(t *testing.T) {
	h := `<body><div class="t">Hello World</div></body>`
	c := `.t{color:#000000;background:#ffffff;font-size:40px}`
	r := mustRender(t, h, c, Options{Width: 400, Height: 100, Scale: 1})
	img := decode(t, r)

	dark := 0
	var sample color.RGBA
	for y := 0; y < 60; y++ {
		for x := 0; x < 400; x++ {
			px := sampleAt(img, x, y)
			if luminance(px) < 128 {
				dark++
				sample = px
			}
		}
	}
	if dark == 0 {
		t.Fatal("no dark text pixels found")
	}
	// a sampled glyph pixel must be closer to text color (black) than bg (white).
	if luminance(sample) >= luminance(white)/2 {
		t.Fatalf("glyph pixel %v not close to text color", sample)
	}
}

func TestDisplayNone(t *testing.T) {
	h := `<body><div class="hid"></div><div class="show"></div></body>`
	c := `.hid{display:none;height:50px;background:#ff0000}
	      .show{height:50px;background:#0000ff}`
	r := mustRender(t, h, c, Options{Width: 200, Height: 100, Scale: 1, Background: "#ffffff"})
	img := decode(t, r)

	if got := sampleAt(img, 100, 25); !approxEq(got, blue, 8) {
		t.Fatalf("show box %v, want blue at top", got)
	}
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			if approxEq(sampleAt(img, x, y), red, 40) {
				t.Fatalf("found red pixel at (%d,%d); display:none leaked", x, y)
			}
		}
	}
}

func TestWarnings(t *testing.T) {
	h := `<body><div><p>x</p></div></body>`
	c := `div{float:left} div > p{color:#ff0000}`
	r := mustRender(t, h, c, Options{Width: 100, Height: 100, Scale: 1})
	joined := strings.Join(r.Warnings, "\n")
	if !strings.Contains(joined, "unsupported property: float") {
		t.Fatalf("missing float warning; got %v", r.Warnings)
	}
	if !strings.Contains(joined, "unsupported selector: div > p") {
		t.Fatalf("missing selector warning; got %v", r.Warnings)
	}

	h2 := `<body><div class="a">hi</div></body>`
	c2 := `.a{display:flex;color:#000000;background:#ffffff;padding:10px;margin:5px;width:100px;font-size:16px}`
	r2 := mustRender(t, h2, c2, Options{Width: 300, Height: 200, Scale: 1})
	if len(r2.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", r2.Warnings)
	}
}

func TestMalformedInput(t *testing.T) {
	h := `<div><span>oops</div></span><p>more`
	c := `body{color:#000000} } .x{width:100px`
	r, err := Render(h, c, Options{Width: 200, Height: 100, Scale: 1})
	if err != nil {
		t.Fatalf("unexpected error on malformed input: %v", err)
	}
	if len(r.PNG) == 0 {
		t.Fatal("expected a PNG despite malformed input")
	}
	decode(t, r)
}
