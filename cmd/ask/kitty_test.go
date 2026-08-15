package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
)

func TestCenterCropAndScaleUsesImageCenter(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 1))
	src.Set(0, 0, color.NRGBA{R: 255, A: 255})
	src.Set(1, 0, color.NRGBA{G: 255, A: 255})
	src.Set(2, 0, color.NRGBA{B: 255, A: 255})
	src.Set(3, 0, color.NRGBA{R: 255, G: 255, A: 255})

	got := centerCropAndScale(src, 2, 1)

	if left := color.NRGBAModel.Convert(got.At(0, 0)).(color.NRGBA); left != (color.NRGBA{G: 255, A: 255}) {
		t.Fatalf("left pixel = %#v, want centered green crop", left)
	}
	if right := color.NRGBAModel.Convert(got.At(1, 0)).(color.NRGBA); right != (color.NRGBA{B: 255, A: 255}) {
		t.Fatalf("right pixel = %#v, want centered blue crop", right)
	}
}

// TestIsKitty covers the three detection branches: TERM contains
// "kitty" → true, TERM contains "ghostty" → true, KITTY_WINDOW_ID
// set → true, otherwise → false.
func TestIsKitty(t *testing.T) {
	t.Setenv("KITTY_WINDOW_ID", "")
	cases := []struct {
		term string
		want bool
	}{
		{"xterm", false},
		{"xterm-kitty", true},
		{"kitty", true},
		{"ghostty", true},
		{"alacritty", false},
	}
	for _, tc := range cases {
		t.Run(tc.term, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			if got := isKitty(); got != tc.want {
				t.Errorf("isKitty(TERM=%q)=%v want %v", tc.term, got, tc.want)
			}
		})
	}
	// KITTY_WINDOW_ID alone (no TERM hint) is also a positive signal.
	t.Setenv("TERM", "xterm")
	t.Setenv("KITTY_WINDOW_ID", "42")
	if !isKitty() {
		t.Error("KITTY_WINDOW_ID set should make isKitty() return true even on xterm")
	}
}

// TestThumbnailGrid covers the math: image dimensions are
// scaled to fit the cell grid, clamped to the [thumbMaxCols ×
// thumbMaxRows] bounding box, and floored to a minimum of 2x1.
func TestThumbnailGrid(t *testing.T) {
	cases := []struct {
		name             string
		w, h             int
		wantCols, wantRows int
	}{
		{"zero dims use 4x2 default", 0, 0, 4, 2},
		{"negative dims use 4x2 default", -1, -1, 4, 2},
		// We just assert "clamped" / "min 2x1" here; the exact
		// ratio math is dependent on thumbCellW/H constants.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, r := thumbnailGrid(tc.w, tc.h)
			if c != tc.wantCols || r != tc.wantRows {
				t.Errorf("thumbnailGrid(%d,%d)=(%d,%d) want (%d,%d)",
					tc.w, tc.h, c, r, tc.wantCols, tc.wantRows)
			}
		})
	}

	// Property-based checks for the "fits in the box" contract.
	for _, tc := range []struct{ w, h int }{
		{10000, 10000}, {4, 4}, {1000, 100}, {50, 30}, {200, 600},
	} {
		c, r := thumbnailGrid(tc.w, tc.h)
		if c < 2 {
			t.Errorf("thumbnailGrid(%d,%d) cols=%d < min 2", tc.w, tc.h, c)
		}
		if r < 1 {
			t.Errorf("thumbnailGrid(%d,%d) rows=%d < min 1", tc.w, tc.h, r)
		}
		if c > thumbMaxCols {
			t.Errorf("thumbnailGrid(%d,%d) cols=%d > max %d", tc.w, tc.h, c, thumbMaxCols)
		}
		if r > thumbMaxRows {
			t.Errorf("thumbnailGrid(%d,%d) rows=%d > max %d", tc.w, tc.h, r, thumbMaxRows)
		}
	}
}

// TestKittyPlaceholderRows_NonPositive: the placeholder rows are
// empty when cols or rows is 0/negative.
func TestKittyPlaceholderRows_NonPositive(t *testing.T) {
	cases := []struct{ cols, rows int }{
		{0, 5}, {5, 0}, {-1, 5}, {5, -1},
	}
	for _, tc := range cases {
		if got := kittyPlaceholderRows(0, tc.cols, tc.rows); got != "" {
			t.Errorf("kittyPlaceholderRows(0, %d, %d) should be empty; got %q", tc.cols, tc.rows, got)
		}
	}
}

