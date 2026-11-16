package layout

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/mrqwer/cairn/internal/workspace"
)

// renumber gives the leaves of n their traversal positions (1-based), as
// FromRects numbers them.
func renumber(n *Node) *Node {
	k := 0
	var walk func(m *Node) *Node
	walk = func(m *Node) *Node {
		if m.isLeaf() {
			k++
			return &Node{Leaf: k}
		}
		return &Node{Split: m.Split, Ratio: m.Ratio, A: walk(m.A), B: walk(m.B)}
	}
	return walk(n)
}

func TestSequenceAsymmetric(t *testing.T) {
	// Traversal numbering of v(h(1,2),3): pane 3 (bottom) was split from
	// pane 1 before pane 2 (top right) was.
	tree := &Node{Split: "v", Ratio: 40,
		A: &Node{Split: "h", Ratio: 30, A: &Node{Leaf: 1}, B: &Node{Leaf: 2}},
		B: &Node{Leaf: 3}}
	want := []workspace.Pane{{}, {Split: "v", From: 1, Size: 40}, {Split: "h", From: 1, Size: 30}}
	if got := Sequence(tree); !reflect.DeepEqual(got, want) {
		t.Fatalf("Sequence = %+v, want %+v", got, want)
	}
	if got := SequenceLeaves(tree); !reflect.DeepEqual(got, []int{1, 3, 2}) {
		t.Fatalf("SequenceLeaves = %v, want [1 3 2]", got)
	}
	if got := Sequence(&Node{Leaf: 1}); !reflect.DeepEqual(got, []workspace.Pane{{}}) {
		t.Fatalf("single leaf: %+v", got)
	}
}

func TestSequenceFromNonFirstLeaf(t *testing.T) {
	// h(1, v(h(2,3), 4)): pane 4 splits from pane 2, pane 3 from pane 2.
	tree := &Node{Split: "h", Ratio: 60, A: &Node{Leaf: 1},
		B: &Node{Split: "v", Ratio: 50,
			A: &Node{Split: "h", Ratio: 25, A: &Node{Leaf: 2}, B: &Node{Leaf: 3}},
			B: &Node{Leaf: 4}}}
	want := []workspace.Pane{{}, {Split: "h", From: 1, Size: 60}, {Split: "v", From: 2, Size: 50}, {Split: "h", From: 2, Size: 25}}
	if got := Sequence(tree); !reflect.DeepEqual(got, want) {
		t.Fatalf("Sequence = %+v, want %+v", got, want)
	}
	if got := SequenceLeaves(tree); !reflect.DeepEqual(got, []int{1, 2, 4, 3}) {
		t.Fatalf("SequenceLeaves = %v", got)
	}
}

func sortedRects(rs []Rect) [][4]int {
	out := make([][4]int, len(rs))
	for i, r := range rs {
		out[i] = [4]int{r.X, r.Y, r.W, r.H}
	}
	sort.Slice(out, func(i, j int) bool {
		for k := 0; k < 4; k++ {
			if out[i][k] != out[j][k] {
				return out[i][k] < out[j][k]
			}
		}
		return false
	})
	return out
}

func TestSequenceRebuildsTreeRandom(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	checked := 0
	for iter := 0; iter < 400; iter++ {
		n := 1 + r.Intn(9)
		w, h := 200+r.Intn(100), 60+r.Intn(60)
		built, err := Build(randomPanes(r, n))
		if err != nil {
			t.Fatal(err)
		}
		tree := renumber(built)
		if !feasible(tree, area{0, 0, w, h}) {
			continue
		}
		checked++
		seq := Sequence(tree)
		leaves := SequenceLeaves(tree)
		if len(seq) != n || len(leaves) != n {
			t.Fatalf("iter %d: %d panes, %d leaves for %d", iter, len(seq), len(leaves), n)
		}
		for i, p := range seq {
			if i == 0 {
				if !reflect.DeepEqual(p, workspace.Pane{}) {
					t.Fatalf("iter %d: first pane %+v", iter, p)
				}
				continue
			}
			if p.From < 1 || p.From > i || p.Size < 5 || p.Size > 95 {
				t.Fatalf("iter %d: pane %d invalid %+v", iter, i+1, p)
			}
		}
		back, err := Build(seq)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		want := Geometry(tree, w, h)
		got := Geometry(back, w, h)
		if !reflect.DeepEqual(sortedRects(got), sortedRects(want)) {
			t.Fatalf("iter %d: rect multiset differs\n got %v\nwant %v", iter, got, want)
		}
		// Config position i is the tree's leaf leaves[i-1], exactly.
		for i, g := range got {
			wr := want[leaves[i]-1]
			if g.X != wr.X || g.Y != wr.Y || g.W != wr.W || g.H != wr.H {
				t.Fatalf("iter %d: config pane %d at %v, leaf %d at %v", iter, i+1, g, leaves[i], wr)
			}
		}
		// Traversal order of the rebuilt tree, mapped back to tree leaves,
		// is the traversal order of the tree.
		var mapped []int
		for _, pos := range Order(back) {
			mapped = append(mapped, leaves[pos-1])
		}
		if !reflect.DeepEqual(mapped, Order(tree)) {
			t.Fatalf("iter %d: order %v (config %v), want %v", iter, mapped, Order(back), Order(tree))
		}
	}
	if checked < 200 {
		t.Fatalf("only %d trees checked", checked)
	}
}
