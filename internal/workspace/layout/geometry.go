package layout

import (
	"fmt"
	"slices"
	"sort"
)

// Rect is the cell rectangle of one pane (Pane is its 1-based position).
type Rect struct{ Pane, X, Y, W, H int }

// Order returns the leaf pane positions (1-based, config order) in traversal
// order: in-order, A before B. This is the order the window's panes must have
// in tmux before select-layout, since tmux assigns panes to the cells of a
// layout string in traversal order and ignores the leaf numbers.
func Order(n *Node) []int {
	if n.isLeaf() {
		return []int{n.Leaf}
	}
	return append(Order(n.A), Order(n.B)...)
}

func minLeaf(n *Node) int {
	return slices.Min(Order(n))
}

// Check reports an error naming the pane position when the tree cannot be laid
// out in w x h: a split with fewer than two content cells to share, or a leaf
// smaller than 1x1.
func Check(n *Node, w, h int) error {
	if w < 1 || h < 1 {
		return fmt.Errorf("layout: pane %d: window %dx%d is too small", minLeaf(n), w, h)
	}
	return check(n, area{0, 0, w, h})
}

func check(n *Node, a area) error {
	if n.isLeaf() {
		if a.w < 1 || a.h < 1 {
			return fmt.Errorf("layout: pane %d would be %dx%d", n.Leaf, a.w, a.h)
		}
		return nil
	}
	size := a.w
	if n.Split == "v" {
		size = a.h
	}
	if size-1 < 2 {
		return fmt.Errorf("layout: pane %d: no room to split a %dx%d area", minLeaf(n.B), a.w, a.h)
	}
	ca, cb := children(n, a)
	if err := check(n.A, ca); err != nil {
		return err
	}
	return check(n.B, cb)
}

// Geometry lays the tree out in a w x h area. Siblings are separated by one
// border cell, so for a split wA + 1 + wB equals the parent size. Rects are
// returned in pane-position order (Rect.Pane), not traversal order. It never
// panics but is only meaningful after Check(n, w, h) has passed.
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

// Size is a window size in cells.
type Size struct{ W, H int }
