package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"strings"
)

const (
	thumbMaxCols = 20
	thumbMaxRows = 8
	thumbCellW   = 10
	thumbCellH   = 20
)

func isKitty() bool {
	term := os.Getenv("TERM")
	if strings.Contains(term, "kitty") || strings.Contains(term, "ghostty") {
		return true
	}
	return os.Getenv("KITTY_WINDOW_ID") != ""
}

func kittyTransmitPNG(id uint32, pngData []byte) error {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer tty.Close()

	b64 := base64.StdEncoding.EncodeToString(pngData)
	const chunk = 4096
	first := true
	for len(b64) > 0 {
		n := len(b64)
		more := 0
		if n > chunk {
			n = chunk
			more = 1
		}
		var header string
		if first {
			header = fmt.Sprintf("a=T,f=100,i=%d,U=1,q=2,m=%d", id, more)
			first = false
		} else {
			header = fmt.Sprintf("m=%d,q=2", more)
		}
		if _, err := fmt.Fprintf(tty, "\x1b_G%s;%s\x1b\\", header, b64[:n]); err != nil {
			return err
		}
		b64 = b64[n:]
	}
	return nil
}

func kittyPlaceholderRows(id uint32, cols, rows int) string {
	if rows <= 0 || cols <= 0 {
		return ""
	}
	if rows > len(kittyDiacritics) {
		rows = len(kittyDiacritics)
	}
	if cols > len(kittyDiacritics) {
		cols = len(kittyDiacritics)
	}
	r := byte(id >> 16)
	g := byte(id >> 8)
	b := byte(id)
	colorOn := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
	colorOff := "\x1b[39m"

	var out strings.Builder
	for ri := 0; ri < rows; ri++ {
		out.WriteString(colorOn)
		out.WriteRune(0x10EEEE)
		out.WriteRune(kittyDiacritics[ri])
		out.WriteRune(kittyDiacritics[0])
		for ci := 1; ci < cols; ci++ {
			out.WriteRune(0x10EEEE)
		}
		out.WriteString(colorOff)
		if ri < rows-1 {
			out.WriteRune('\n')
		}
	}
	return out.String()
}

func thumbnailGrid(imgW, imgH int) (cols, rows int) {
	if imgW <= 0 || imgH <= 0 {
		return 4, 2
	}
	boxW := thumbMaxCols * thumbCellW
	boxH := thumbMaxRows * thumbCellH
	scaleW := float64(boxW) / float64(imgW)
	scaleH := float64(boxH) / float64(imgH)
	scale := scaleW
	if scaleH < scale {
		scale = scaleH
	}
	pxW := float64(imgW) * scale
	pxH := float64(imgH) * scale
	cols = int((pxW + float64(thumbCellW-1)) / float64(thumbCellW))
	rows = int((pxH + float64(thumbCellH-1)) / float64(thumbCellH))
	if cols < 2 {
		cols = 2
	}
	if rows < 1 {
		rows = 1
	}
	if cols > thumbMaxCols {
		cols = thumbMaxCols
	}
	if rows > thumbMaxRows {
		rows = thumbMaxRows
	}
	return
}

func encodeToPNG(data []byte) ([]byte, int, int, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	cols, rows := thumbnailGrid(w, h)
	preview := centerCropAndScale(img, cols*thumbCellW, rows*thumbCellH)
	var buf bytes.Buffer
	if err := png.Encode(&buf, preview); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}

func centerCropAndScale(src image.Image, dstW, dstH int) *image.NRGBA {
	if dstW <= 0 || dstH <= 0 {
		return image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 {
		return image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	}

	cropW, cropH := srcW, srcH
	if srcW*dstH > srcH*dstW {
		cropW = max(1, srcH*dstW/dstH)
	} else if srcW*dstH < srcH*dstW {
		cropH = max(1, srcW*dstH/dstW)
	}
	cropX := b.Min.X + (srcW-cropW)/2
	cropY := b.Min.Y + (srcH-cropH)/2

	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		sy := cropY + y*cropH/dstH
		for x := 0; x < dstW; x++ {
			sx := cropX + x*cropW/dstW
			dst.Set(x, y, color.NRGBAModel.Convert(src.At(sx, sy)))
		}
	}
	return dst
}
