package layout

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Checksum is the tmux layout checksum of body, as four lowercase hex digits.
func Checksum(body string) string {
	var c uint16
	for i := 0; i < len(body); i++ {
		c = (c >> 1) + ((c & 1) << 15) + uint16(body[i])
	}
	return fmt.Sprintf("%04x", c)
}

// TmuxString renders the classic tmux layout string "<csum>,<body>" for the
// tree in a w x h window. Splits of the same direction nested inside each
// other are flattened into one child list, as tmux prints them.
func TmuxString(n *Node, w, h int) string {
	var b strings.Builder
	writeCell(&b, n, area{0, 0, w, h})
	body := b.String()
	return Checksum(body) + "," + body
}

func writeCell(b *strings.Builder, n *Node, a area) {
	fmt.Fprintf(b, "%dx%d,%d,%d", a.w, a.h, a.x, a.y)
	if n.isLeaf() {
		fmt.Fprintf(b, ",%d", n.Leaf-1)
		return
	}
	open, closer := "{", "}"
	if n.Split == "v" {
		open, closer = "[", "]"
	}
	type child struct {
		n *Node
		a area
	}
	var kids []child
	var collect func(m *Node, ma area)
	collect = func(m *Node, ma area) {
		if m.isLeaf() || m.Split != n.Split {
			kids = append(kids, child{m, ma})
			return
		}
		ca, cb := children(m, ma)
		collect(m.A, ca)
		collect(m.B, cb)
	}
	ca, cb := children(n, a)
	collect(n.A, ca)
	collect(n.B, cb)
	b.WriteString(open)
	for i, k := range kids {
		if i > 0 {
			b.WriteString(",")
		}
		writeCell(b, k.n, k.a)
	}
	b.WriteString(closer)
}

// ErrNoCleanSplit is returned by FromRects when the panes cannot be described
// by nested full-width/full-height cuts.
var ErrNoCleanSplit = errors.New("layout: no clean split")

// FromRects rebuilds a split tree from pane rects (the inverse of Geometry),
// used by capture. It looks for a vertical gap column (h split), else a
// horizontal gap row (v split), takes the leftmost/topmost one, and recurses.
// The ratio is round(100*wB/(wA+wB)) over content cells, clamped to 5..95.
func FromRects(rs []Rect, w, h int) (*Node, error) {
	if len(rs) == 0 {
		return nil, errors.New("layout: no panes")
	}
	for _, r := range rs {
		if r.W < 1 || r.H < 1 || r.X < 0 || r.Y < 0 || r.X+r.W > w || r.Y+r.H > h {
			return nil, fmt.Errorf("layout: pane %d does not fit %dx%d", r.Pane, w, h)
		}
	}
	sorted := append([]Rect(nil), rs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Pane < sorted[j].Pane })
	return fromRects(sorted, area{0, 0, w, h})
}

func fromRects(rs []Rect, a area) (*Node, error) {
	if len(rs) == 1 {
		return &Node{Leaf: rs[0].Pane}, nil
	}
	for _, dir := range []string{"h", "v"} {
		left, right, ok := cut(rs, dir)
		if !ok {
			continue
		}
		sa, sb := extent(left, dir), extent(right, dir)
		node := &Node{Split: dir, Ratio: ratioFor(a, dir, extentStart(right, dir), sa, sb)}
		areaA, areaB := children(node, a)
		var err error
		if node.A, err = fromRects(left, areaA); err != nil {
			return nil, err
		}
		if node.B, err = fromRects(right, areaB); err != nil {
			return nil, err
		}
		return node, nil
	}
	return nil, ErrNoCleanSplit
}

func lo(r Rect, dir string) int {
	if dir == "h" {
		return r.X
	}
	return r.Y
}

func size(r Rect, dir string) int {
	if dir == "h" {
		return r.W
	}
	return r.H
}

// extent is the bounding length of rs along dir.
func extent(rs []Rect, dir string) int {
	mn, mx := lo(rs[0], dir), lo(rs[0], dir)+size(rs[0], dir)
	for _, r := range rs[1:] {
		mn = min(mn, lo(r, dir))
		mx = max(mx, lo(r, dir)+size(r, dir))
	}
	return mx - mn
}

// cut finds the first gap (lowest position) along dir that separates rs into
// two non-empty groups with exactly one border cell between them.
func cut(rs []Rect, dir string) (a, b []Rect, ok bool) {
	var edges []int
	for _, r := range rs {
		edges = append(edges, lo(r, dir)+size(r, dir))
	}
	sort.Ints(edges)
	for _, e := range edges {
		var left, right []Rect
		clean := true
		for _, r := range rs {
			switch {
			case lo(r, dir)+size(r, dir) <= e:
				left = append(left, r)
			case lo(r, dir) >= e+1:
				right = append(right, r)
			default:
				clean = false
			}
		}
		if !clean || len(left) == 0 || len(right) == 0 {
			continue
		}
		if extentEnd(left, dir) == e && extentStart(right, dir) == e+1 {
			return left, right, true
		}
	}
	return nil, nil, false
}

func extentEnd(rs []Rect, dir string) int {
	mx := 0
	for _, r := range rs {
		mx = max(mx, lo(r, dir)+size(r, dir))
	}
	return mx
}

func extentStart(rs []Rect, dir string) int {
	mn := lo(rs[0], dir)
	for _, r := range rs[1:] {
		mn = min(mn, lo(r, dir))
	}
	return mn
}

// ratioFor picks the Ratio whose split of area a along dir puts the start of B
// closest to startB (the true position). The ratio round(100*sb/(sa+sb))
// clamped to 5..95 is the preferred value on ties. Fitting against the area
// the rebuilt tree actually has, rather than the original sizes, keeps the
// rounding error of nested splits from accumulating.
func ratioFor(a area, dir string, startB, sa, sb int) int {
	r0 := min(95, max(5, (200*sb+(sa+sb))/(2*(sa+sb))))
	origin, length := a.x, a.w
	if dir == "v" {
		origin, length = a.y, a.h
	}
	dist := func(r int) int {
		wa, _ := divide(length, r)
		d := origin + wa + 1 - startB
		return max(d, -d)
	}
	best := r0
	for r := 5; r <= 95; r++ {
		if d := dist(r); d < dist(best) || (d == dist(best) && abs(r-r0) < abs(best-r0)) {
			best = r
		}
	}
	return best
}

func abs(x int) int { return max(x, -x) }
