package diff

// Align matches a against b along a longest common subsequence: for each
// element of a it returns the index of the element of b it is paired with,
// or -1 when it is not part of the subsequence. Pairs are strictly
// increasing in both a and b. It runs Myers' O((N+M)D) algorithm in linear
// space — splitting each problem at the middle of its shortest edit path
// (the bisection diff-match-patch uses) — so long inputs with many
// differences need no frontier history and no budget.
func Align[T comparable](a, b []T) []int {
	match := make([]int, len(a))
	for i := range match {
		match[i] = -1
	}
	align(a, b, 0, 0, match)
	return match
}

func align[T comparable](a, b []T, aOff, bOff int, match []int) {
	for {
		n := 0
		for n < len(a) && n < len(b) && a[n] == b[n] {
			match[aOff+n] = bOff + n
			n++
		}
		a, b, aOff, bOff = a[n:], b[n:], aOff+n, bOff+n
		s := 0
		for s < len(a) && s < len(b) && a[len(a)-1-s] == b[len(b)-1-s] {
			match[aOff+len(a)-1-s] = bOff + len(b) - 1 - s
			s++
		}
		a, b = a[:len(a)-s], b[:len(b)-s]
		if len(a) == 0 || len(b) == 0 {
			return
		}
		x, y, ok := bisect(a, b)
		if !ok || (x == 0 && y == 0) || (x == len(a) && y == len(b)) {
			return
		}
		align(a[:x], b[:y], aOff, bOff, match)
		a, b, aOff, bOff = a[x:], b[y:], aOff+x, bOff+y
	}
}

// bisect finds where the forward and reverse searches for a shortest edit
// path from a to b meet, the point both halves are split at. ok is false
// only when a and b share nothing at all.
func bisect[T comparable](a, b []T) (x, y int, ok bool) {
	n, m := len(a), len(b)
	maxD := (n + m + 1) / 2
	off := maxD
	size := 2*maxD + 2
	v1 := make([]int, size)
	v2 := make([]int, size)
	for i := range v1 {
		v1[i], v2[i] = -1, -1
	}
	v1[off+1], v2[off+1] = 0, 0
	delta := n - m
	front := delta%2 != 0
	k1start, k1end, k2start, k2end := 0, 0, 0, 0
	for d := 0; d < maxD; d++ {
		for k1 := -d + k1start; k1 <= d-k1end; k1 += 2 {
			i := off + k1
			var x1 int
			if k1 == -d || (k1 != d && v1[i-1] < v1[i+1]) {
				x1 = v1[i+1]
			} else {
				x1 = v1[i-1] + 1
			}
			y1 := x1 - k1
			for x1 < n && y1 < m && a[x1] == b[y1] {
				x1++
				y1++
			}
			v1[i] = x1
			switch {
			case x1 > n:
				k1end += 2
			case y1 > m:
				k1start += 2
			case front:
				j := off + delta - k1
				if j >= 0 && j < size && v2[j] != -1 && x1 >= n-v2[j] {
					return x1, y1, true
				}
			}
		}
		for k2 := -d + k2start; k2 <= d-k2end; k2 += 2 {
			i := off + k2
			var x2 int
			if k2 == -d || (k2 != d && v2[i-1] < v2[i+1]) {
				x2 = v2[i+1]
			} else {
				x2 = v2[i-1] + 1
			}
			y2 := x2 - k2
			for x2 < n && y2 < m && a[n-x2-1] == b[m-y2-1] {
				x2++
				y2++
			}
			v2[i] = x2
			switch {
			case x2 > n:
				k2end += 2
			case y2 > m:
				k2start += 2
			case !front:
				j := off + delta - k2
				if j >= 0 && j < size && v1[j] != -1 {
					x1 := v1[j]
					y1 := off + x1 - j
					if x1 >= n-x2 {
						return x1, y1, true
					}
				}
			}
		}
	}
	return 0, 0, false
}
