// Package layout turns a pane split sequence into a tree, computes pane
// geometry and renders tmux layout strings. It also provides the inverse used
// by capture. Everything here is pure: no tmux, no I/O.
package layout

import (
	"fmt"

	"github.com/mrqwer/cairn/internal/workspace"
)

// Node is a binary split tree. A leaf has Split == "" and Leaf is the 1-based
// pane position. A split has Split "h" (left/right) or "v" (top/bottom); Ratio
// is the percent of the area given to B (the new pane), A is the existing one.
type Node struct {
	Leaf  int
	Split string
	Ratio int
	A, B  *Node
}

func (n *Node) isLeaf() bool { return n.Split == "" }

// Build folds the pane split sequence into a tree. Pane 1 is the root leaf;
// each later pane replaces the leaf it was split from. From 0 means the
// previous pane and Size 0 means workspace.DefaultSize.
func Build(panes []workspace.Pane) (*Node, error) {
	if len(panes) == 0 {
		return nil, fmt.Errorf("layout: no panes")
	}
	root := &Node{Leaf: 1}
	leaves := map[int]*Node{1: root}
	for i := 1; i < len(panes); i++ {
		pos := i + 1
		p := panes[i]
		if p.Split != workspace.SplitH && p.Split != workspace.SplitV {
			return nil, fmt.Errorf("layout: pane %d: split %q must be h or v", pos, p.Split)
		}
		from := p.From
		if from == 0 {
			from = pos - 1
		}
		if from < 1 || from >= pos {
			return nil, fmt.Errorf("layout: pane %d: from %d must be between 1 and %d", pos, from, pos-1)
		}
		size := p.Size
		if size == 0 {
			size = workspace.DefaultSize
		}
		if size < 5 || size > 95 {
			return nil, fmt.Errorf("layout: pane %d: size %d must be between 5 and 95", pos, size)
		}
		target := leaves[from]
		old := *target
		*target = Node{Split: p.Split, Ratio: size, A: &old, B: &Node{Leaf: pos}}
		leaves[from] = target.A
		leaves[pos] = target.B
	}
	return root, nil
}