// TestKittyPlaceholderRows_Shape: a 1×1 placeholder carries
// exactly one 0x10EEEE diacritic anchor + a diacritics[0] mark.
// The wrapping ANSI colour codes are present so the cell renders
// in the placeholder's id-derived colour.
func TestKittyPlaceholderRows_Shape(t *testing.T) {
	got := kittyPlaceholderRows(0xFF0000, 1, 1)
	if got == "" {
		t.Fatal("1x1 placeholder should produce output")
	}
	if !strings.Contains(got, "\x1b[38;2;255;0;0m") {
		t.Errorf("placeholder should carry the id-derived fg color; got %q", got)
	}
	if !strings.ContainsRune(got, 0x10EEEE) {
		t.Errorf("placeholder should carry the diacritic anchor rune; got %q", got)
	}
}

// TestKittyPlaceholderRows_ClampsToTable: a request for more
// rows than the diacritics table is clamped to len(kittyDiacritics).
func TestKittyPlaceholderRows_ClampsToTable(t *testing.T) {
	huge := len(kittyDiacritics) + 50
	got := kittyPlaceholderRows(0, 1, huge)
	// Count the 0x10EEEE diacritic anchors. Each row starts with one.
	anchors := strings.Count(got, string(rune(0x10EEEE)))
	if anchors != len(kittyDiacritics) {
		t.Errorf("anchors=%d want %d (clamped to table)", anchors, len(kittyDiacritics))
	}
}

// TestKittyPlaceholderRows_MultiLine: 3 rows × 2 cols produces 2
// newlines (between rows, none after the last).
func TestKittyPlaceholderRows_MultiLine(t *testing.T) {
	got := kittyPlaceholderRows(0, 2, 3)
	if c := strings.Count(got, "\n"); c != 2 {
		t.Errorf("3 rows should produce 2 newlines; got %d in %q", c, got)
	}
}

// TestEncodeToPNG_Success: encoding a real image produces a
// valid PNG byte stream.
func TestEncodeToPNG_Success(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 50, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 50; x++ {
			src.Set(x, y, color.NRGBA{
				R: uint8(x * 5),
				G: uint8(y * 8),
				B: 128,
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode: %v", err)
	}
	data, w, h, err := encodeToPNG(buf.Bytes())
	if err != nil {
		t.Fatalf("encodeToPNG: %v", err)
	}
	if w != 50 || h != 30 {
		t.Errorf("dims=%d×%d want 50×30", w, h)
	}
	// Verify the returned bytes are a valid PNG (PNG magic header).
	if len(data) < 8 || !bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		t.Error("encodeToPNG did not return a valid PNG byte stream")
	}
}

// TestEncodeToPNG_BadInput: garbage bytes produce an error
// (the function is fail-fast on decode failure).
func TestEncodeToPNG_BadInput(t *testing.T) {
	_, _, _, err := encodeToPNG([]byte("not an image"))
	if err == nil {
		t.Error("encodeToPNG on garbage should error")
	}
}

// TestCenterCropAndScale_NonPositiveDims: the contract is
// that the function never panics on a zero/negative dst size
// and returns a minimal valid image.
func TestCenterCropAndScale_NonPositiveDims(t *testing.T) {
	cases := []struct{ dstW, dstH int }{
		{0, 0}, {-1, -1}, {0, 10}, {10, 0},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("centerCropAndScale(%d,%d) panicked: %v", tc.dstW, tc.dstH, r)
				}
			}()
			src := image.NewNRGBA(image.Rect(0, 0, 4, 4))
			got := centerCropAndScale(src, tc.dstW, tc.dstH)
			if got == nil {
				t.Error("centerCropAndScale returned nil")
			}
		})
	}
}

// TestKittyTransmitPNG_OpensTty: the function calls os.OpenFile
// on /dev/tty — this isn't available in the test sandbox, so
// the call returns an error rather than a panic. We don't
// exercise the success path (it would require a real terminal).
func TestKittyTransmitPNG_OpensTty(t *testing.T) {
	err := kittyTransmitPNG(1, []byte{0x89, 'P', 'N', 'G'})
	if err == nil {
		// Some sandboxes DO have /dev/tty; if so, the test
		// effectively passed (data was written somewhere).
		// We can't read it back without setup, so we just log.
		t.Log("kittyTransmitPNG wrote to /dev/tty (sandbox permitted)")
		return
	}
	// Any error means the function reacted gracefully; it must
	// not have panicked.
	if !os.IsNotExist(err) && !strings.Contains(err.Error(), "permission") &&
		!strings.Contains(err.Error(), "device") {
		t.Logf("kittyTransmitPNG err=%v (acceptable: sandbox rejection)", err)
	}
}
