package layout

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/workspace"
)

func threePanes() []workspace.Pane {
	return []workspace.Pane{
		{},
		{Split: "h", From: 1, Size: 40},
		{Split: "v", From: 2, Size: 50},
	}
}

func TestChecksum(t *testing.T) {
	body := "200x50,0,0{100x50,0,0,0,99x50,101,0[99x25,101,0,1,99x24,101,26,2]}"
	if got := Checksum(body); got != "21be" {
		t.Fatalf("checksum = %s, want 21be", got)
	}
}

func TestBuildAndGeometry(t *testing.T) {
	n, err := Build(threePanes())
	if err != nil {
		t.Fatal(err)
	}
	want := []Rect{{1, 0, 0, 120, 50}, {2, 121, 0, 79, 25}, {3, 121, 26, 79, 24}}
	got := Geometry(n, 200, 50)
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rect %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestTmuxStringNested(t *testing.T) {
	n, _ := Build(threePanes())
	body := "200x50,0,0{120x50,0,0,0,79x50,121,0[79x25,121,0,1,79x24,121,26,2]}"
	want := Checksum(body) + "," + body
	if got := TmuxString(n, 200, 50); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestTmuxStringFlattensSameDirection(t *testing.T) {
	n, err := Build([]workspace.Pane{{}, {Split: "h", Size: 50}, {Split: "h", From: 2, Size: 50}})
	if err != nil {
		t.Fatal(err)
	}
	got := TmuxString(n, 100, 20)
	body := got[strings.Index(got, ",")+1:]
	if strings.Count(body, "{") != 1 {
		t.Fatalf("not flattened: %s", body)
	}
	if body != "100x20,0,0{50x20,0,0,0,24x20,51,0,1,24x20,76,0,2}" {
		t.Fatalf("body = %s", body)
	}
}

func assertSame(t *testing.T, got, want []Rect, tol int) {
	t.Helper()
	assertSameTol(t, got, want, tol, tol)
}

// assertSameTol allows tolPos on X/Y and tolSize on W/H.
func assertSameTol(t *testing.T, got, want []Rect, tolPos, tolSize int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	abs := func(x int) int { return max(x, -x) }
	for i := range want {
		g, w := got[i], want[i]
		if g.Pane != w.Pane || abs(g.X-w.X) > tolPos || abs(g.Y-w.Y) > tolPos || abs(g.W-w.W) > tolSize || abs(g.H-w.H) > tolSize {
			t.Errorf("rect %d = %v, want %v (pos tol %d, size tol %d)", i, g, w, tolPos, tolSize)
		}
	}
}

func TestFromRectsInverse(t *testing.T) {
	n, _ := Build(threePanes())
	rects := Geometry(n, 200, 50)
	back, err := FromRects(rects, 200, 50)
	if err != nil {
		t.Fatal(err)
	}
	assertSame(t, Geometry(back, 200, 50), rects, 0)
}

func TestFromRectsNoCleanSplit(t *testing.T) {
	pin := []Rect{{1, 0, 0, 60, 10}, {2, 61, 0, 39, 20}, {3, 40, 21, 60, 9}, {4, 0, 11, 39, 19}}
	if _, err := FromRects(pin, 100, 30); err == nil || err.Error() != "layout: no clean split" {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildErrors(t *testing.T) {
	cases := map[string][]workspace.Pane{
		"size low":      {{}, {Split: "h", Size: 4}},
		"size high":     {{}, {Split: "h", Size: 96}},
		"from zero-neg": {{}, {Split: "h", From: -1}},
		"from too big":  {{}, {Split: "h", From: 2}},
		"bad split":     {{}, {Split: "x"}},
		"no split":      {{}, {}},
		"empty":         {},
	}
	for name, ps := range cases {
		if _, err := Build(ps); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func randomPanes(r *rand.Rand, n int) []workspace.Pane {
	ps := make([]workspace.Pane, n)
	for i := 1; i < n; i++ {
		ps[i] = workspace.Pane{Split: []string{"h", "v"}[r.Intn(2)], From: 1 + r.Intn(i), Size: 5 + r.Intn(91)}
	}
	return ps
}

// feasible reports whether every split has room for two non-empty children.
func feasible(n *Node, a area) bool {
	if n.isLeaf() {
		return true
	}
	size := a.w
	if n.Split == "v" {
		size = a.h
	}
	if size-1 < 2 {
		return false
	}
	ca, cb := children(n, a)
	return feasible(n.A, ca) && feasible(n.B, cb)
}

func TestGeometryTilesExactly(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	checked := 0
	defer func() {
		if checked < 150 {
			t.Errorf("only %d feasible trees checked", checked)
		}
	}()
	for iter := 0; iter < 300; iter++ {
		n := 1 + r.Intn(8)
		w, h := 200+r.Intn(100), 60+r.Intn(60)
		tree, err := Build(randomPanes(r, n))
		if err != nil {
			t.Fatal(err)
		}
		if !feasible(tree, area{0, 0, w, h}) {
			continue
		}
		checked++
		rects := Geometry(tree, w, h)
		if len(rects) != n {
			t.Fatalf("iter %d: %d rects for %d panes", iter, len(rects), n)
		}
		// Every cell is covered by exactly one pane or is a border cell;
		// border cells lie between panes, so pane cells + border cells = w*h.
		grid := make([]int, w*h)
		for _, rc := range rects {
			if rc.W < 1 || rc.H < 1 || rc.X < 0 || rc.Y < 0 || rc.X+rc.W > w || rc.Y+rc.H > h {
				t.Fatalf("iter %d: bad rect %v in %dx%d", iter, rc, w, h)
			}
			for y := rc.Y; y < rc.Y+rc.H; y++ {
				for x := rc.X; x < rc.X+rc.W; x++ {
					if grid[y*w+x] != 0 {
						t.Fatalf("iter %d: overlap at %d,%d", iter, x, y)
					}
					grid[y*w+x] = rc.Pane
				}
			}
		}
		checkTiles(t, tree, area{0, 0, w, h})
	}
}

// checkTiles asserts wA+1+wB (resp. height) equals the node size at every split.
func checkTiles(t *testing.T, n *Node, a area) {
	t.Helper()
	if n.isLeaf() {
		return
	}
	ca, cb := children(n, a)
	if n.Split == "h" {
		if ca.w+1+cb.w != a.w || ca.h != a.h || cb.h != a.h || cb.x != ca.x+ca.w+1 {
			t.Fatalf("h split does not tile: %v -> %v %v", a, ca, cb)
		}
	} else if ca.h+1+cb.h != a.h || ca.w != a.w || cb.w != a.w || cb.y != ca.y+ca.h+1 {
		t.Fatalf("v split does not tile: %v -> %v %v", a, ca, cb)
	}
	if ca.w < 1 || ca.h < 1 || cb.w < 1 || cb.h < 1 {
		t.Fatalf("size < 1: %v %v", ca, cb)
	}
	checkTiles(t, n.A, ca)
	checkTiles(t, n.B, cb)
}

func TestFromRectsRoundTripRandom(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	checked := 0
	defer func() {
		if checked < 100 {
			t.Errorf("only %d round trips checked", checked)
		}
	}()
	for iter := 0; iter < 300; iter++ {
		w, h := 200+r.Intn(100), 60+r.Intn(60)
		tree, _ := Build(randomPanes(r, 1+r.Intn(6)))
		if !feasible(tree, area{0, 0, w, h}) {
			continue
		}
		rects := Geometry(tree, w, h)
		back, err := FromRects(rects, w, h)
		if err != nil {
			t.Fatalf("iter %d: %v (%v)", iter, err, rects)
		}
		if clamped(back) {
			continue // a 5..95 clamp cannot express this proportion
		}
		checked++
		// Positions are exact to one cell; a pane between two boundaries that
		// each round the other way can differ by two cells in size.
		assertSameTol(t, Geometry(back, w, h), rects, 1, 2)
	}
}

func clamped(n *Node) bool {
	if n.isLeaf() {
		return false
	}
	return n.Ratio <= 5 || n.Ratio >= 95 || clamped(n.A) || clamped(n.B)
}
