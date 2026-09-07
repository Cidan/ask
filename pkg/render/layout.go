package render

import (
	"image/color"
	"strings"

	"github.com/tdewolff/canvas"
)

type textRun struct {
	text     string
	x        float64
	baseline float64
	col      color.RGBA
	fontKey  string
	px       float64
}

func resolveEdges(b *box, base float64) {
	s := b.style
	b.mt = s.margin[0].resolve(base, 0)
	b.mr = s.margin[1].resolve(base, 0)
	b.mb = s.margin[2].resolve(base, 0)
	b.ml = s.margin[3].resolve(base, 0)
	b.pt = s.padding[0].resolve(base, 0)
	b.pr = s.padding[1].resolve(base, 0)
	b.pb = s.padding[2].resolve(base, 0)
	b.pl = s.padding[3].resolve(base, 0)
	b.bt = s.border[0].width
	b.br = s.border[1].width
	b.bb = s.border[2].width
	b.bl = s.border[3].width
}

func extra(b *box) float64 { return b.pl + b.pr + b.bl + b.br }

func (e *engine) computeBlockWidth(b *box, cw float64) float64 {
	resolveEdges(b, cw)
	s := b.style
	var bbw float64
	if !s.width.isAuto() {
		w := s.width.resolve(cw, 0)
		if s.boxSizing == "border-box" {
			bbw = w
		} else {
			bbw = w + extra(b)
		}
	} else {
		bbw = cw - b.ml - b.mr
	}
	bbw = clampWidth(b, bbw, cw)
	if bbw < 0 {
		bbw = 0
	}
	return bbw
}

func clampWidth(b *box, bbw, cw float64) float64 {
	s := b.style
	adjust := func(l length) float64 {
		v := l.resolve(cw, 0)
		if s.boxSizing != "border-box" {
			v += extra(b)
		}
		return v
	}
	if !s.maxWidth.isAuto() {
		if mx := adjust(s.maxWidth); bbw > mx {
			bbw = mx
		}
	}
	if !s.minWidth.isAuto() {
		if mn := adjust(s.minWidth); bbw < mn {
			bbw = mn
		}
	}
	return bbw
}

func (e *engine) layoutBlock(b *box, x, y, bw, forcedH float64) {
	b.x, b.y, b.w = x, y, bw
	// Flex lays each item out twice (measure natural size, then place); reset
	// so the final pass replaces the measurement pass's runs instead of
	// accumulating a second, mispositioned copy of every text run.
	b.runs = nil
	b.contentW = bw - b.bl - b.br - b.pl - b.pr
	if b.contentW < 0 {
		b.contentW = 0
	}
	contentLeft := x + b.bl + b.pl
	contentTop := y + b.bt + b.pt

	var contentH float64
	if b.disp == "flex" {
		eh := explicitContentHeight(b, forcedH)
		if b.style.flexDirection == "column" {
			contentH = e.layoutFlexColumn(b, contentLeft, contentTop, b.contentW, eh)
		} else {
			contentH = e.layoutFlexRow(b, contentLeft, contentTop, b.contentW, eh)
		}
	} else {
		contentH = e.layoutBlockChildren(b, contentLeft, contentTop, b.contentW)
	}

	if forcedH >= 0 {
		b.h = forcedH
		b.contentH = forcedH - b.pt - b.pb - b.bt - b.bb
		if b.contentH < 0 {
			b.contentH = 0
		}
		return
	}
	if b.disp != "flex" && !b.style.height.isAuto() && b.style.height.u == unitPx {
		if b.style.boxSizing == "border-box" {
			contentH = b.style.height.v - b.pt - b.pb - b.bt - b.bb
		} else {
			contentH = b.style.height.v
		}
		if contentH < 0 {
			contentH = 0
		}
	}
	b.contentH = contentH
	b.h = contentH + b.pt + b.pb + b.bt + b.bb
}

func explicitContentHeight(b *box, forcedH float64) float64 {
	if forcedH >= 0 {
		v := forcedH - b.pt - b.pb - b.bt - b.bb
		if v < 0 {
			v = 0
		}
		return v
	}
	if !b.style.height.isAuto() && b.style.height.u == unitPx {
		var v float64
		if b.style.boxSizing == "border-box" {
			v = b.style.height.v - b.pt - b.pb - b.bt - b.bb
		} else {
			v = b.style.height.v
		}
		if v < 0 {
			v = 0
		}
		return v
	}
	return -1
}

