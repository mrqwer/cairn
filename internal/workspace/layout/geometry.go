package layout

import "sort"

// Rect is the cell rectangle of one pane (Pane is its 1-based position).
type Rect struct{ Pane, X, Y, W, H int }

// Geometry lays the tree out in a w x h area. Siblings are separated by one
// border cell, so for a split wA + 1 + wB equals the parent size. Rects are
// returned in pane-position order.
func Geometry(n *Node, w, h int) []Rect {
	var out []Rect
	walk(n, 0, 0, w, h, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Pane < out[j].Pane })
	return out
}

type area struct{ x, y, w, h int }

// children returns the areas of A and B inside a.
func children(n *Node, a area) (area, area) {
	if n.Split == "h" {
		wa, wb := divide(a.w, n.Ratio)
		return area{a.x, a.y, wa, a.h}, area{a.x + wa + 1, a.y, wb, a.h}
	}
	ha, hb := divide(a.h, n.Ratio)
	return area{a.x, a.y, a.w, ha}, area{a.x, a.y + ha + 1, a.w, hb}
}

// divide splits size minus one border cell into (a, b) with b = ratio percent.
func divide(size, ratio int) (a, b int) {
	total := size - 1
	b = max(1, total*ratio/100)
	if total >= 2 {
		b = min(b, total-1)
	}
	return total - b, b
}

func walk(n *Node, x, y, w, h int, out *[]Rect) {
	if n.isLeaf() {
		*out = append(*out, Rect{n.Leaf, x, y, w, h})
		return
	}
	a, b := children(n, area{x, y, w, h})
	walk(n.A, a.x, a.y, a.w, a.h, out)
	walk(n.B, b.x, b.y, b.w, b.h, out)
}
