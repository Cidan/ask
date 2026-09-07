package render

import (
	"image/color"

	"github.com/tdewolff/canvas"
)

// paint walks the box tree parent-first, drawing each block/flex box and its
// text runs onto the canvas context. Coordinates are in CSS pixels; the
// device scale is applied once by the rasterizer.
func (e *engine) paint(ctx *canvas.Context, b *box) {
	if b.disp == "block" || b.disp == "flex" {
		e.paintBox(ctx, b)
	}
	for _, c := range b.children {
		if c.disp == "block" || c.disp == "flex" {
			e.paint(ctx, c)
		}
	}
}

func (e *engine) paintBox(ctx *canvas.Context, b *box) {
	x, y, w, h := b.x, b.y, b.w, b.h
	r := b.style.borderRadius

	if r > 0.5 {
		e.paintRounded(ctx, b, x, y, w, h, r)
	} else {
		if b.style.hasBackground {
			fillRect(ctx, x, y, w, h, b.style.background)
		}
		e.paintSquareBorders(ctx, b, x, y, w, h)
	}
	e.paintText(ctx, b)
}

func (e *engine) paintSquareBorders(ctx *canvas.Context, b *box, x, y, w, h float64) {
	bt := b.bt
	br := b.br
	bb := b.bb
	bl := b.bl
	col := func(i int) color.RGBA {
		if b.style.border[i].has {
			return b.style.border[i].color
		}
		return b.style.color
	}
	if bt > 0 {
		fillRect(ctx, x, y, w, bt, col(0))
	}
	if bb > 0 {
		fillRect(ctx, x, y+h-bb, w, bb, col(2))
	}
	if bl > 0 {
		fillRect(ctx, x, y, bl, h, col(3))
	}
	if br > 0 {
		fillRect(ctx, x+w-br, y, br, h, col(1))
	}
}

func (e *engine) paintRounded(ctx *canvas.Context, b *box, x, y, w, h, r float64) {
	if b.style.hasBackground {
		ctx.SetFillColor(b.style.background)
		ctx.DrawPath(x, y, canvas.RoundedRectangle(w, h, r))
	}
	bw := b.bt
	if b.br > bw {
		bw = b.br
	}
	if b.bb > bw {
		bw = b.bb
	}
	if b.bl > bw {
		bw = b.bl
	}
	if bw > 0 {
		bc := b.style.color
		if b.style.border[0].has {
			bc = b.style.border[0].color
		}
		innerR := r - bw
		if innerR < 0 {
			innerR = 0
		}
		// A ring: the inner rounded rectangle is reversed so that, under the
		// nonzero fill rule, it cuts a hole out of the outer one.
		ring := canvas.RoundedRectangle(w, h, r)
		ring.Append(canvas.RoundedRectangle(w-2*bw, h-2*bw, innerR).Translate(bw, bw).Reverse())
		ctx.SetFillColor(bc)
		ctx.DrawPath(x, y, ring)
	}
}

func (e *engine) paintText(ctx *canvas.Context, b *box) {
	for _, run := range b.runs {
		face := getFaceColored(run.fontKey, run.px, run.col)
		if face == nil {
			continue
		}
		ctx.DrawText(run.x, run.baseline, canvas.NewTextLine(face, run.text, canvas.Left))
	}
}

// fillRect fills the CSS-px rectangle at (x,y) of size w×h with col.
func fillRect(ctx *canvas.Context, x, y, w, h float64, col color.RGBA) {
	if w <= 0 || h <= 0 {
		return
	}
	ctx.SetFillColor(col)
	ctx.DrawPath(x, y, canvas.Rectangle(w, h))
}