func (e *engine) layoutBlockChildren(parent *box, contentX, contentY, contentW float64) float64 {
	cursorY := contentY
	children := parent.children
	i := 0
	for i < len(children) {
		c := children[i]
		if isInlineLevel(c) {
			j := i
			var group []*box
			for j < len(children) && isInlineLevel(children[j]) {
				group = append(group, children[j])
				j++
			}
			runs, h := e.layoutInline(parent, group, contentX, cursorY, contentW)
			parent.runs = append(parent.runs, runs...)
			cursorY += h
			i = j
			continue
		}
		bw := e.computeBlockWidth(c, contentW)
		x := contentX + c.ml
		y := cursorY + c.mt
		e.layoutBlock(c, x, y, bw, -1)
		cursorY += c.mt + c.h + c.mb
		i++
	}
	return cursorY - contentY
}

func isInlineLevel(b *box) bool {
	return b.disp == "text" || b.disp == "inline" || b.disp == "break"
}

type inlineFrag struct {
	text    string
	style   *computed
	isBreak bool
}

func flattenInline(boxes []*box, out *[]inlineFrag) {
	for _, c := range boxes {
		switch c.disp {
		case "text":
			*out = append(*out, inlineFrag{text: c.text, style: c.style})
		case "break":
			*out = append(*out, inlineFrag{isBreak: true, style: c.style})
		case "inline":
			flattenInline(c.children, out)
		default:
			*out = append(*out, inlineFrag{isBreak: true, style: c.style})
		}
	}
}

type inlineToken struct {
	word    string
	style   *computed
	face    *canvas.FontFace
	adv     float64
	isBreak bool
}

func (e *engine) layoutInline(block *box, group []*box, originX, originY, width float64) ([]textRun, float64) {
	var frags []inlineFrag
	flattenInline(group, &frags)

	var tokens []inlineToken
	for _, f := range frags {
		if f.isBreak {
			tokens = append(tokens, inlineToken{isBreak: true, style: f.style})
			continue
		}
		s := strings.Join(strings.Fields(f.text), " ")
		if s == "" {
			continue
		}
		face := getFace(f.style.fontKey(), f.style.fontSize)
		for _, w := range strings.Split(s, " ") {
			tokens = append(tokens, inlineToken{
				word:  w,
				style: f.style,
				face:  face,
				adv:   measureText(face, w),
			})
		}
	}

	var runs []textRun
	align := block.style.textAlign

	type placed struct {
		tok inlineToken
		x   float64
	}
	var line []placed
	var curX, lineAscent, lineHeight, yCursor float64

	flush := func() {
		shift := 0.0
		switch align {
		case "center":
			shift = (width - curX) / 2
		case "right":
			shift = width - curX
		}
		if shift < 0 {
			shift = 0
		}
		base := originY + yCursor + lineAscent
		for _, p := range line {
			runs = append(runs, textRun{
				text:     p.tok.word,
				x:        originX + shift + p.x,
				baseline: base,
				col:      p.tok.style.color,
				fontKey:  p.tok.style.fontKey(),
				px:       p.tok.style.fontSize,
			})
		}
		yCursor += lineHeight
		line = nil
		curX = 0
		lineAscent = 0
		lineHeight = 0
	}

	for _, tok := range tokens {
		if tok.isBreak {
			if len(line) == 0 {
				lineHeight = tok.style.lineHeight.px(tok.style.fontSize)
				lineAscent = faceAscent(getFace(tok.style.fontKey(), tok.style.fontSize))
			}
			flush()
			continue
		}
		sp := 0.0
		if len(line) > 0 {
			sp = measureText(tok.face, " ")
		}
		if len(line) > 0 && curX+sp+tok.adv > width {
			flush()
			sp = 0
		}
		x := curX + sp
		line = append(line, placed{tok: tok, x: x})
		curX = x + tok.adv
		if lh := tok.style.lineHeight.px(tok.style.fontSize); lh > lineHeight {
			lineHeight = lh
		}
		if asc := faceAscent(tok.face); asc > lineAscent {
			lineAscent = asc
		}
	}
	if len(line) > 0 {
		flush()
	}
	return runs, yCursor
}

