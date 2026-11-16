package layout

import "github.com/mrqwer/cairn/internal/workspace"

// firstLeaf is the in-order-first leaf of n.
func firstLeaf(n *Node) int {
	for !n.isLeaf() {
		n = n.A
	}
	return n.Leaf
}

// SequenceLeaves returns the leaf numbers of n in the creation order Sequence
// emits them: element i is the leaf that becomes config pane i+1. Capture
// uses it to map config panes back to the tmux panes they were read from.
func SequenceLeaves(n *Node) []int {
	out := []int{firstLeaf(n)}
	var walk func(m *Node)
	walk = func(m *Node) {
		if m.isLeaf() {
			return
		}
		out = append(out, firstLeaf(m.B))
		walk(m.A)
		walk(m.B)
	}
	walk(n)
	return out
}

// Sequence converts a split tree into the config pane list (creation order)
// that Build folds back into the same tree, decision E8. The leaves of n are
// typically traversal positions, as returned by FromRects. Emission is
// top-down: the first pane is the in-order-first leaf of the whole tree; a
// split (dir, A, B) creates the in-order-first leaf of B, split from the
// in-order-first leaf of A with the node's ratio as size; then A and B are
// emitted recursively. Only Split, From and Size are set.
func Sequence(n *Node) []workspace.Pane {
	leaves := SequenceLeaves(n)
	pos := make(map[int]int, len(leaves))
	for i, l := range leaves {
		pos[l] = i + 1
	}
	panes := make([]workspace.Pane, len(leaves))
	var walk func(m *Node)
	walk = func(m *Node) {
		if m.isLeaf() {
			return
		}
		panes[pos[firstLeaf(m.B)]-1] = workspace.Pane{Split: m.Split, From: pos[firstLeaf(m.A)], Size: m.Ratio}
		walk(m.A)
		walk(m.B)
	}
	walk(n)
	return panes
}