// maxContentWidth returns the natural (shrink-to-fit) border-box width of b and
// its subtree, ignoring any available-width constraint. It resolves the
// flex-basis of an auto-width flex item, exactly as CSS resolves an auto basis
// to the item's max-content size; without it such an item collapses to width 0
// and its text wraps one word per line.
func (e *engine) maxContentWidth(b *box) float64 {
	if b.disp == "none" {
		return 0
	}
	resolveEdges(b, 0)
	s := b.style
	if !s.width.isAuto() {
		w := s.width.resolve(0, 0)
		var bbw float64
		if s.boxSizing == "border-box" {
			bbw = w
		} else {
			bbw = w + extra(b)
		}
		return clampWidth(b, bbw, 0)
	}
	return clampWidth(b, e.contentMaxWidth(b)+extra(b), 0)
}

// contentMaxWidth is the natural width of b's content box (inside padding and
// border): a flex row sums its items, a flex column and block flow take the
// widest child, and an inline run is its widest break-delimited line.
func (e *engine) contentMaxWidth(b *box) float64 {
	if b.disp == "flex" {
		items := flexItems(b)
		if b.style.flexDirection == "column" {
			max := 0.0
			for _, it := range items {
				resolveEdges(it, 0)
				if w := e.maxContentWidth(it) + it.ml + it.mr; w > max {
					max = w
				}
			}
			return max
		}
		total := 0.0
		for _, it := range items {
			resolveEdges(it, 0)
			total += e.maxContentWidth(it) + it.ml + it.mr
		}
		if len(items) > 1 {
			total += b.style.gap * float64(len(items)-1)
		}
		return total
	}
	max := 0.0
	children := b.children
	i := 0
	for i < len(children) {
		c := children[i]
		if isInlineLevel(c) {
			j := i
			var group []*box
			for j < len(children) && isInlineLevel(children[j]) {
				group = append(group, children[j])
				j++
			}
			if w := e.inlineMaxWidth(group); w > max {
				max = w
			}
			i = j
			continue
		}
		resolveEdges(c, 0)
		if w := e.maxContentWidth(c) + c.ml + c.mr; w > max {
			max = w
		}
		i++
	}
	return max
}

// inlineMaxWidth is the widest single line an inline group produces when it is
// never wrapped — the widest run of words between forced breaks, mirroring the
// tokenization in layoutInline so the measured width matches what gets painted.
func (e *engine) inlineMaxWidth(group []*box) float64 {
	var frags []inlineFrag
	flattenInline(group, &frags)
	max, cur := 0.0, 0.0
	started := false
	for _, f := range frags {
		if f.isBreak {
			if cur > max {
				max = cur
			}
			cur, started = 0, false
			continue
		}
		s := strings.Join(strings.Fields(f.text), " ")
		if s == "" {
			continue
		}
		face := getFace(f.style.fontKey(), f.style.fontSize)
		for _, w := range strings.Split(s, " ") {
			if started {
				cur += measureText(face, " ")
			}
			cur += measureText(face, w)
			started = true
		}
	}
	if cur > max {
		max = cur
	}
	return max
}

func flexItems(b *box) []*box {
	var items []*box
	for _, c := range b.children {
		if c.disp == "text" || c.disp == "break" {
			continue
		}
		items = append(items, c)
	}
	return items
}

func justifyDistribute(justify string, leftover float64, n int) (start, between float64) {
	if leftover < 0 {
		leftover = 0
	}
	switch justify {
	case "center":
		return leftover / 2, 0
	case "flex-end":
		return leftover, 0
	case "space-between":
		if n > 1 {
			return 0, leftover / float64(n-1)
		}
		return 0, 0
	case "space-around":
		per := leftover / float64(n)
		return per / 2, per
	default:
		return 0, 0
	}
}

func (e *engine) layoutFlexRow(b *box, cx, cy, cw, explicitH float64) float64 {
	items := flexItems(b)
	n := len(items)
	if n == 0 {
		if explicitH >= 0 {
			return explicitH
		}
		return 0
	}
	gap := b.style.gap
	totalGap := gap * float64(n-1)

	base := make([]float64, n)
	mainMargin := make([]float64, n)
	var sumBase, sumGrow, sumMargin float64
	for i, it := range items {
		resolveEdges(it, cw)
		if !it.style.width.isAuto() {
			w := it.style.width.resolve(cw, 0)
			if it.style.boxSizing == "border-box" {
				base[i] = w
			} else {
				base[i] = w + extra(it)
			}
		} else {
			base[i] = e.maxContentWidth(it)
			resolveEdges(it, cw)
		}
		base[i] = clampWidth(it, base[i], cw)
		mainMargin[i] = it.ml + it.mr
		sumBase += base[i]
		sumMargin += mainMargin[i]
		sumGrow += it.style.flexGrow
	}

	free := cw - sumBase - sumMargin - totalGap
	mains := make([]float64, n)
	copy(mains, base)
	if free > 0 && sumGrow > 0 {
		for i, it := range items {
			mains[i] = base[i] + free*it.style.flexGrow/sumGrow
		}
	}

	used := totalGap + sumMargin
	for _, m := range mains {
		used += m
	}
	leftover := cw - used
	start, between := justifyDistribute(b.style.justifyContent, leftover, n)

	naturalCross := make([]float64, n)
	maxCross := 0.0
	for i, it := range items {
		e.layoutBlock(it, cx, cy, mains[i], -1)
		naturalCross[i] = it.h
		if it.h > maxCross {
			maxCross = it.h
		}
	}
	crossContainer := maxCross
	if explicitH >= 0 {
		crossContainer = explicitH
	}

	mainPos := cx + start
	for i, it := range items {
		forced := -1.0
		crossSize := naturalCross[i]
		if b.style.alignItems == "stretch" && it.style.height.isAuto() {
			crossSize = crossContainer
			forced = crossContainer
		}
		var yy float64
		switch b.style.alignItems {
		case "center":
			yy = cy + (crossContainer-crossSize)/2
		case "flex-end":
			yy = cy + (crossContainer - crossSize)
		default:
			yy = cy
		}
		e.layoutBlock(it, mainPos+it.ml, yy+it.mt, mains[i], forced)
		mainPos += it.ml + mains[i] + it.mr + gap + between
	}

	if explicitH >= 0 && explicitH > crossContainer {
		return explicitH
	}
	return crossContainer
}

func (e *engine) layoutFlexColumn(b *box, cx, cy, cw, explicitH float64) float64 {
	items := flexItems(b)
	n := len(items)
	if n == 0 {
		if explicitH >= 0 {
			return explicitH
		}
		return 0
	}
	gap := b.style.gap
	totalGap := gap * float64(n-1)

	base := make([]float64, n)
	crossSizes := make([]float64, n)
	crossMargin := make([]float64, n)
	var sumBase, sumGrow, sumMainMargin float64
	for i, it := range items {
		resolveEdges(it, cw)
		crossMargin[i] = it.ml + it.mr
		cross := cw - crossMargin[i]
		if !it.style.width.isAuto() {
			w := it.style.width.resolve(cw, 0)
			if it.style.boxSizing == "border-box" {
				cross = w
			} else {
				cross = w + extra(it)
			}
		} else if b.style.alignItems != "stretch" {
			cross = e.maxContentWidth(it)
			resolveEdges(it, cw)
		}
		crossSizes[i] = clampWidth(it, cross, cw)
		e.layoutBlock(it, cx, cy, crossSizes[i], -1)
		if !it.style.height.isAuto() && it.style.height.u == unitPx {
			base[i] = it.h
		} else {
			base[i] = it.h
		}
		sumBase += base[i]
		sumMainMargin += it.mt + it.mb
		sumGrow += it.style.flexGrow
	}

	mainContainer := explicitH
	if explicitH < 0 {
		mainContainer = sumBase + totalGap + sumMainMargin
	}
	free := mainContainer - sumBase - totalGap - sumMainMargin
	mains := make([]float64, n)
	copy(mains, base)
	if free > 0 && sumGrow > 0 {
		for i, it := range items {
			mains[i] = base[i] + free*it.style.flexGrow/sumGrow
		}
		free = 0
	}

	used := totalGap + sumMainMargin
	for _, m := range mains {
		used += m
	}
	leftover := mainContainer - used
	start, between := justifyDistribute(b.style.justifyContent, leftover, n)

	mainPos := cy + start
	for i, it := range items {
		var xx float64
		switch b.style.alignItems {
		case "center":
			xx = cx + (cw-crossMargin[i]-crossSizes[i])/2
		case "flex-end":
			xx = cx + (cw - crossMargin[i] - crossSizes[i])
		default:
			xx = cx
		}
		e.layoutBlock(it, xx+it.ml, mainPos+it.mt, crossSizes[i], mains[i])
		mainPos += it.mt + mains[i] + it.mb + gap + between
	}

	if explicitH >= 0 {
		return explicitH
	}
	return mainContainer
}
